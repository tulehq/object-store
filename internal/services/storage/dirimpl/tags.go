package dirimpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"tule.dev/object-store/internal/contracts"
)

// objectTags is what is stored for an object's tags: the tags, and the
// ETag and modification time of the object they were stored for, as
// objectACL is for its ACL. Every write and delete of the object replaces
// or removes it; the two are checked too, so tags a crash left behind are
// not inherited by a later object.
type objectTags struct {
	LastModified time.Time       `json:"lastModified"`
	ETag         string          `json:"etag"`
	Tags         []contracts.Tag `json:"tags"`
}

// tagFile is the file the tags of the object at key are stored in.
func tagFile(bucket, key string) string {
	return filepath.Join(tagDir, bucket, keyHash(key))
}

// GetObjectTagging returns the tags stored for the object at key, if they
// were stored for the object as it now is, read under the bucket's lock,
// or its pin when ctx is pinned to it.
func (s *Service) GetObjectTagging(
	ctx context.Context,
	bucket, key string,
) ([]contracts.Tag, error) {
	if _, pinned := pinnedGenerations(ctx)[bucket]; !pinned {
		unlock := s.bucketLocks.rlock(bucket)
		defer unlock()
	}
	info, err := s.statObject(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	return s.readObjectTags(bucket, info)
}

// readObjectTags returns the tags stored for the object info describes,
// none when the stored ones were stored for another object at its key.
func (s *Service) readObjectTags(
	bucket string,
	info *contracts.ObjectInfo,
) ([]contracts.Tag, error) {
	raw, err := s.root.ReadFile(tagFile(bucket, info.Key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: failed to read the tags of %q: %w", s.Name(), info.Key, err)
	}
	var stored objectTags
	if err = json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("%s: failed to decode the tags of %q: %w", s.Name(), info.Key, err)
	}
	if stored.ETag != info.ETag || !stored.LastModified.Equal(info.LastModified) {
		return nil, nil
	}
	return stored.Tags, nil
}

// PutObjectTagging stores tags as the tags of the object at key as it now
// is, or removes its tags when there are none, under the key's lock, so
// the object they are stored for is not written over meanwhile. ctx must
// be pinned to bucket.
func (s *Service) PutObjectTagging(
	ctx context.Context,
	bucket, key string,
	tags []contracts.Tag,
) error {
	if _, err := requirePin(ctx, bucket); err != nil {
		return err
	}
	unlock := s.locks.lock(bucket, key)
	defer unlock()
	info, err := s.StatObject(ctx, bucket, key)
	if err != nil {
		return err
	}
	return s.setObjectTags(bucket, info, tags)
}

// setObjectTags stores tags as the tags of the object info describes, or
// removes the ones stored for its key when there are none, so no tag file
// outlives the object it was stored for. The caller holds the key's lock.
func (s *Service) setObjectTags(
	bucket string,
	info *contracts.ObjectInfo,
	tags []contracts.Tag,
) error {
	if len(tags) == 0 {
		if err := s.removeIfExists(tagFile(bucket, info.Key)); err != nil {
			return fmt.Errorf("%s: failed to delete the tags of %q: %w", s.Name(), info.Key, err)
		}
		return nil
	}
	raw, err := json.Marshal(objectTags{
		LastModified: info.LastModified, ETag: info.ETag, Tags: tags,
	})
	if err != nil {
		return fmt.Errorf("%s: failed to encode the tags of %q: %w", s.Name(), info.Key, err)
	}
	if err = s.writeFile(tagFile(bucket, info.Key), raw); err != nil {
		return fmt.Errorf("%s: failed to store the tags of %q: %w", s.Name(), info.Key, err)
	}
	return nil
}
