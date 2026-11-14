package dirimpl

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"tule.dev/object-store/internal/contracts"
)

// configFile is the file bucket's configuration of kind is stored in, a
// side file as the bucket's policy is. Bucket names are valid file names
// of their own (see policyFile).
func configFile(kind contracts.BucketConfig, bucket string) string {
	return filepath.Join(configDir, string(kind), bucket)
}

// GetBucketConfig returns bucket's configuration of kind, read under the
// bucket's lock, or its pin when ctx is pinned to it:
// contracts.ErrNoSuchBucketConfig when the bucket has none,
// storage.ErrBucketNotFound when there is no bucket.
func (s *Service) GetBucketConfig(
	ctx context.Context,
	bucket string,
	kind contracts.BucketConfig,
) ([]byte, error) {
	if _, pinned := pinnedGenerations(ctx)[bucket]; !pinned {
		unlock := s.bucketLocks.rlock(bucket)
		defer unlock()
	}
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return nil, err
	}
	doc, err := s.root.ReadFile(configFile(kind, bucket))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, contracts.ErrNoSuchBucketConfig
	}
	if err != nil {
		return nil, fmt.Errorf("%s: failed to read the %s configuration of bucket %s: %w",
			s.Name(), kind, bucket, err)
	}
	return doc, nil
}

// PutBucketConfig stores doc as bucket's configuration of kind. ctx must be
// pinned to bucket. It is written to a temp file and renamed into place, so
// a reader sees the old document or the new one, never part of either.
func (s *Service) PutBucketConfig(
	ctx context.Context,
	bucket string,
	kind contracts.BucketConfig,
	doc []byte,
) error {
	if _, err := requirePin(ctx, bucket); err != nil {
		return err
	}
	if err := s.writeFile(configFile(kind, bucket), doc); err != nil {
		return fmt.Errorf("%s: failed to store the %s configuration of bucket %s: %w",
			s.Name(), kind, bucket, err)
	}
	return nil
}

// DeleteBucketConfig removes bucket's configuration of kind, if it has
// one. ctx must be pinned to bucket.
func (s *Service) DeleteBucketConfig(
	ctx context.Context,
	bucket string,
	kind contracts.BucketConfig,
) error {
	if _, err := requirePin(ctx, bucket); err != nil {
		return err
	}
	if err := s.removeIfExists(configFile(kind, bucket)); err != nil {
		return fmt.Errorf("%s: failed to delete the %s configuration of bucket %s: %w",
			s.Name(), kind, bucket, err)
	}
	return nil
}

// resetConfigs removes the policy, configuration, object ACLs and object
// tags bucket has, which an interrupted DeleteBucket can leave, and stores
// configs as its configuration, for CreateBucket, which holds the bucket's
// write lock.
func (s *Service) resetConfigs(bucket string, configs map[contracts.BucketConfig][]byte) error {
	if err := s.removePolicy(bucket); err != nil {
		return err
	}
	if err := s.removeConfigs(bucket); err != nil {
		return err
	}
	for kind, doc := range configs {
		if err := s.writeFile(configFile(kind, bucket), doc); err != nil {
			return fmt.Errorf("%s: failed to store the %s configuration of bucket %s: %w",
				s.Name(), kind, bucket, err)
		}
	}
	return nil
}

// removeConfigs removes every configuration, object ACL and object tag
// set of bucket. The caller holds the bucket's write lock.
func (s *Service) removeConfigs(bucket string) error {
	for _, kind := range contracts.BucketConfigs {
		if err := s.removeIfExists(configFile(kind, bucket)); err != nil {
			return fmt.Errorf("%s: failed to delete the %s configuration of bucket %s: %w",
				s.Name(), kind, bucket, err)
		}
	}
	if err := s.root.RemoveAll(filepath.Join(aclDir, bucket)); err != nil {
		return fmt.Errorf("%s: failed to delete the object ACLs of bucket %s: %w",
			s.Name(), bucket, err)
	}
	if err := s.root.RemoveAll(filepath.Join(tagDir, bucket)); err != nil {
		return fmt.Errorf("%s: failed to delete the object tags of bucket %s: %w",
			s.Name(), bucket, err)
	}
	return nil
}

// writeFile writes doc to name through a temp file renamed into place.
func (s *Service) writeFile(name string, doc []byte) error {
	if err := s.root.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		return err //nolint:wrapcheck // wrapped by the caller with what is written
	}
	tmp := name + ".tmp-" + rand.Text()
	if err := s.root.WriteFile(tmp, doc, 0o600); err != nil {
		return errors.Join(err, s.removeIfExists(tmp))
	}
	if err := s.root.Rename(tmp, name); err != nil {
		return errors.Join(err, s.removeIfExists(tmp))
	}
	return nil
}

// objectACL is what is stored for an object's ACL: the document, and the
// ETag and modification time of the object it was stored for. Every write
// and delete of the object replaces or removes it; the two are checked
// too, so an ACL a crash left behind is not inherited by a later object.
type objectACL struct {
	LastModified time.Time `json:"lastModified"`
	ETag         string    `json:"etag"`
	ACL          []byte    `json:"acl"`
}

// aclFile is the file the ACL of the object at key is stored in.
func aclFile(bucket, key string) string {
	return filepath.Join(aclDir, bucket, keyHash(key))
}

// GetObjectACL returns the ACL stored for the object at key, if it was
// stored for the object as it now is. ctx must be pinned to bucket.
func (s *Service) GetObjectACL(ctx context.Context, bucket, key string) ([]byte, error) {
	info, err := s.StatObject(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	raw, err := s.root.ReadFile(aclFile(bucket, key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, contracts.ErrNoSuchBucketConfig
	}
	if err != nil {
		return nil, fmt.Errorf("%s: failed to read the ACL of %q: %w", s.Name(), key, err)
	}
	var stored objectACL
	if err = json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("%s: failed to decode the ACL of %q: %w", s.Name(), key, err)
	}
	if stored.ETag != info.ETag || !stored.LastModified.Equal(info.LastModified) {
		return nil, contracts.ErrNoSuchBucketConfig
	}
	return stored.ACL, nil
}

// PutObjectACL stores doc as the ACL of the object at key as it now is,
// under the key's lock, so the object it is stored for is not written over
// meanwhile. ctx must be pinned to bucket.
func (s *Service) PutObjectACL(ctx context.Context, bucket, key string, doc []byte) error {
	if _, err := requirePin(ctx, bucket); err != nil {
		return err
	}
	unlock := s.locks.lock(bucket, key)
	defer unlock()
	info, err := s.StatObject(ctx, bucket, key)
	if err != nil {
		return err
	}
	return s.writeObjectACL(bucket, info, doc)
}

// setObjectACL stores doc as the ACL of the object info describes, just
// written, or removes the one stored for its key when doc is nil, so no
// ACL file outlives the object it was stored for. The caller holds the
// key's lock.
func (s *Service) setObjectACL(bucket string, info *contracts.ObjectInfo, doc []byte) error {
	if doc != nil {
		return s.writeObjectACL(bucket, info, doc)
	}
	if err := s.removeIfExists(aclFile(bucket, info.Key)); err != nil {
		return fmt.Errorf("%s: failed to delete the ACL of %q: %w", s.Name(), info.Key, err)
	}
	return nil
}

// writeObjectACL stores doc as the ACL of the object info describes. The
// caller holds the key's lock.
func (s *Service) writeObjectACL(bucket string, info *contracts.ObjectInfo, doc []byte) error {
	raw, err := json.Marshal(objectACL{LastModified: info.LastModified, ETag: info.ETag, ACL: doc})
	if err != nil {
		return fmt.Errorf("%s: failed to encode the ACL of %q: %w", s.Name(), info.Key, err)
	}
	if err = s.writeFile(aclFile(bucket, info.Key), raw); err != nil {
		return fmt.Errorf("%s: failed to store the ACL of %q: %w", s.Name(), info.Key, err)
	}
	return nil
}
