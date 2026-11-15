package dirimpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// tagFile is the file the tags of the object at key, a key without a
// manifest, are stored in, which is also the one of a Legacy entry's
// version (see sideFile).
func tagFile(bucket, key string) string {
	return sideFile(tagDir, bucket, key, nil)
}

// GetObjectTagging returns the tags of the version ref names as it now is
// (see StatObjectTagging).
func (s *Service) GetObjectTagging(
	ctx context.Context,
	ref contracts.ObjectRef,
) ([]contracts.Tag, error) {
	info, err := s.StatObjectTagging(ctx, ref)
	if err != nil {
		return nil, err
	}
	return info.Tags, nil
}

// StatObjectTagging returns the info of the version ref names as it now
// is, with its tags (see readObjectTags), both from one read of the
// object, under the bucket's lock, or its pin when ctx is pinned to it.
func (s *Service) StatObjectTagging(
	ctx context.Context,
	ref contracts.ObjectRef,
) (*contracts.ObjectInfo, error) {
	if _, pinned := pinnedGenerations(ctx)[ref.Bucket]; !pinned {
		unlock := s.bucketLocks.rlock(ref.Bucket)
		defer unlock()
	}
	obj, _, err := s.getObject(ctx, "stat", ref)
	if err != nil {
		return nil, err
	}
	if err = obj.Body.Close(); err != nil {
		return nil, fmt.Errorf("%s: stat %q: %w", s.Name(), ref.Key, err)
	}
	return &obj.ObjectInfo, nil
}

// statChecked returns the info of the version ref names as it now is, as
// StatObject does, and its entry, nil for a key without a manifest, if
// check passes for it, which is given the info with its tags. ctx must be
// pinned to the bucket.
func (s *Service) statChecked(
	ctx context.Context,
	ref contracts.ObjectRef,
	check contracts.ObjectCheck,
) (*contracts.ObjectInfo, *versionEntry, error) {
	if _, err := requirePin(ctx, ref.Bucket); err != nil {
		return nil, nil, err
	}
	if check == nil {
		r, err := s.resolve(ctx, "stat", ref)
		if err != nil {
			return nil, nil, err
		}
		if err = r.obj.Body.Close(); err != nil {
			return nil, nil, fmt.Errorf("%s: stat %q: %w", s.Name(), ref.Key, err)
		}
		return &r.obj.ObjectInfo, r.entry, nil
	}
	obj, e, err := s.getObject(ctx, "stat", ref)
	if err != nil {
		return nil, nil, err
	}
	if err = obj.Body.Close(); err != nil {
		return nil, nil, fmt.Errorf("%s: stat %q: %w", s.Name(), ref.Key, err)
	}
	if err = check(&obj.ObjectInfo); err != nil {
		return nil, nil, err
	}
	return &obj.ObjectInfo, e, nil
}

// readObjectTags returns the tags of the object info describes, which was
// written with the tags written, and whose tag file is name: the ones its
// tag file holds when that was stored for this object, which a
// PutObjectTagging or DeleteObjectTagging since its write wrote, and the
// ones it was written with otherwise. A tag file stored for another object
// at its key is ignored, so whether a write removed its predecessor's yet
// does not change what is read. An object written before tags were stored
// with it has none of its own, and its tag file, if any, is the one its
// write stored after it.
func (s *Service) readObjectTags(
	name string,
	info *contracts.ObjectInfo,
	written []contracts.Tag,
) ([]contracts.Tag, error) {
	raw, err := s.root.ReadFile(name)
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

// PutObjectTagging stores tags as the tags of the version ref names as it
// now is, replacing the ones it has, or none when there are none, if check
// passes for it, under the key's lock, so the version they are stored for
// is not written over meanwhile, and returns the version's ID. ctx must be
// pinned to the bucket.
func (s *Service) PutObjectTagging(
	ctx context.Context,
	ref contracts.ObjectRef,
	tags []contracts.Tag,
	check contracts.ObjectCheck,
) (string, error) {
	if _, err := requirePin(ctx, ref.Bucket); err != nil {
		return "", err
	}
	unlock := s.locks.lock(ref.Bucket, ref.Key)
	defer unlock()
	info, e, err := s.statChecked(ctx, ref, check)
	if err != nil {
		return "", err
	}
	if err = s.writeTagFile(sideFile(tagDir, ref.Bucket, ref.Key, e), info, tags); err != nil {
		return "", err
	}
	return info.VersionID, nil
}

// writeTagFile stores tags as the tags of the object info describes, in
// its tag file, name, which overrides the tags it was written with; an
// empty tag set is stored too, so those do not apply again. The caller
// holds the key's lock.
func (s *Service) writeTagFile(
	name string,
	info *contracts.ObjectInfo,
	tags []contracts.Tag,
) error {
	raw, err := json.Marshal(objectTags{
		LastModified: info.LastModified, ETag: info.ETag, Tags: tags,
	})
	if err != nil {
		return fmt.Errorf("%s: failed to encode the tags of %q: %w", s.Name(), info.Key, err)
	}
	if err = s.writeFile(name, raw); err != nil {
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
