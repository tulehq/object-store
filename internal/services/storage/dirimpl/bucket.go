package dirimpl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// The dir backend keeps these next to each object. They mirror unexported
// constants in github.com/gasmod/gas/storage/dir and must stay in sync with it.
const (
	backendMetaFilePrefix = ".__GAS_FILE_METADATA__"
	backendTmpFilePrefix  = ".__GAS_FILE_TMP__"
)

// bucketNameRe is the core of the S3 general purpose bucket naming rules:
// 3-63 characters of lowercase letters, digits, dots and hyphens, starting and
// ending with a letter or digit.
var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// ListBuckets returns every bucket: the root's subdirectories that are valid
// bucket names. A bucket's creation date is its directory's modification time.
func (s *Service) ListBuckets(_ context.Context) ([]contracts.BucketInfo, error) {
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("%s: failed to read root directory: %w", s.Name(), err)
	}

	buckets := make([]contracts.BucketInfo, 0, len(entries))
	for _, e := range entries {
		// Directories that are not valid bucket names, such as the multipart
		// staging area, are not buckets.
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

// ListObjects lists one page of bucket. Every call walks the whole bucket
// directory, which is fine at local-development scale.
func (s *Service) ListObjects(
	ctx context.Context,
	bucket string,
	in contracts.ListObjectsInput,
) (*contracts.ListObjectsOutput, error) {
	keys, err := s.bucketKeys(bucket)
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
		info, sErr := s.StatObject(ctx, bucket, key)
		if sErr != nil {
			return nil, sErr
		}
		out.Objects = append(out.Objects, *info)
		out.NextStartAfter = key
		count++
	}

	return out, nil
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

// bucketKeys returns every object key in bucket, sorted. It skips the dir
// backend's metadata and temp files, and directories, which are left behind
// when nested keys are deleted and do not count as objects.
func (s *Service) bucketKeys(bucket string) ([]string, error) {
	if !bucketNameRe.MatchString(bucket) {
		return nil, storage.ErrBucketNotFound
	}

	var keys []string
	err := fs.WalkDir(s.root.FS(), bucket, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, backendMetaFilePrefix) ||
			strings.HasPrefix(name, backendTmpFilePrefix) {
			return nil
		}
		keys = append(keys, strings.TrimPrefix(p, bucket+"/"))
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, storage.ErrBucketNotFound
		}
		return nil, fmt.Errorf("%s: failed to list bucket %s: %w", s.Name(), bucket, err)
	}

	slices.Sort(keys)
	return keys, nil
}

// HeadBucket returns storage.ErrBucketNotFound when bucket does not exist.
func (s *Service) HeadBucket(_ context.Context, bucket string) (*contracts.BucketInfo, error) {
	if !bucketNameRe.MatchString(bucket) {
		return nil, storage.ErrBucketNotFound
	}

	info, err := s.root.Stat(bucket)
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
func (s *Service) CreateBucket(_ context.Context, bucket string) (*contracts.BucketInfo, error) {
	if !bucketNameRe.MatchString(bucket) {
		return nil, contracts.ErrInvalidBucketName
	}

	if err := s.root.Mkdir(bucket, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, contracts.ErrBucketExists
		}
		return nil, fmt.Errorf("%s: failed to create bucket: %s: %w", s.Name(), bucket, err)
	}

	info, err := s.root.Stat(bucket)
	if err != nil {
		return nil, fmt.Errorf("%s: failed to stat bucket: %s: %w", s.Name(), bucket, err)
	}

	return &contracts.BucketInfo{Name: bucket, CreationDate: info.ModTime()}, nil
}

// DeleteBucket removes an empty bucket. It returns contracts.ErrBucketNotEmpty
// while the bucket holds objects.
func (s *Service) DeleteBucket(_ context.Context, bucket string) error {
	keys, err := s.bucketKeys(bucket)
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		return contracts.ErrBucketNotEmpty
	}

	// Anything left is empty directories from deleted nested keys or orphaned
	// backend temp files, none of which S3 would count as objects.
	if err = s.root.RemoveAll(bucket); err != nil {
		return fmt.Errorf("%s: failed to delete bucket: %s: %w", s.Name(), bucket, err)
	}

	return nil
}
