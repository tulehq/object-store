package dirimpl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// bucketNameRe is the core of the S3 general purpose bucket naming rules:
// 3-63 characters of lowercase letters, digits, dots and hyphens, starting and
// ending with a letter or digit.
var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// ListBuckets returns every bucket in the bucket registry.
func (s *Service) ListBuckets(_ context.Context) ([]contracts.BucketInfo, error) {
	entries, err := fs.ReadDir(s.root.FS(), bucketsDir)
	if err != nil {
		return nil, fmt.Errorf("%s: failed to read bucket registry: %w", s.Name(), err)
	}

	buckets := make([]contracts.BucketInfo, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !bucketNameRe.MatchString(e.Name()) {
			continue
		}
		info, iErr := e.Info()
		if iErr != nil {
			return nil, fmt.Errorf("%s: failed to stat bucket %s: %w", s.Name(), e.Name(), iErr)
		}
		buckets = append(buckets, contracts.BucketInfo{
			Name:         e.Name(),
			CreationDate: info.ModTime(),
		})
	}

	return buckets, nil
}

// ListObjects lists one page of bucket. Every call reads the bucket's whole
// key index, which is fine at local-development scale.
func (s *Service) ListObjects(
	ctx context.Context,
	bucket string,
	in contracts.ListObjectsInput,
) (*contracts.ListObjectsOutput, error) {
	keys, err := s.bucketKeys(ctx, bucket)
	if err != nil {
		return nil, err
	}

	out := &contracts.ListObjectsOutput{}
	count := 0
	lastPrefix := ""

	for _, key := range keys {
		if !strings.HasPrefix(key, in.Prefix) || !isAfter(key, in.StartAfter, in.Delimiter) {
			continue
		}

		if cp := commonPrefix(key, in.Prefix, in.Delimiter); cp != "" {
			if cp == lastPrefix {
				continue
			}
			if count == in.MaxKeys {
				out.IsTruncated = true
				break
			}
			out.CommonPrefixes = append(out.CommonPrefixes, cp)
			out.NextStartAfter = cp
			lastPrefix = cp
			count++
			continue
		}

		if count == in.MaxKeys {
			out.IsTruncated = true
			break
		}
		if err = s.listObject(ctx, bucket, key, out); err != nil {
			return nil, err
		}
		count = len(out.CommonPrefixes) + len(out.Objects)
	}

	return out, nil
}

// bucketKeys returns the keys in bucket's index, sorted, or
// storage.ErrBucketNotFound when there is no such bucket.
func (s *Service) bucketKeys(ctx context.Context, bucket string) ([]string, error) {
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return nil, err
	}
	return s.indexedKeys(bucket)
}

// listObject adds the object at key to out. An indexed key that holds no
// object is skipped.
func (s *Service) listObject(
	ctx context.Context,
	bucket, key string,
	out *contracts.ListObjectsOutput,
) error {
	info, err := s.StatObject(ctx, bucket, key)
	if errors.Is(err, storage.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	out.Objects = append(out.Objects, *info)
	out.NextStartAfter = key
	return nil
}

// isAfter reports whether key sorts after the listing's start point. A start
// point that is a common prefix (it ends with the delimiter) also skips every
// key under it, so resuming a page never repeats that prefix.
func isAfter(key, startAfter, delimiter string) bool {
	if startAfter == "" {
		return true
	}
	if delimiter != "" && strings.HasSuffix(startAfter, delimiter) &&
		strings.HasPrefix(key, startAfter) {
		return false
	}
	return key > startAfter
}

// commonPrefix returns the prefix key rolls up into, or "" when key is listed
// on its own.
func commonPrefix(key, prefix, delimiter string) string {
	if delimiter == "" {
		return ""
	}
	i := strings.Index(key[len(prefix):], delimiter)
	if i < 0 {
		return ""
	}
	return key[:len(prefix)+i+len(delimiter)]
}

// HeadBucket returns storage.ErrBucketNotFound when bucket is not in the
// bucket registry.
func (s *Service) HeadBucket(_ context.Context, bucket string) (*contracts.BucketInfo, error) {
	if !bucketNameRe.MatchString(bucket) {
		return nil, storage.ErrBucketNotFound
	}

	info, err := s.root.Stat(filepath.Join(bucketsDir, bucket))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, storage.ErrBucketNotFound
		}
		return nil, fmt.Errorf("%s: failed to stat bucket: %s: %w", s.Name(), bucket, err)
	}
	if !info.IsDir() {
		return nil, storage.ErrBucketNotFound
	}

	return &contracts.BucketInfo{Name: bucket, CreationDate: info.ModTime()}, nil
}

// CreateBucket creates bucket. It returns contracts.ErrInvalidBucketName for a
// name S3 would reject and contracts.ErrBucketExists when it already exists.
//
// The bucket is created in the backend and given an index directory before
// it is added to the registry, which is what makes it exist, so an
// interrupted create is completed by the next one.
func (s *Service) CreateBucket(ctx context.Context, bucket string) (*contracts.BucketInfo, error) {
	if !bucketNameRe.MatchString(bucket) {
		return nil, contracts.ErrInvalidBucketName
	}
	if _, err := s.HeadBucket(ctx, bucket); err == nil {
		return nil, contracts.ErrBucketExists
	} else if !errors.Is(err, storage.ErrBucketNotFound) {
		return nil, err
	}

	if _, err := s.dir.CreateBucket(ctx, &storage.CreateBucketInput{Bucket: bucket}); err != nil {
		return nil, fmt.Errorf("%s: failed to create bucket: %s: %w", s.Name(), bucket, err)
	}
	if err := s.root.MkdirAll(filepath.Join(indexDir, bucket), 0o750); err != nil {
		return nil, fmt.Errorf("%s: failed to create bucket: %s: %w", s.Name(), bucket, err)
	}
	if err := s.root.Mkdir(filepath.Join(bucketsDir, bucket), 0o750); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, contracts.ErrBucketExists
		}
		return nil, fmt.Errorf("%s: failed to create bucket: %s: %w", s.Name(), bucket, err)
	}

	return s.HeadBucket(ctx, bucket)
}

// DeleteBucket removes an empty bucket. It returns contracts.ErrBucketNotEmpty
// while the bucket holds objects.
//
// The backend cannot delete buckets, so its directories for the bucket stay
// behind, empty. Creating the bucket again reuses them.
func (s *Service) DeleteBucket(ctx context.Context, bucket string) error {
	keys, err := s.bucketKeys(ctx, bucket)
	if err != nil {
		return err
	}
	for _, key := range keys {
		_, sErr := s.StatObject(ctx, bucket, key)
		if sErr == nil {
			return contracts.ErrBucketNotEmpty
		}
		if !errors.Is(sErr, storage.ErrKeyNotFound) {
			return sErr
		}
	}

	if err = s.root.Remove(filepath.Join(bucketsDir, bucket)); err != nil {
		return fmt.Errorf("%s: failed to delete bucket: %s: %w", s.Name(), bucket, err)
	}
	if err = s.root.RemoveAll(filepath.Join(indexDir, bucket)); err != nil {
		return fmt.Errorf("%s: failed to delete bucket index: %s: %w", s.Name(), bucket, err)
	}

	return nil
}
