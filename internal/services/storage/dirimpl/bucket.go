package dirimpl

import (
	"context"
	"crypto/rand"
	"encoding/json"
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

// ListObjects lists one page of bucket; a MaxKeys of 0 or less is an
// empty page that is not truncated. Every call reads the bucket's whole
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
	if in.MaxKeys <= 0 {
		// A page that can hold nothing is empty, not truncated, so a
		// client paging on its markers does not loop, as
		// ListObjectVersions answers it.
		return &contracts.ListObjectsOutput{}, nil
	}

	out := &contracts.ListObjectsOutput{}
	count := 0
	lastPrefix := ""

	for _, key := range keys {
		if !selects(in, key) {
			continue
		}
		cp := commonPrefix(key, in.Prefix, in.Delimiter)
		info, prefix, lErr := s.listEntry(ctx, bucket, key, cp, lastPrefix)
		if lErr != nil {
			return nil, lErr
		}
		if info == nil && prefix == "" {
			continue
		}
		if count == in.MaxKeys {
			out.IsTruncated = true
			break
		}
		count++
		if prefix != "" {
			out.CommonPrefixes = append(out.CommonPrefixes, prefix)
			out.NextStartAfter, lastPrefix = prefix, prefix
			continue
		}
		out.Objects = append(out.Objects, *info)
		out.NextStartAfter = key
	}

	return out, nil
}

// listEntry returns what key adds to a listing: cp, the common prefix it
// rolls up into, when it is one to emit after lastPrefix (see
// emitsPrefix), its current version when it rolls up into none (see
// currentObject), or neither, when it adds nothing, which then takes no
// room in the page.
func (s *Service) listEntry(
	ctx context.Context,
	bucket, key, cp, lastPrefix string,
) (*contracts.ObjectInfo, string, error) {
	if cp == "" {
		info, err := s.currentObject(ctx, bucket, key)
		return info, "", err
	}
	emit, err := s.emitsPrefix(bucket, key, cp, lastPrefix)
	if err != nil || !emit {
		return nil, "", err
	}
	return nil, cp, nil
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

// currentObject returns the current version of the object at key, to
// list, or nil when there is none to list: an indexed key that holds no
// object, or whose current version is a delete marker, so only what is
// listed counts toward a page. The current version of a key with a
// manifest is described from its manifest, without opening it.
func (s *Service) currentObject(
	ctx context.Context,
	bucket, key string,
) (*contracts.ObjectInfo, error) {
	m, _, err := s.readVersions(bucket, key)
	if err != nil {
		return nil, err
	}
	if m != nil {
		e := m.live()
		if e == nil {
			return nil, nil
		}
		return &contracts.ObjectInfo{
			Key: key, ETag: e.ETag, Size: e.Size, LastModified: e.LastModified,
			VersionID: e.ID, IsLatest: true,
		}, nil
	}
	info, err := s.StatObject(ctx, contracts.ObjectRef{Bucket: bucket, Key: key})
	if errors.Is(err, storage.ErrObjectNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return info, nil
}

// selects reports whether in's listing includes key, or the common prefix
// it rolls up into.
func selects(in contracts.ListObjectsInput, key string) bool {
	return strings.HasPrefix(key, in.Prefix) && isAfter(key, in.StartAfter, in.Delimiter)
}

// emitsPrefix reports whether key makes a listing emit cp, the common
// prefix it rolls up into, when the last emitted was lastPrefix: once, for
// the first key under it that holds an object, or has no manifest, as any
// indexed key without one counts. A prefix whose keys all have a delete
// marker as their current version holds no object to list.
func (s *Service) emitsPrefix(bucket, key, cp, lastPrefix string) (bool, error) {
	if cp == lastPrefix {
		return false, nil
	}
	m, _, err := s.readVersions(bucket, key)
	if err != nil {
		return false, err
	}
	return m == nil || m.live() != nil, nil
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

// CreateBucket creates bucket with configs as its configuration. It returns
// contracts.ErrInvalidBucketName for a name S3 would reject and
// contracts.ErrBucketExists when it already exists.
//
// The bucket is created in the backend and given an index directory before
// it is added to the registry, which is what makes it exist, so an
// interrupted create is completed by the next one. A policy,
// configuration, object ACL or tag set, version manifest or versioning
// state left by an interrupted DeleteBucket of the same name is removed
// first, so the new bucket has only configs and is Unversioned. Its
// registry entry is made under a temporary name with
// a new generation and renamed into place, so the bucket exists only with
// its generation.
//
// An existing bucket is found under its read lock, so creating a bucket
// that exists, as an "ensure bucket" call does, never queues the bucket's
// requests behind a writer. Only an absent bucket takes the write lock,
// and is checked again under it.
func (s *Service) CreateBucket(
	ctx context.Context,
	bucket string,
	configs map[contracts.BucketConfig][]byte,
) (*contracts.BucketInfo, error) {
	if !bucketNameRe.MatchString(bucket) || isReservedBucketName(bucket) {
		return nil, contracts.ErrInvalidBucketName
	}
	runlock := s.bucketLocks.rlock(bucket)
	err := s.checkAbsent(ctx, bucket)
	runlock()
	if err != nil {
		return nil, err
	}
	unlock := s.bucketLocks.lock(bucket)
	defer unlock()
	if err = s.checkAbsent(ctx, bucket); err != nil {
		return nil, err
	}

	if err := s.dir.CreateBucket(ctx, &storage.CreateBucketInput{Bucket: bucket}); err != nil {
		return nil, fmt.Errorf("%s: failed to create bucket: %s: %w", s.Name(), bucket, err)
	}
	if err := s.root.MkdirAll(filepath.Join(indexDir, bucket), 0o750); err != nil {
		return nil, fmt.Errorf("%s: failed to create bucket: %s: %w", s.Name(), bucket, err)
	}
	if err := s.resetConfigs(bucket, configs); err != nil {
		return nil, err
	}
	if err := s.registerBucket(bucket); err != nil {
		return nil, err
	}

	return s.HeadBucket(ctx, bucket)
}

// checkAbsent returns contracts.ErrBucketExists when bucket exists.
func (s *Service) checkAbsent(ctx context.Context, bucket string) error {
	_, err := s.HeadBucket(ctx, bucket)
	switch {
	case err == nil:
		return contracts.ErrBucketExists
	case errors.Is(err, storage.ErrBucketNotFound):
		return nil
	}
	return err
}

// checkDeletable returns storage.ErrBucketNotFound when there is no bucket
// of ref's generation, and contracts.ErrBucketNotEmpty when it holds an
// object. The caller holds the bucket's lock.
func (s *Service) checkDeletable(ctx context.Context, ref contracts.BucketRef) error {
	gen, err := s.generation(ctx, ref.Name)
	if err != nil {
		return err
	}
	if ref.Generation == "" || gen != ref.Generation {
		return storage.ErrBucketNotFound
	}
	return s.checkEmpty(ctx, ref.Name)
}

// checkEmpty returns contracts.ErrBucketNotEmpty when bucket holds an
// object version or a delete marker: "All objects (including all object
// versions and delete markers) in the bucket must be deleted before the
// bucket itself can be deleted" (DeleteBucket API reference).
//
// It does not read the key index, whose entries are not synced: a power
// loss can drop the entry of a key whose manifest or object survives, and
// DeleteBucket would then purge live data (see purgeObjects). It reads
// what is durable instead: every manifest of the bucket, any of which
// with an entry is a version or marker, and the backend's objects, where
// an object at a keyHash that has no manifest is a key's object in place.
// That is exact, because an object at keyHash is garbage only behind a
// manifest: deleteLegacy removes it before the key's index entry,
// removeKey before the key's manifest, and collect removes a displaced
// Legacy entry's only after the manifest that no longer lists it, which
// stays. Version data under "<keyHash>/<token>" is visible only through a
// manifest entry, so what is left of it is garbage.
func (s *Service) checkEmpty(ctx context.Context, bucket string) error {
	manifests, err := s.manifestHashes(bucket)
	if err != nil {
		return err
	}
	var token *string
	for {
		out, lErr := s.dir.ListObjects(ctx, &storage.ListObjectsInput{
			Bucket: &bucket, ContinuationToken: token,
		})
		if lErr != nil {
			return fmt.Errorf("%s: failed to list the objects of bucket %s: %w",
				s.Name(), bucket, lErr)
		}
		for _, o := range out.Objects {
			if !strings.Contains(o.Key, "/") && !manifests[o.Key] {
				return contracts.ErrBucketNotEmpty
			}
		}
		if !out.IsTruncated {
			return nil
		}
		token = &out.NextContinuationToken
	}
}

// manifestHashes returns the keyHashes of the keys of bucket that have a
// manifest, or contracts.ErrBucketNotEmpty when one of them lists a
// version or marker. A manifest removed while they are read is none.
func (s *Service) manifestHashes(bucket string) (map[string]bool, error) {
	d := filepath.Join(versionsDir, bucket)
	entries, err := fs.ReadDir(s.root.FS(), d)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: read the versions of bucket %s: %w", s.Name(), bucket, err)
	}
	hashes := make(map[string]bool, len(entries))
	for _, e := range entries {
		// Anything else is a temp file of a manifest being written.
		if !indexEntryRe.MatchString(e.Name()) {
			continue
		}
		raw, rErr := s.root.ReadFile(filepath.Join(d, e.Name()))
		if errors.Is(rErr, fs.ErrNotExist) {
			continue
		}
		if rErr != nil {
			return nil, fmt.Errorf("%s: read the versions of bucket %s: %w",
				s.Name(), bucket, rErr)
		}
		var m versionManifest
		if rErr = json.Unmarshal(raw, &m); rErr != nil || keyHash(m.Key) != e.Name() {
			return nil, fmt.Errorf("%s: the versions %s of bucket %s are damaged: %w",
				s.Name(), e.Name(), bucket, errors.Join(storage.ErrBackend, rErr))
		}
		if len(m.Versions) > 0 {
			return nil, contracts.ErrBucketNotEmpty
		}
		hashes[e.Name()] = true
	}
	return hashes, nil
}

// removeContents removes what bucket, an empty bucket of generation gen,
// still holds: its uploads and the garbage objects of its backend bucket.
// The caller holds the bucket's write lock.
func (s *Service) removeContents(ctx context.Context, bucket, gen string) error {
	if err := s.removeUploads(bucket, gen); err != nil {
		return err
	}
	return s.purgeObjects(ctx, bucket)
}

// purgeObjects deletes every object the backend holds in bucket, which
// checkEmpty found holding no object version or marker: what is left is
// garbage no manifest lists, the data of a version a crash kept from being
// committed or removed, or an object at keyHash behind an empty manifest. The caller
// holds the bucket's write lock.
func (s *Service) purgeObjects(ctx context.Context, bucket string) error {
	for {
		out, err := s.dir.ListObjects(ctx, &storage.ListObjectsInput{Bucket: &bucket})
		if err != nil {
			return fmt.Errorf("%s: failed to list the objects of bucket %s: %w",
				s.Name(), bucket, err)
		}
		for _, o := range out.Objects {
			if err = s.dir.DeleteObject(ctx, &storage.DeleteObjectInput{
				BaseObjectInput: storage.BaseObjectInput{Bucket: &bucket, Key: o.Key},
			}); err != nil {
				return fmt.Errorf("%s: failed to delete garbage of bucket %s: %w",
					s.Name(), bucket, err)
			}
		}
		if !out.IsTruncated {
			return nil
		}
	}
}

// registerBucket adds bucket to the registry with a new generation. The
// entry is written under a name no bucket can have, which ListBuckets and
// HeadBucket ignore, and renamed into place.
func (s *Service) registerBucket(bucket string) error {
	tmp := filepath.Join(bucketsDir, ".new-"+rand.Text())
	if err := s.root.Mkdir(tmp, 0o750); err != nil {
		return fmt.Errorf("%s: failed to create bucket: %s: %w", s.Name(), bucket, err)
	}
	err := s.writeGeneration(filepath.Join(tmp, generationFile))
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
// The generation and emptiness are checked under the bucket's read lock
// first, so a bucket that is not empty is answered at once. The write lock
// is then tried for at most s.deleteWait, polling rather than waiting, so
// the bucket's requests are not blocked meanwhile; an operation pinned to
// the bucket that outlasts it fails the delete with
// contracts.ErrOperationAborted. Under the write lock both are checked
// again, the uploads of this generation and the objects left in the
// backend, which are garbage (see purgeObjects), are removed, and the
// registry entry is renamed away, which makes the bucket not exist; then
// the policy, the configurations, the object ACLs, tags and versions, the
// versioning state and the index are removed. The bucket is left in the
// backend, empty.
// Creating the bucket again reuses it, with a new generation.
func (s *Service) DeleteBucket(ctx context.Context, ref contracts.BucketRef) error {
	bucket := ref.Name
	runlock := s.bucketLocks.rlock(bucket)
	err := s.checkDeletable(ctx, ref)
	runlock()
	if err != nil {
		return err
	}
	unlock, ok := s.bucketLocks.tryLock(bucket, s.deleteWait)
	if !ok {
		return contracts.ErrOperationAborted
	}
	defer unlock()
	if err = s.checkDeletable(ctx, ref); err != nil {
		return err
	}
	if err = s.removeContents(ctx, bucket, ref.Generation); err != nil {
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
	if err = s.removeConfigs(bucket); err != nil {
		return err
	}
	if err = s.root.RemoveAll(filepath.Join(indexDir, bucket)); err != nil {
		return fmt.Errorf("%s: failed to delete bucket index: %s: %w", s.Name(), bucket, err)
	}

	return nil
}
