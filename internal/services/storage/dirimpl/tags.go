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

// objectTags is what is stored in an object's tag file: the tags, and the
// ETag and modification time of the object they were stored for, as
// objectACL is for its ACL. PutObjectTagging writes it, also for an empty
// tag set, which then overrides the tags the object was written with.
// The two are checked, so a tag file stored for an earlier object at the
// key is never the tags of a later one.
type objectTags struct {
	LastModified time.Time       `json:"lastModified"`
	ETag         string          `json:"etag"`
	Tags         []contracts.Tag `json:"tags"`
}

// tagFile is the file the tags of the object at key are stored in.
func tagFile(bucket, key string) string {
	return filepath.Join(tagDir, bucket, keyHash(key))
}

// GetObjectTagging returns the tags of the object at key as it now is (see
// StatObjectTagging).
func (s *Service) GetObjectTagging(
	ctx context.Context,
	bucket, key string,
) ([]contracts.Tag, error) {
	info, err := s.StatObjectTagging(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	return info.Tags, nil
}

// StatObjectTagging returns the info of the object at key as it now is,
// with its tags (see readObjectTags), both from one read of the object,
// under the bucket's lock, or its pin when ctx is pinned to it.
func (s *Service) StatObjectTagging(
	ctx context.Context,
	bucket, key string,
) (*contracts.ObjectInfo, error) {
	if _, pinned := pinnedGenerations(ctx)[bucket]; !pinned {
		unlock := s.bucketLocks.rlock(bucket)
		defer unlock()
	}
	obj, err := s.getObject(ctx, "stat", bucket, key)
	if err != nil {
		return nil, err
	}
	if err = obj.Body.Close(); err != nil {
		return nil, fmt.Errorf("%s: stat %q: %w", s.Name(), key, err)
	}
	return &obj.ObjectInfo, nil
}

// statChecked returns the info of the object at key as it now is, as
// StatObject does, if check passes for it, which is given the info with
// its tags. ctx must be pinned to bucket.
func (s *Service) statChecked(
	ctx context.Context,
	bucket, key string,
	check contracts.ObjectCheck,
) (*contracts.ObjectInfo, error) {
	if check == nil {
		return s.StatObject(ctx, bucket, key)
	}
	if _, err := requirePin(ctx, bucket); err != nil {
		return nil, err
	}
	obj, err := s.getObject(ctx, "stat", bucket, key)
	if err != nil {
		return nil, err
	}
	if err = obj.Body.Close(); err != nil {
		return nil, fmt.Errorf("%s: stat %q: %w", s.Name(), key, err)
	}
	if err = check(&obj.ObjectInfo); err != nil {
		return nil, err
	}
	return &obj.ObjectInfo, nil
}

// readObjectTags returns the tags of the object info describes, which was
// written with the tags written: the ones its tag file holds when that was
// stored for this object, which a PutObjectTagging or DeleteObjectTagging
// since its write wrote, and the ones it was written with otherwise. A tag
// file stored for another object at its key is ignored, so whether a write
// removed its predecessor's yet does not change what is read. An object
// written before tags were stored with it has none of its own, and its
// tag file, if any, is the one its write stored after it.
func (s *Service) readObjectTags(
	bucket string,
	info *contracts.ObjectInfo,
	written []contracts.Tag,
) ([]contracts.Tag, error) {
	raw, err := s.root.ReadFile(tagFile(bucket, info.Key))
	if errors.Is(err, fs.ErrNotExist) {
		return written, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: failed to read the tags of %q: %w", s.Name(), info.Key, err)
	}
	var stored objectTags
	if err = json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("%s: failed to decode the tags of %q: %w", s.Name(), info.Key, err)
	}
	if stored.ETag != info.ETag || !stored.LastModified.Equal(info.LastModified) {
		return written, nil
	}
	if len(stored.Tags) == 0 {
		return nil, nil
	}
	return stored.Tags, nil
}

// PutObjectTagging stores tags as the tags of the object at key as it now
// is, replacing the ones it has, or none when there are none, if check
// passes for it, under the key's lock, so the object they are stored for
// is not written over meanwhile. ctx must be pinned to bucket.
func (s *Service) PutObjectTagging(
	ctx context.Context,
	bucket, key string,
	tags []contracts.Tag,
	check contracts.ObjectCheck,
) error {
	if _, err := requirePin(ctx, bucket); err != nil {
		return err
	}
	unlock := s.locks.lock(bucket, key)
	defer unlock()
	info, err := s.statChecked(ctx, bucket, key, check)
	if err != nil {
		return err
	}
	return s.writeTagFile(bucket, info, tags)
}

// writeTagFile stores tags as the tags of the object info describes, in
// its tag file, which overrides the tags it was written with; an empty
// tag set is stored too, so those do not apply again. The caller holds the
// key's lock.
func (s *Service) writeTagFile(
	bucket string,
	info *contracts.ObjectInfo,
	tags []contracts.Tag,
) error {
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

// dropTagFile removes the tag file of key, which a write of the object
// leaves stored for the object it replaced, so tag files do not
// accumulate. Readers ignore such a file (see readObjectTags), so the
// object's tags do not depend on it. The caller holds the key's lock.
func (s *Service) dropTagFile(bucket, key string) error {
	if err := s.removeIfExists(tagFile(bucket, key)); err != nil {
		return fmt.Errorf("%s: failed to delete the tags of %q: %w", s.Name(), key, err)
	}
	return nil
}
