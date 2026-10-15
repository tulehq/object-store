package dirimpl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// Upload stores an object and records the MD5 of its body as its ETag.
func (s *Service) Upload(
	ctx context.Context,
	key string,
	data io.Reader,
	opts ...gas.StorageOption,
) error {
	_, _, meta := gas.ApplyStorageOptions(opts)
	er := newETagReader(data, meta)
	opts = append(opts, gas.WithMetadata(er.meta))

	if err := s.dir.Upload(ctx, key, er, opts...); err != nil {
		return fmt.Errorf("%s: upload %q: %w", s.Name(), key, err)
	}
	return nil
}

// Download opens an object for reading. The caller must close its Body.
func (s *Service) Download(
	ctx context.Context,
	key string,
	opts ...gas.StorageOption,
) (*gas.StorageObject, error) {
	obj, err := s.dir.Download(ctx, key, opts...)
	if err != nil {
		if isNotDir(err) {
			err = storage.ErrKeyNotFound
		}
		return nil, fmt.Errorf("%s: download %q: %w", s.Name(), key, err)
	}
	splitETag(obj.Metadata)
	splitHeaders(obj.Metadata)
	if _, err = splitParts(obj.Metadata); err != nil {
		return nil, errors.Join(
			fmt.Errorf("%s: download %q: %w", s.Name(), key, err), obj.Body.Close())
	}
	return obj, nil
}

// Delete removes an object. Deleting a key that holds no object succeeds,
// including a key that names a directory of nested keys or one this store
// cannot hold; the backend would otherwise remove the wrong file or fail.
func (s *Service) Delete(ctx context.Context, key string, opts ...gas.StorageOption) error {
	bucket, _, _ := gas.ApplyStorageOptions(opts)
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return err
	}

	unlock := s.locks.lock(bucket, key)
	defer unlock()

	if !supportedKey(key) {
		return nil
	}
	info, err := s.root.Stat(filepath.Join(bucket, key))
	if (err == nil && info.IsDir()) || isNotDir(err) {
		return nil
	}

	if err = s.dir.Delete(ctx, key, opts...); err != nil {
		return fmt.Errorf("%s: delete %q: %w", s.Name(), key, err)
	}
	return nil
}

// Head returns an object's content type, size, modification time and user
// metadata.
func (s *Service) Head(
	ctx context.Context,
	key string,
	opts ...gas.StorageOption,
) (*gas.ObjectInfo, error) {
	info, err := s.dir.Head(ctx, key, opts...)
	if err != nil {
		if isNotDir(err) {
			err = storage.ErrKeyNotFound
		}
		return nil, fmt.Errorf("%s: head %q: %w", s.Name(), key, err)
	}
	splitETag(info.Metadata)
	splitHeaders(info.Metadata)
	if _, err = splitParts(info.Metadata); err != nil {
		return nil, fmt.Errorf("%s: head %q: %w", s.Name(), key, err)
	}
	return info, nil
}

// StatObject returns an object's metadata, with its ETag split out of the
// user metadata.
func (s *Service) StatObject(
	ctx context.Context,
	bucket, key string,
) (*contracts.ObjectInfo, error) {
	if !supportedKey(key) {
		if _, err := s.HeadBucket(ctx, bucket); err != nil {
			return nil, err
		}
		return nil, storage.ErrKeyNotFound
	}

	info, err := s.dir.Head(ctx, key, gas.InBucket(bucket))
	if err != nil {
		if isNotDir(err) {
			err = storage.ErrKeyNotFound
		}
		return nil, fmt.Errorf("%s: stat %q: %w", s.Name(), key, err)
	}

	parts, err := splitParts(info.Metadata)
	if err != nil {
		return nil, fmt.Errorf("%s: stat %q: %w", s.Name(), key, err)
	}

	return &contracts.ObjectInfo{
		Key:          key,
		ETag:         splitETag(info.Metadata),
		Headers:      splitHeaders(info.Metadata),
		PartSizes:    parts,
		Size:         info.Size,
		ContentType:  info.ContentType,
		LastModified: info.LastModified,
		Metadata:     info.Metadata,
	}, nil
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

	if err := s.checkWritable(bucket, key); err != nil {
		return nil, err
	}
	if err := s.checkPrecondition(ctx, bucket, key, cond); err != nil {
		return nil, err
	}

	opts := []gas.StorageOption{
		gas.InBucket(bucket),
		gas.WithMetadata(joinMetadata(in.Metadata, in.Headers)),
	}
	if in.ContentType != "" {
		opts = append(opts, gas.WithContentType(in.ContentType))
	}

	if err := s.Upload(ctx, key, body, opts...); err != nil {
		return nil, err
	}

	return s.StatObject(ctx, bucket, key)
}

// CopyObject copies an object within or across buckets. Copying an object
// onto itself is safe: the backend writes the copy to a temp file and renames
// it into place, while the source stays open for reading.
func (s *Service) CopyObject(
	ctx context.Context,
	in contracts.CopyObjectInput,
) (_ *contracts.ObjectInfo, err error) {
	src, err := s.StatObject(ctx, in.SrcBucket, in.SrcKey)
	if err != nil {
		return nil, err
	}

	obj, err := s.dir.Download(ctx, in.SrcKey, gas.InBucket(in.SrcBucket))
	if err != nil {
		return nil, fmt.Errorf("%s: copy %q: %w", s.Name(), in.SrcKey, err)
	}
	defer func() {
		if cErr := obj.Body.Close(); cErr != nil {
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

	return s.PutObject(ctx, in.DstBucket, in.DstKey, obj.Body, put, in.Precondition)
}

// PresignDownloadURL returns a download URL from the backend's presigner. The
// S3 API does not use it: clients presign URLs themselves.
func (s *Service) PresignDownloadURL(
	ctx context.Context,
	key string,
	ttl time.Duration,
	opts ...gas.StorageOption,
) (string, error) {
	u, err := s.dir.PresignDownloadURL(ctx, key, ttl, opts...)
	if err != nil {
		return "", fmt.Errorf("%s: presign download %q: %w", s.Name(), key, err)
	}
	return u, nil
}

// PresignUploadURL returns an upload URL from the backend's presigner. The
// S3 API does not use it: clients presign URLs themselves.
func (s *Service) PresignUploadURL(
	ctx context.Context,
	key string,
	ttl time.Duration,
	opts ...gas.StorageOption,
) (string, error) {
	u, err := s.dir.PresignUploadURL(ctx, key, ttl, opts...)
	if err != nil {
		return "", fmt.Errorf("%s: presign upload %q: %w", s.Name(), key, err)
	}
	return u, nil
}
