package dirimpl

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gasmod/gas/storage"
	"github.com/gasmod/gas/storage/dir"

	"tule.dev/object-store/internal/contracts"
)

// objectInput addresses key in bucket in the backend.
func objectInput(bucket, key string) storage.BaseObjectInput {
	return storage.BaseObjectInput{Bucket: &bucket, Key: key}
}

// objectInfo returns the stored object info, with the ETag, system headers
// and part sizes split out of the backend metadata.
func objectInfo(key string, info *storage.ObjectInfo) (*contracts.ObjectInfo, error) {
	meta := info.Metadata
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
		Size:         info.Size,
		ContentType:  info.ContentType,
		LastModified: info.LastModified,
		Metadata:     meta,
	}, nil
}

// StatObject returns an object's metadata, with its ETag split out of the
// user metadata.
func (s *Service) StatObject(
	ctx context.Context,
	bucket, key string,
) (*contracts.ObjectInfo, error) {
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return nil, err
	}

	info, err := s.dir.HeadObject(ctx, &storage.HeadObjectInput{
		BaseObjectInput: objectInput(bucket, key),
	})
	if err != nil {
		// The backend reports a missing object to a HEAD as ErrNotFound; the
		// S3 API treats it as a missing key whatever the method.
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf(
				"%s: stat %q: %w: %w", s.Name(), key, storage.ErrKeyNotFound, err)
		}
		return nil, fmt.Errorf("%s: stat %q: %w", s.Name(), key, err)
	}

	out, err := objectInfo(key, info)
	if err != nil {
		return nil, fmt.Errorf("%s: stat %q: %w", s.Name(), key, err)
	}
	return out, nil
}

// GetObject opens an object for reading. The caller must close its Body. The
// info and body come from the same read, so they match even when the object
// is overwritten meanwhile.
func (s *Service) GetObject(ctx context.Context, bucket, key string) (*contracts.Object, error) {
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return nil, err
	}

	obj, err := s.dir.GetObject(ctx, &storage.GetObjectInput{
		HeadObjectInput: storage.HeadObjectInput{BaseObjectInput: objectInput(bucket, key)},
	})
	if err != nil {
		return nil, fmt.Errorf("%s: get %q: %w", s.Name(), key, err)
	}

	info, err := objectInfo(key, &obj.ObjectInfo)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("%s: get %q: %w", s.Name(), key, err), obj.Body.Close())
	}
	return &contracts.Object{ObjectInfo: *info, Body: obj.Body}, nil
}

// DeleteObject removes an object. Deleting a key that holds no object
// succeeds.
func (s *Service) DeleteObject(ctx context.Context, bucket, key string) error {
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return err
	}

	unlock := s.locks.lock(bucket, key)
	defer unlock()

	if _, err := s.dir.DeleteObject(ctx, &storage.DeleteObjectInput{
		BaseObjectInput: objectInput(bucket, key),
	}); err != nil {
		return fmt.Errorf("%s: delete %q: %w", s.Name(), key, err)
	}
	return s.removeFromIndex(bucket, key)
}

// PutObject stores body with the content type, user metadata and system
// headers in in, and returns the stored object's info. The write only happens
// if cond holds.
func (s *Service) PutObject(
	ctx context.Context,
	bucket, key string,
	body io.Reader,
	in contracts.PutObjectInput,
	cond contracts.Precondition,
) (*contracts.ObjectInfo, error) {
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

// putObject indexes key and writes the object, with meta as its backend
// metadata. The caller holds the key's lock.
func (s *Service) putObject(
	ctx context.Context,
	bucket, key string,
	body io.Reader,
	contentType string,
	meta map[string]string,
) (*contracts.ObjectInfo, error) {
	if err := s.checkKey(ctx, bucket, key); err != nil {
		return nil, err
	}
	if err := s.addToIndex(bucket, key); err != nil {
		return nil, err
	}

	in := &storage.PutObjectInput{
		BaseObjectInput: objectInput(bucket, key),
		Body:            body,
		Metadata:        meta,
	}
	if contentType != "" {
		in.ContentType = &contentType
	}

	if _, err := s.dir.PutObject(ctx, in); err != nil {
		if !errors.Is(err, dir.ErrStaleData) {
			err = fmt.Errorf("%s: put %q: %w", s.Name(), key, err)
			return nil, errors.Join(err, s.unindexMissing(ctx, bucket, key))
		}
		// The object was written; only the data file it replaced is left
		// behind.
		s.logger.Warn("object written, but its previous data file was left on disk").
			Err("error", err).Send()
	}

	return s.StatObject(ctx, bucket, key)
}

// unindexMissing drops key from bucket's index when it holds no object, so a
// failed write of a new key does not leave its entry behind. The caller holds
// the key's lock.
func (s *Service) unindexMissing(ctx context.Context, bucket, key string) error {
	_, err := s.dir.HeadObject(ctx, &storage.HeadObjectInput{
		BaseObjectInput: objectInput(bucket, key),
	})
	if !errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return s.removeFromIndex(bucket, key)
}

// checkKey returns an error wrapping storage.ErrInvalidKey when the backend
// cannot store key. The backend validates keys before it touches the disk,
// so asking it about the key is how this is known before the key is indexed.
func (s *Service) checkKey(ctx context.Context, bucket, key string) error {
	_, err := s.dir.HeadObject(ctx, &storage.HeadObjectInput{
		BaseObjectInput: objectInput(bucket, key),
	})
	if errors.Is(err, storage.ErrInvalidKey) {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}
	return nil
}

// CopyObject copies an object within or across buckets. Copying an object
// onto itself is safe: the backend writes the copy to a new data file, while
// the source stays open for reading.
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
