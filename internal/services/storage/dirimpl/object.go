package dirimpl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"time"
	"unicode/utf8"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// objectInput addresses key in bucket in the backend. It returns an error
// wrapping storage.ErrInvalidKey when key breaks the key rules.
//
// The backend refuses keys that S3 and this service accept, such as one
// holding a tab, so it is given the key's hash rather than the key, and the
// key is checked against this service's own rules: 1 to 1024 bytes of
// UTF-8 whose last "/"-separated element is not "." and that does not
// clean to ".". The key index maps the hashes back to the keys.
func objectInput(bucket, key string) (storage.BaseObjectInput, error) {
	if len(key) < 1 || len(key) > 1024 || !utf8.ValidString(key) ||
		path.Base(key) == "." || path.Clean(key) == "." {
		return storage.BaseObjectInput{}, storage.ErrInvalidKey
	}
	return storage.BaseObjectInput{Bucket: &bucket, Key: keyHash(key)}, nil
}

// objectInfo returns the stored object info, with the ETag, system headers
// and part sizes split out of the trailer's metadata. size is the length of
// the object bytes.
func objectInfo(key string, tr *trailer, size int64) (*contracts.ObjectInfo, error) {
	meta := tr.Meta
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
		Size:         size,
		ContentType:  tr.ContentType,
		LastModified: tr.LastModified,
		Metadata:     meta,
	}, nil
}

// StatObject returns the metadata of the version ref names, the current
// one when ref.Version is "", with its ETag split out of the user metadata
// (see resolve). The metadata is in the object's trailer, so this opens the
// object to read it. ctx must be pinned to the bucket.
func (s *Service) StatObject(
	ctx context.Context,
	ref contracts.ObjectRef,
) (*contracts.ObjectInfo, error) {
	if _, err := requirePin(ctx, ref.Bucket); err != nil {
		return nil, err
	}
	return s.statObject(ctx, ref)
}

// statObject is StatObject without the pin, for DeleteBucket, which holds
// the bucket's write lock instead.
func (s *Service) statObject(
	ctx context.Context,
	ref contracts.ObjectRef,
) (*contracts.ObjectInfo, error) {
	r, err := s.resolve(ctx, "stat", ref)
	if err != nil {
		return nil, err
	}
	if err = r.obj.Body.Close(); err != nil {
		return nil, fmt.Errorf("%s: stat %q: %w", s.Name(), ref.Key, err)
	}
	return &r.obj.ObjectInfo, nil
}

// GetObject opens the version ref names for reading, the current one when
// ref.Version is "", with its tags. The caller must close its Body. The
// info and body come from the same read, so they match even when the
// object is overwritten meanwhile, and the tags are those stored for that
// version (see readObjectTags). ctx must be pinned to the bucket.
func (s *Service) GetObject(
	ctx context.Context,
	ref contracts.ObjectRef,
) (*contracts.Object, error) {
	if _, err := requirePin(ctx, ref.Bucket); err != nil {
		return nil, err
	}
	obj, _, err := s.getObject(ctx, "get", ref)
	return obj, err
}

// getObject is GetObject without the pin check, for StatObjectTagging,
// which may hold the bucket's lock instead, and returns the version's
// entry with it, nil for a key without a manifest. op names the operation
// in errors.
func (s *Service) getObject(
	ctx context.Context,
	op string,
	ref contracts.ObjectRef,
) (*contracts.Object, *versionEntry, error) {
	r, err := s.resolve(ctx, op, ref)
	if err != nil {
		return nil, nil, err
	}
	if r.obj.Tags, err = s.readObjectTags(
		sideFile(tagDir, ref.Bucket, ref.Key, r.entry), &r.obj.ObjectInfo, r.written,
	); err != nil {
		return nil, nil, errors.Join(err, r.obj.Body.Close())
	}
	return r.obj, r.entry, nil
}

// openData opens the data of e, a version of key in bucket (see
// dataInput), and reads its trailer, returning it with the tags its
// trailer holds, the ones it was written with, which leave the info's
// Tags nil. Its VersionID is left for the caller to set. A missing object
// is an error wrapping storage.ErrObjectNotFound. key is valid. op names
// the operation in errors.
func (s *Service) openData(
	ctx context.Context,
	op, bucket, key string,
	e *versionEntry,
) (*contracts.Object, []contracts.Tag, error) {
	obj, err := s.dir.GetObject(ctx, &storage.GetObjectInput{
		HeadObjectInput: storage.HeadObjectInput{BaseObjectInput: dataInput(bucket, key, e)},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %s %q: %w", s.Name(), op, key, err)
	}

	// The dir backend's body is an io.ReadSeeker, which the trailer is
	// read through.
	rs, ok := obj.Body.(io.ReadSeekCloser)
	if !ok {
		err = fmt.Errorf("%s: %s %q: backend body %T is not seekable",
			s.Name(), op, key, obj.Body)
		return nil, nil, errors.Join(err, obj.Body.Close())
	}

	tr, size, err := readTrailer(rs, obj.Size)
	if err != nil {
		err = fmt.Errorf("%s: %s %q: %w", s.Name(), op, key, err)
		return nil, nil, errors.Join(err, obj.Body.Close())
	}

	info, err := objectInfo(key, tr, size)
	if err != nil {
		err = fmt.Errorf("%s: %s %q: %w", s.Name(), op, key, err)
		return nil, nil, errors.Join(err, obj.Body.Close())
	}

	return &contracts.Object{ObjectInfo: *info, Body: &objectBody{rs: rs, size: size}},
		tr.Tags, nil
}

// DeleteObject deletes the version ref names, or without a version, the
// object: in place in an Unversioned bucket, and by adding a delete marker
// otherwise (see deleteCurrent). Deleting a key or version that does not
// exist succeeds. ctx must be pinned to the bucket.
func (s *Service) DeleteObject(
	ctx context.Context,
	ref contracts.ObjectRef,
) (*contracts.DeleteObjectOutput, error) {
	if _, err := requirePin(ctx, ref.Bucket); err != nil {
		return nil, err
	}
	if err := s.checkRef(ctx, "delete", ref); err != nil {
		return nil, err
	}
	unlock := s.locks.lock(ref.Bucket, ref.Key)
	defer unlock()
	return s.deleteLocked(ctx, ref)
}

// deleteLocked is DeleteObject under the key's lock.
func (s *Service) deleteLocked(
	ctx context.Context,
	ref contracts.ObjectRef,
) (*contracts.DeleteObjectOutput, error) {
	st, err := s.versioning(ref.Bucket)
	if err != nil {
		return nil, err
	}
	m, _, err := s.readVersions(ref.Bucket, ref.Key)
	if err != nil {
		return nil, err
	}
	unversioned := st.Status == contracts.Unversioned
	switch {
	case m != nil && unversioned:
		return nil, fmt.Errorf("%s: %q: %w", s.Name(), ref.Key, errUnversionedManifest)
	case m != nil && ref.Version != "":
		return s.deleteVersion(ctx, ref.Bucket, m, ref.Version)
	case m != nil || !unversioned && ref.Version == "":
		return s.deleteCurrent(ctx, ref.Bucket, ref.Key, s.now())
	}
	return s.deleteUnlisted(ctx, ref, unversioned)
}

// deleteUnlisted deletes the version ref names of a key without a
// manifest, whose one version is the null version, in a bucket that is
// Unversioned, or for a ref with a version. It causes an event only when
// there was an object to delete. The caller holds the key's lock.
func (s *Service) deleteUnlisted(
	ctx context.Context,
	ref contracts.ObjectRef,
	unversioned bool,
) (*contracts.DeleteObjectOutput, error) {
	out := &contracts.DeleteObjectOutput{}
	if !unversioned {
		out.VersionID = ref.Version
	}
	if ref.Version != "" && ref.Version != contracts.VersionNull {
		return out, nil
	}
	p, err := s.event(ctx, contracts.BucketEvent{
		Name: contracts.EventObjectRemovedDelete, Bucket: ref.Bucket, Key: ref.Key,
		Time: s.now(), VersionID: out.VersionID,
	}, func(ev *contracts.BucketEvent) (witness, bool, error) {
		e, lErr := s.legacyEntry(ctx, ref.Bucket, ref.Key)
		if lErr != nil || e == nil {
			return witness{}, false, lErr
		}
		ev.Sequence = s.nextSeq(e.Seq)
		return witness{Kind: witnessGone}, true, nil
	})
	if err != nil {
		return nil, err
	}
	if err = s.deleteLegacy(ctx, ref.Bucket, ref.Key); err != nil {
		return out, errors.Join(err, p.settle(ctx))
	}
	return out, s.committed(p, nil)
}

// checkRef returns storage.ErrBucketNotFound when there is no bucket
// ref.Bucket, an error wrapping storage.ErrInvalidKey for a key that
// breaks the key rules, and one wrapping contracts.ErrInvalidVersionID for
// a version ID this package does not issue. op names the operation in
// errors.
func (s *Service) checkRef(ctx context.Context, op string, ref contracts.ObjectRef) error {
	if _, err := s.HeadBucket(ctx, ref.Bucket); err != nil {
		return err
	}
	if _, err := objectInput(ref.Bucket, ref.Key); err != nil {
		return fmt.Errorf("%s: %s %q: %w", s.Name(), op, ref.Key, err)
	}
	if err := checkVersionID(ref.Version); err != nil {
		return fmt.Errorf("%s: %s %q: %w", s.Name(), op, ref.Key, err)
	}
	return nil
}

// deleteLegacy removes the object at key, a key without a manifest, its
// ACL and tag files, and its index entry. The caller holds the key's lock.
func (s *Service) deleteLegacy(ctx context.Context, bucket, key string) error {
	if err := s.dir.DeleteObject(
		ctx,
		&storage.DeleteObjectInput{BaseObjectInput: dataInput(bucket, key, nil)},
	); err != nil {
		return fmt.Errorf("%s: delete %q: %w", s.Name(), key, err)
	}
	// The object is gone, so its index entry goes whether or not its ACL
	// and tag files could be removed.
	var aclErr, tagErr error
	if rmErr := s.removeIfExists(aclFile(bucket, key)); rmErr != nil {
		aclErr = fmt.Errorf("%s: delete the ACL of %q: %w", s.Name(), key, rmErr)
	}
	if rmErr := s.removeIfExists(tagFile(bucket, key)); rmErr != nil {
		tagErr = fmt.Errorf("%s: delete the tags of %q: %w", s.Name(), key, rmErr)
	}
	return errors.Join(aclErr, tagErr, s.removeFromIndex(bucket, key))
}

// PutObject stores body with the content type, user metadata and system
// headers in in, and in's ACL and tags, if any, as its ACL and tags, and
// returns the stored object's info, with its VersionID. The tags are
// written in the object's trailer, so the object is never visible without
// them. The write only happens if cond holds. In an Unversioned bucket,
// and in a Suspended one for a key without a manifest, the object is
// written in place, replacing or removing the ACL and tags of the one it
// replaces (see dropTagFile); otherwise it is a new version (see
// writeVersion). ctx must be pinned to bucket.
func (s *Service) PutObject(
	ctx context.Context,
	bucket, key string,
	body io.Reader,
	in contracts.PutObjectInput,
	cond contracts.Precondition,
) (*contracts.ObjectInfo, error) {
	return s.putObjectAs(ctx, bucket, key, body, in, cond, contracts.EventObjectCreatedPut)
}

// putObjectAs is PutObject for a write that causes event once committed.
func (s *Service) putObjectAs(
	ctx context.Context,
	bucket, key string,
	body io.Reader,
	in contracts.PutObjectInput,
	cond contracts.Precondition,
	event contracts.EventName,
) (*contracts.ObjectInfo, error) {
	if _, err := requirePin(ctx, bucket); err != nil {
		return nil, err
	}
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return nil, err
	}
	if err := s.checkKey(key); err != nil {
		return nil, err
	}

	return s.writeObject(ctx, bucket, key, &objectWrite{
		open: func() (io.Reader, *trailer, func() error, error) {
			er := newETagReader(body, joinMetadata(in.Metadata, in.Headers))
			return er, &trailer{Meta: er.meta, ContentType: in.ContentType, Tags: in.Tags},
				func() error { return nil }, nil
		},
		event: event,
		acl:   in.ACL,
		cond:  cond,
	})
}

// putObject indexes key and writes the object, with tr as its trailer,
// whose metadata, content type and tags the caller sets. An empty content
// type is detected from the body. A failure to write it, after which no
// object was written, is the error fail returns for it. The caller holds
// the key's lock.
func (s *Service) putObject(
	ctx context.Context,
	bucket, key string,
	body io.Reader,
	tr *trailer,
	fail func(error) error,
) (*contracts.ObjectInfo, error) {
	base, err := objectInput(bucket, key)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Name(), err)
	}
	if err = s.addToIndex(bucket, key); err != nil {
		return nil, err
	}

	if _, err = s.dir.PutObject(ctx, &storage.PutObjectInput{
		BaseObjectInput: base,
		Body:            withTrailer(body, tr, s.now),
	}); err != nil {
		err = fail(fmt.Errorf("%s: put %q: %w", s.Name(), key, err))
		return nil, errors.Join(err, s.unindexMissing(ctx, base, bucket, key))
	}

	return s.StatObject(ctx, contracts.ObjectRef{Bucket: bucket, Key: key})
}

// unindexMissing drops key from bucket's index when it holds no object, so a
// failed write of a new key does not leave its entry behind. base addresses
// key in the backend. The caller holds the key's lock.
func (s *Service) unindexMissing(
	ctx context.Context,
	base storage.BaseObjectInput,
	bucket, key string,
) error {
	_, err := s.dir.HeadObject(ctx, &storage.HeadObjectInput{BaseObjectInput: base})
	if !errors.Is(err, storage.ErrObjectNotFound) {
		return nil
	}
	return s.removeFromIndex(bucket, key)
}

// checkKey returns an error wrapping storage.ErrInvalidKey when key breaks
// the key rules, so a key is known to be storable before it is indexed.
func (s *Service) checkKey(key string) error {
	if _, err := objectInput("", key); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}
	return nil
}

// CopyObject copies an object version within or across buckets, if
// in.SourceCheck passes and in.SourceCondition holds of the source as it
// is read, and returns the copy, written as PutObject writes, with the
// source's version ID. Copying an object onto itself is safe: the backend
// writes the copy to a new data file, while the source stays open for
// reading. ctx must be pinned to both buckets.
func (s *Service) CopyObject(
	ctx context.Context,
	in contracts.CopyObjectInput,
) (_ *contracts.CopyObjectOutput, err error) {
	src, err := s.GetObject(ctx, contracts.ObjectRef{
		Bucket: in.SrcBucket, Key: in.SrcKey, Version: in.SrcVersion,
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		if cErr := src.Body.Close(); cErr != nil {
			err = errors.Join(err, fmt.Errorf("%s: copy %q: %w", s.Name(), in.SrcKey, cErr))
		}
	}()
	if err = checkSource(in.SourceCheck, in.SourceCondition, &src.ObjectInfo); err != nil {
		return nil, err
	}

	info, err := s.putObjectAs(ctx, in.DstBucket, in.DstKey, src.Body, copyPut(in, src),
		in.Precondition, contracts.EventObjectCreatedCopy)
	if err != nil {
		return nil, err
	}
	return &contracts.CopyObjectOutput{ObjectInfo: *info, SourceVersionID: src.VersionID}, nil
}

// copyPut returns what in's copy of src is written with.
func copyPut(in contracts.CopyObjectInput, src *contracts.Object) contracts.PutObjectInput {
	put := contracts.PutObjectInput{
		ContentType: src.ContentType,
		Metadata:    src.Metadata,
		Headers:     src.Headers,
	}
	if in.Replace != nil {
		put = *in.Replace
	}
	if len(in.Headers) > 0 {
		headers := make(map[string]string, len(put.Headers)+len(in.Headers))
		for k, v := range put.Headers {
			headers[k] = v
		}
		for k, v := range in.Headers {
			if v == "" {
				delete(headers, k)
				continue
			}
			headers[k] = v
		}
		put.Headers = headers
	}
	put.ACL = in.ACL
	put.Tags = in.Tags
	if in.CopyTags {
		put.Tags = src.Tags
	}
	return put
}

// checkSource checks info, the source of a copy as it was opened, with its
// tags: the error check returns for it, when check is set, and then
// checkSourceCondition's for cond.
func checkSource(
	check contracts.ObjectCheck,
	cond contracts.SourceCondition,
	info *contracts.ObjectInfo,
) error {
	if check != nil {
		if err := check(info); err != nil {
			return err
		}
	}
	return checkSourceCondition(cond, info)
}

// checkSourceCondition returns contracts.ErrPreconditionFailed unless cond
// holds of the source object info describes, as S3 evaluates the
// x-amz-copy-source-if-* headers (CopyObject and UploadPartCopy API
// references): x-amz-copy-source-if-match true and
// x-amz-copy-source-if-unmodified-since false copies, and
// x-amz-copy-source-if-none-match false and
// x-amz-copy-source-if-modified-since true fails, which is RFC 7232's
// order. The dates are compared to the second, as an HTTP date carries
// them.
func checkSourceCondition(cond contracts.SourceCondition, info *contracts.ObjectInfo) error {
	modified := info.LastModified.Truncate(time.Second)
	if !matchHolds(cond, info.ETag, modified) || !noneMatchHolds(cond, info.ETag, modified) {
		return contracts.ErrPreconditionFailed
	}
	return nil
}

// matchHolds reports whether cond's IfMatch, or else its
// IfUnmodifiedSince, holds of an object of etag last modified at modified.
func matchHolds(cond contracts.SourceCondition, etag string, modified time.Time) bool {
	switch {
	case cond.IfMatch != "":
		return cond.IfMatch == "*" || cond.IfMatch == etag
	case cond.IfUnmodifiedSince != nil:
		return !modified.After(*cond.IfUnmodifiedSince)
	}
	return true
}

// noneMatchHolds reports whether cond's IfNoneMatch, or else its
// IfModifiedSince, holds of an object of etag last modified at modified.
func noneMatchHolds(cond contracts.SourceCondition, etag string, modified time.Time) bool {
	switch {
	case cond.IfNoneMatch != "":
		return cond.IfNoneMatch != "*" && cond.IfNoneMatch != etag
	case cond.IfModifiedSince != nil:
		return modified.After(*cond.IfModifiedSince)
	}
	return true
}
