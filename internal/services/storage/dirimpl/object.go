package dirimpl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"unicode/utf8"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// objectInput addresses key in bucket in the backend. It returns an error
// wrapping storage.ErrInvalidKey when key breaks the key rules.
//
// The backend refuses keys that S3 and this service accept, such as one
// holding a tab, so it is given the key's hash rather than the key, and the
// key is checked against this service's own rules: 1 to 1024 bytes of
// UTF-8 whose last "/"-separated element is not "." and that does not
// clean to ".". The key index maps the hashes back to the keys.
func objectInput(bucket, key string) (storage.BaseObjectInput, error) {
	if len(key) < 1 || len(key) > 1024 || !utf8.ValidString(key) ||
		path.Base(key) == "." || path.Clean(key) == "." {
		return storage.BaseObjectInput{}, storage.ErrInvalidKey
	}
	return storage.BaseObjectInput{Bucket: &bucket, Key: keyHash(key)}, nil
}

// objectInfo returns the stored object info, with the ETag, system headers
// and part sizes split out of the trailer's metadata. size is the length of
// the object bytes.
func objectInfo(key string, tr *trailer, size int64) (*contracts.ObjectInfo, error) {
	meta := tr.Meta
	if meta == nil {
		meta = make(map[string]string)
	}

	parts, err := splitParts(meta)
	if err != nil {
		return nil, err
	}

	return &contracts.ObjectInfo{
		Key:          key,
		ETag:         splitETag(meta),
		Headers:      splitHeaders(meta),
		PartSizes:    parts,
		Size:         size,
		ContentType:  tr.ContentType,
		LastModified: tr.LastModified,
		Metadata:     meta,
	}, nil
}

// StatObject returns an object's metadata, with its ETag split out of the
// user metadata. The metadata is in the object's trailer, so this opens the
// object to read it. ctx must be pinned to bucket.
func (s *Service) StatObject(
	ctx context.Context,
	bucket, key string,
) (*contracts.ObjectInfo, error) {
	if _, err := requirePin(ctx, bucket); err != nil {
		return nil, err
	}
	return s.statObject(ctx, bucket, key)
}

// statObject is StatObject without the pin, for DeleteBucket, which holds
// the bucket's write lock instead.
func (s *Service) statObject(
	ctx context.Context,
	bucket, key string,
) (*contracts.ObjectInfo, error) {
	obj, err := s.openObject(ctx, "stat", bucket, key)
	if err != nil {
		return nil, err
	}
	if err = obj.Body.Close(); err != nil {
		return nil, fmt.Errorf("%s: stat %q: %w", s.Name(), key, err)
	}
	return &obj.ObjectInfo, nil
}

// GetObject opens an object for reading. The caller must close its Body. The
// info and body come from the same read, so they match even when the object
// is overwritten meanwhile. ctx must be pinned to bucket.
func (s *Service) GetObject(ctx context.Context, bucket, key string) (*contracts.Object, error) {
	if _, err := requirePin(ctx, bucket); err != nil {
		return nil, err
	}
	return s.openObject(ctx, "get", bucket, key)
}

// openObject opens an object and reads its trailer. A missing object is an
// error wrapping storage.ErrObjectNotFound. op names the operation in
// errors.
func (s *Service) openObject(
	ctx context.Context,
	op, bucket, key string,
) (*contracts.Object, error) {
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return nil, err
	}

	base, err := objectInput(bucket, key)
	if err != nil {
		return nil, fmt.Errorf("%s: %s %q: %w", s.Name(), op, key, err)
	}

	obj, err := s.dir.GetObject(ctx, &storage.GetObjectInput{
		HeadObjectInput: storage.HeadObjectInput{BaseObjectInput: base},
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %s %q: %w", s.Name(), op, key, err)
	}

	// The dir backend's body is an io.ReadSeeker, which the trailer is
	// read through.
	rs, ok := obj.Body.(io.ReadSeekCloser)
	if !ok {
		err = fmt.Errorf("%s: %s %q: backend body %T is not seekable",
			s.Name(), op, key, obj.Body)
		return nil, errors.Join(err, obj.Body.Close())
	}

	tr, size, err := readTrailer(rs, obj.Size)
	if err != nil {
		err = fmt.Errorf("%s: %s %q: %w", s.Name(), op, key, err)
		return nil, errors.Join(err, obj.Body.Close())
	}

	info, err := objectInfo(key, tr, size)
	if err != nil {
		err = fmt.Errorf("%s: %s %q: %w", s.Name(), op, key, err)
		return nil, errors.Join(err, obj.Body.Close())
	}

	return &contracts.Object{ObjectInfo: *info, Body: &objectBody{rs: rs, size: size}}, nil
}

// DeleteObject removes an object. Deleting a key that holds no object
// succeeds. ctx must be pinned to bucket.
func (s *Service) DeleteObject(ctx context.Context, bucket, key string) error {
	if _, err := requirePin(ctx, bucket); err != nil {
		return err
	}
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return err
	}

	base, err := objectInput(bucket, key)
	if err != nil {
		return fmt.Errorf("%s: delete %q: %w", s.Name(), key, err)
	}

	unlock := s.locks.lock(bucket, key)
	defer unlock()

	if err = s.dir.DeleteObject(
		ctx,
		&storage.DeleteObjectInput{BaseObjectInput: base},
	); err != nil {
		return fmt.Errorf("%s: delete %q: %w", s.Name(), key, err)
	}
	return s.removeFromIndex(bucket, key)
}

// PutObject stores body with the content type, user metadata and system
// headers in in, and returns the stored object's info. The write only happens
// if cond holds. ctx must be pinned to bucket.
func (s *Service) PutObject(
	ctx context.Context,
	bucket, key string,
	body io.Reader,
	in contracts.PutObjectInput,
	cond contracts.Precondition,
) (*contracts.ObjectInfo, error) {
	if _, err := requirePin(ctx, bucket); err != nil {
		return nil, err
	}
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return nil, err
	}

	unlock := s.locks.lock(bucket, key)
	defer unlock()

	if err := s.checkPrecondition(ctx, bucket, key, cond); err != nil {
		return nil, err
	}

	er := newETagReader(body, joinMetadata(in.Metadata, in.Headers))
	return s.putObject(ctx, bucket, key, er, in.ContentType, er.meta)
}

// putObject indexes key and writes the object, with meta as the metadata
// of its trailer. An empty contentType is detected from the body. The
// caller holds the key's lock.
func (s *Service) putObject(
	ctx context.Context,
	bucket, key string,
	body io.Reader,
	contentType string,
	meta map[string]string,
) (*contracts.ObjectInfo, error) {
	base, err := objectInput(bucket, key)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Name(), err)
	}
	if err = s.addToIndex(bucket, key); err != nil {
		return nil, err
	}

	if _, err = s.dir.PutObject(ctx, &storage.PutObjectInput{
		BaseObjectInput: base,
		Body:            withTrailer(body, &trailer{Meta: meta, ContentType: contentType}),
	}); err != nil {
		err = fmt.Errorf("%s: put %q: %w", s.Name(), key, err)
		return nil, errors.Join(err, s.unindexMissing(ctx, base, bucket, key))
	}

	return s.StatObject(ctx, bucket, key)
}

// unindexMissing drops key from bucket's index when it holds no object, so a
// failed write of a new key does not leave its entry behind. base addresses
// key in the backend. The caller holds the key's lock.
func (s *Service) unindexMissing(
	ctx context.Context,
	base storage.BaseObjectInput,
	bucket, key string,
) error {
	_, err := s.dir.HeadObject(ctx, &storage.HeadObjectInput{BaseObjectInput: base})
	if !errors.Is(err, storage.ErrObjectNotFound) {
		return nil
	}
	return s.removeFromIndex(bucket, key)
}

// checkKey returns an error wrapping storage.ErrInvalidKey when key breaks
// the key rules, so a key is known to be storable before it is indexed.
func (s *Service) checkKey(key string) error {
	if _, err := objectInput("", key); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}
	return nil
}

// CopyObject copies an object within or across buckets. Copying an object
// onto itself is safe: the backend writes the copy to a new data file, while
// the source stays open for reading. ctx must be pinned to both buckets.
func (s *Service) CopyObject(
	ctx context.Context,
	in contracts.CopyObjectInput,
) (_ *contracts.ObjectInfo, err error) {
	src, err := s.GetObject(ctx, in.SrcBucket, in.SrcKey)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cErr := src.Body.Close(); cErr != nil {
			err = errors.Join(err, fmt.Errorf("%s: copy %q: %w", s.Name(), in.SrcKey, cErr))
		}
	}()

	put := contracts.PutObjectInput{
		ContentType: src.ContentType,
		Metadata:    src.Metadata,
		Headers:     src.Headers,
	}
	if in.Replace != nil {
		put = *in.Replace
	}

	return s.PutObject(ctx, in.DstBucket, in.DstKey, src.Body, put, in.Precondition)
}
