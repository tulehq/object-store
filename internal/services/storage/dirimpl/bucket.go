package dirimpl

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// bucketNameRe is the core of the S3 general purpose bucket naming rules:
// 3-63 characters of lowercase letters, digits, dots and hyphens, starting and
// ending with a letter or digit.
var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// The prefixes and suffixes the S3 bucket naming rules reserve. The backend
// accepts them, so CreateBucket refuses them itself.
var (
	reservedBucketPrefixes = []string{"xn--", "sthree-", "amzn-s3-demo-"}
	reservedBucketSuffixes = []string{
		"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3", "-an",
	}
)

// isReservedBucketName reports whether bucket has a reserved prefix or
// suffix.
func isReservedBucketName(bucket string) bool {
	return slices.ContainsFunc(reservedBucketPrefixes, func(p string) bool {
		return strings.HasPrefix(bucket, p)
	}) || slices.ContainsFunc(reservedBucketSuffixes, func(sfx string) bool {
		return strings.HasSuffix(bucket, sfx)
	})
}

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
// It requires ctx to be pinned to bucket.
func (s *Service) bucketKeys(ctx context.Context, bucket string) ([]string, error) {
	if _, err := requirePin(ctx, bucket); err != nil {
		return nil, err
	}
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
	if errors.Is(err, storage.ErrObjectNotFound) {
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
// interrupted create is completed by the next one. A policy left by an
// interrupted DeleteBucket of the same name is removed first, so the new
// bucket has none. Its registry entry is made under a temporary name with
// a new generation and renamed into place, so the bucket exists only with
// its generation. It holds the bucket's write lock throughout.
func (s *Service) CreateBucket(ctx context.Context, bucket string) (*contracts.BucketInfo, error) {
	if !bucketNameRe.MatchString(bucket) || isReservedBucketName(bucket) {
		return nil, contracts.ErrInvalidBucketName
	}
	unlock := s.bucketLocks.lock(bucket)
	defer unlock()
	if _, err := s.HeadBucket(ctx, bucket); err == nil {
		return nil, contracts.ErrBucketExists
	} else if !errors.Is(err, storage.ErrBucketNotFound) {
		return nil, err
	}

	if err := s.dir.CreateBucket(ctx, &storage.CreateBucketInput{Bucket: bucket}); err != nil {
		return nil, fmt.Errorf("%s: failed to create bucket: %s: %w", s.Name(), bucket, err)
	}
	if err := s.root.MkdirAll(filepath.Join(indexDir, bucket), 0o750); err != nil {
		return nil, fmt.Errorf("%s: failed to create bucket: %s: %w", s.Name(), bucket, err)
	}
	if err := s.removePolicy(bucket); err != nil {
		return nil, err
	}
	if err := s.registerBucket(bucket); err != nil {
		return nil, err
	}

	return s.HeadBucket(ctx, bucket)
}

// checkEmpty returns contracts.ErrBucketNotEmpty when bucket holds an
// object. An indexed key that holds none does not count.
func (s *Service) checkEmpty(ctx context.Context, bucket string) error {
	keys, err := s.indexedKeys(bucket)
	if err != nil {
		return err
	}
	for _, key := range keys {
		_, sErr := s.statObject(ctx, bucket, key)
		if sErr == nil {
			return contracts.ErrBucketNotEmpty
		}
		if !errors.Is(sErr, storage.ErrObjectNotFound) {
			return sErr
		}
	}
	return nil
}

// registerBucket adds bucket to the registry with a new generation. The
// entry is written under a name no bucket can have, which ListBuckets and
// HeadBucket ignore, and renamed into place.
func (s *Service) registerBucket(bucket string) error {
	tmp := filepath.Join(bucketsDir, ".new-"+rand.Text())
	if err := s.root.Mkdir(tmp, 0o750); err != nil {
		return fmt.Errorf("%s: failed to create bucket: %s: %w", s.Name(), bucket, err)
	}
	err := s.root.WriteFile(filepath.Join(tmp, generationFile), []byte(newGeneration()), 0o600)
	if err == nil {
		err = s.root.Rename(tmp, filepath.Join(bucketsDir, bucket))
	}
	if err != nil {
		return errors.Join(fmt.Errorf("%s: failed to create bucket: %s: %w", s.Name(), bucket, err),
			s.root.RemoveAll(tmp))
	}
	return nil
}

// DeleteBucket removes an empty bucket of ref's generation. It returns
// contracts.ErrBucketNotEmpty while the bucket holds objects, and
// storage.ErrBucketNotFound when there is no such bucket or it is of
// another generation.
//
// It holds the bucket's write lock, so it waits for the operations pinned
// to the bucket and none starts until it is done. The registry entry is
// renamed away first, which makes the bucket not exist, then the policy and
// the index are removed. The bucket is left in the backend, empty.
// Creating the bucket again reuses it, with a new generation, so an upload
// started on this bucket does not complete into the new one.
func (s *Service) DeleteBucket(ctx context.Context, ref contracts.BucketRef) error {
	bucket := ref.Name
	unlock := s.bucketLocks.lock(bucket)
	defer unlock()
	gen, err := s.generation(ctx, bucket)
	if err != nil {
		return err
	}
	if ref.Generation == "" || gen != ref.Generation {
		return storage.ErrBucketNotFound
	}
	if err = s.checkEmpty(ctx, bucket); err != nil {
		return err
	}

	gone := filepath.Join(bucketsDir, ".gone-"+rand.Text())
	if err = s.root.Rename(filepath.Join(bucketsDir, bucket), gone); err != nil {
		return fmt.Errorf("%s: failed to delete bucket: %s: %w", s.Name(), bucket, err)
	}
	if err = s.root.RemoveAll(gone); err != nil {
		return fmt.Errorf("%s: failed to delete bucket: %s: %w", s.Name(), bucket, err)
	}
	if err = s.removePolicy(bucket); err != nil {
		return err
	}
	if err = s.root.RemoveAll(filepath.Join(indexDir, bucket)); err != nil {
		return fmt.Errorf("%s: failed to delete bucket index: %s: %w", s.Name(), bucket, err)
	}

	return nil
}
