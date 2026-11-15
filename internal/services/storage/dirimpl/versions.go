package dirimpl

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"time"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// The versions of a key in a bucket that is not Unversioned are listed in
// its version manifest, a side file in versionsDir named by keyHash, which
// is the one commit point of every change to them: each change writes the
// whole manifest to a temp file and renames it into place, under the key's
// lock. Each version's data is a backend object of its own under
// "<keyHash>/<token>", where token is drawn when the data is written, so
// writing a version never writes in place, and its ACL and tag files are
// "<keyHash>.<token>" (see aclFile and tagFile).
//
// A key without a manifest is a legacy key: the backend object at keyHash,
// written as an unversioned bucket writes it, is its one version, the null
// version. Its first change in a versioned bucket writes a manifest that
// lists it as a Legacy entry, its data and side files left in place. Once
// a key has a manifest, the backend object at keyHash is visible only
// through such an entry, and is garbage otherwise.
//
// Data is written before the manifest that lists it, and deleted after the
// manifest that no longer does, so a crash leaves at most garbage that no
// manifest lists, which DeleteBucket removes (see purgeObjects).

// versionManifest is what is stored in a key's version manifest.
type versionManifest struct {
	// Key is the key, checked on read against the hash the manifest is
	// named by.
	Key string `json:"key"`
	// Versions are the key's versions and delete markers, newest first,
	// so the first is the current one. A manifest without any lists no
	// versions: it never falls back to the legacy object, so one a crash
	// left behind does not come back.
	Versions []versionEntry `json:"versions"`
}

// versionEntry is one version or delete marker of a key.
type versionEntry struct {
	// LastModified is the version's, as its trailer holds it, or when a
	// marker was created.
	LastModified time.Time `json:"lastModified"`
	// ID is VersionNull or an ID newVersionID drew. IDs are unique within
	// a manifest.
	ID string `json:"id"`
	// Token names the version's data and side files. It is empty for a
	// marker and a Legacy entry.
	Token string `json:"token,omitempty"`
	ETag  string `json:"etag,omitempty"`
	Size  int64  `json:"size"`
	// Seq is the commit sequence of the version: nextSeq's when it was
	// committed, or its LastModified in nanoseconds for a Legacy entry.
	Seq int64 `json:"seq"`
	// Legacy marks the null version whose data is the backend object at
	// keyHash and whose side files are keyHash's.
	Legacy bool `json:"legacy,omitempty"`
	// Marker marks a delete marker, which has no data or side files.
	Marker bool `json:"marker,omitempty"`
}

// resolveRetries is how many times resolve reads a version's manifest
// again when the data it lists was removed meanwhile.
const resolveRetries = 3

// Stages of a change to a key's versions that Service.hook is called at.
const (
	// stageWritten: a version's data and ACL are written, not committed.
	stageWritten = "written"
	// stageCommitted: the manifest is committed, the data it no longer
	// lists not yet removed.
	stageCommitted = "committed"
	// stageResolved: a read has read the manifest and is about to open
	// the data it lists.
	stageResolved = "resolved"
)

// at calls s.hook, when set, at stage.
func (s *Service) at(stage string) error {
	if s.hook == nil {
		return nil
	}
	return s.hook(stage)
}

// errUnversionedManifest is a manifest found in an Unversioned bucket,
// which only damage can leave.
var errUnversionedManifest = fmt.Errorf("%w: a version manifest in an unversioned bucket",
	storage.ErrBackend)

// versionsFile is the version manifest of key in bucket.
func versionsFile(bucket, key string) string {
	return filepath.Join(versionsDir, bucket, keyHash(key))
}

// dataInput addresses the data of e, a version of key in bucket, in the
// backend: the object at keyHash for a key without a manifest (nil e) or
// a Legacy entry, and "<keyHash>/<token>" otherwise. key is valid.
func dataInput(bucket, key string, e *versionEntry) storage.BaseObjectInput {
	name := keyHash(key)
	if e != nil && !e.Legacy {
		name += "/" + e.Token
	}
	return storage.BaseObjectInput{Bucket: &bucket, Key: name}
}

// sideFile is the name of the side file in dir of e, a version of key in
// bucket: keyHash for a key without a manifest (nil e) or a Legacy entry,
// and "<keyHash>.<token>" otherwise.
func sideFile(dir, bucket, key string, e *versionEntry) string {
	name := keyHash(key)
	if e != nil && !e.Legacy {
		name += "." + e.Token
	}
	return filepath.Join(dir, bucket, name)
}

// readVersions returns the manifest of key in bucket and its bytes, or nil
// ones when key has none.
func (s *Service) readVersions(bucket, key string) (*versionManifest, []byte, error) {
	raw, err := s.root.ReadFile(versionsFile(bucket, key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%s: failed to read the versions of %q: %w", s.Name(), key, err)
	}
	var m versionManifest
	if err = json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("%s: failed to decode the versions of %q: %w",
			s.Name(), key, err)
	}
	if m.Key != key {
		return nil, nil, fmt.Errorf("%s: the versions of %q are stored for %q: %w",
			s.Name(), key, m.Key, storage.ErrBackend)
	}
	return &m, raw, nil
}

// writeVersions commits m as the manifest of its key in bucket. The caller
// holds the key's lock.
func (s *Service) writeVersions(bucket string, m *versionManifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("%s: failed to encode the versions of %q: %w", s.Name(), m.Key, err)
	}
	if err = s.writeFile(versionsFile(bucket, m.Key), raw); err != nil {
		return fmt.Errorf("%s: failed to store the versions of %q: %w", s.Name(), m.Key, err)
	}
	return nil
}

// find returns the index of the entry of version id, or -1.
func (m *versionManifest) find(id string) int {
	return slices.IndexFunc(m.Versions, func(e versionEntry) bool { return e.ID == id })
}

// remove removes the entry at i and returns it.
func (m *versionManifest) remove(i int) *versionEntry {
	e := m.Versions[i]
	m.Versions = slices.Delete(m.Versions, i, i+1)
	return &e
}

// dropNull removes the null version or null marker, if there is one, and
// returns it.
func (m *versionManifest) dropNull() *versionEntry {
	if i := m.find(contracts.VersionNull); i >= 0 {
		return m.remove(i)
	}
	return nil
}

// push makes e the current version.
func (m *versionManifest) push(e versionEntry) {
	m.Versions = slices.Insert(m.Versions, 0, e)
}

// live returns the current version of m when it is not a delete marker.
func (m *versionManifest) live() *versionEntry {
	if len(m.Versions) == 0 || m.Versions[0].Marker {
		return nil
	}
	return &m.Versions[0]
}

// legacyEntry returns the entry of the null version of key, a key without
// a manifest, read from the backend object at keyHash: nil when there is
// none.
func (s *Service) legacyEntry(ctx context.Context, bucket, key string) (*versionEntry, error) {
	info, err := s.statData(ctx, "stat", bucket, key, nil)
	if errors.Is(err, storage.ErrObjectNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &versionEntry{
		ID: contracts.VersionNull, Legacy: true, ETag: info.ETag, Size: info.Size,
		LastModified: info.LastModified, Seq: info.LastModified.UnixNano(),
	}, nil
}

// loadVersions returns the manifest of key in bucket, or for a key without
// one, the manifest its first change writes: its null version, if it has
// one, as a Legacy entry. existed reports whether the key had a manifest.
// The caller holds the key's lock.
func (s *Service) loadVersions(
	ctx context.Context,
	bucket, key string,
) (m *versionManifest, existed bool, err error) {
	if m, _, err = s.readVersions(bucket, key); err != nil || m != nil {
		return m, m != nil, err
	}
	m = &versionManifest{Key: key, Versions: []versionEntry{}}
	e, err := s.legacyEntry(ctx, bucket, key)
	if err != nil {
		return nil, false, err
	}
	if e != nil {
		m.push(*e)
	}
	return m, false, nil
}

// resolved is a version a read opened: its object, with the tags its
// trailer holds, and its entry, nil for a key without a manifest.
type resolved struct {
	obj     *contracts.Object
	entry   *versionEntry
	written []contracts.Tag
}

// resolve opens the version ref names, the current one when ref.Version
// is "": storage.ErrObjectNotFound when there is none or the current one
// is a delete marker (a contracts.DeleteMarkerError), an error wrapping
// both storage.ErrVersionNotFound and storage.ErrObjectNotFound for a
// version the key does not have, a contracts.DeleteMarkerError wrapping
// contracts.ErrMethodNotAllowed for one that is a marker, and
// contracts.ErrInvalidVersionID for a version ID this package does not
// issue. It takes no lock: the manifest it reads is one committed, and
// when the data it lists was removed meanwhile, by a change committed
// after, it reads the manifest again. op names the operation in errors.
func (s *Service) resolve(
	ctx context.Context,
	op string,
	ref contracts.ObjectRef,
) (*resolved, error) {
	if err := s.checkRef(ctx, op, ref); err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		r, raw, err := s.resolveOnce(ctx, op, ref)
		if !errors.Is(err, errDataGone) {
			return r, err
		}
		if err = s.recheck(op, ref, raw, attempt); err != nil {
			return nil, err
		}
	}
}

// errDataGone is resolveOnce's error for data its manifest lists, or for
// the object of a key without one, that it did not find.
var errDataGone = errors.New("data gone")

// resolveOnce is one attempt of resolve, which returns the manifest it
// read with errDataGone, as raw bytes, nil for none.
func (s *Service) resolveOnce(
	ctx context.Context,
	op string,
	ref contracts.ObjectRef,
) (*resolved, []byte, error) {
	m, raw, err := s.readVersions(ref.Bucket, ref.Key)
	if err != nil {
		return nil, nil, err
	}
	e, err := s.pick(op, ref, m)
	if err != nil {
		return nil, nil, err
	}
	if err = s.at(stageResolved); err != nil {
		return nil, nil, err
	}
	obj, written, err := s.openData(ctx, op, ref.Bucket, ref.Key, e)
	if errors.Is(err, storage.ErrObjectNotFound) {
		return nil, raw, errDataGone
	}
	if err != nil {
		return nil, nil, err
	}
	if obj.VersionID, err = s.versionIDOf(ref.Bucket, e); err != nil {
		return nil, nil, errors.Join(err, obj.Body.Close())
	}
	return &resolved{obj: obj, entry: e, written: written}, nil, nil
}

// recheck reads the manifest of ref's key again after resolve's attempt
// found no data for the one it read, raw: nil, to try again, when it
// changed meanwhile, and otherwise storage.ErrObjectNotFound for a key
// without a manifest and without an object, and storage.ErrBackend for
// data missing from a manifest that lists it, which only damage leaves,
// since data is removed only after the manifest that lists it. A key
// whose manifest keeps changing is contracts.ErrOperationAborted.
func (s *Service) recheck(op string, ref contracts.ObjectRef, raw []byte, attempt int) error {
	_, now, err := s.readVersions(ref.Bucket, ref.Key)
	switch {
	case err != nil:
		return err
	case bytes.Equal(raw, now) && raw == nil:
		return fmt.Errorf("%s: %s %q: %w", s.Name(), op, ref.Key, storage.ErrObjectNotFound)
	case bytes.Equal(raw, now):
		return fmt.Errorf("%s: %s %q: version data missing: %w",
			s.Name(), op, ref.Key, storage.ErrBackend)
	case attempt == resolveRetries:
		return fmt.Errorf("%s: %s %q: the versions keep changing: %w",
			s.Name(), op, ref.Key, contracts.ErrOperationAborted)
	}
	return nil
}

// pick returns the entry of m that ref names, nil for the legacy object of
// a key without a manifest, with resolve's errors.
func (s *Service) pick(
	op string,
	ref contracts.ObjectRef,
	m *versionManifest,
) (*versionEntry, error) {
	if m == nil {
		if ref.Version == "" || ref.Version == contracts.VersionNull {
			return nil, nil
		}
		return nil, s.noSuchVersion(op, ref)
	}
	if ref.Version == "" {
		if len(m.Versions) == 0 {
			return nil, fmt.Errorf(
				"%s: %s %q: %w",
				s.Name(),
				op,
				ref.Key,
				storage.ErrObjectNotFound,
			)
		}
		e := &m.Versions[0]
		if e.Marker {
			return nil, &contracts.DeleteMarkerError{
				LastModified: e.LastModified, VersionID: e.ID, Current: true,
			}
		}
		return e, nil
	}
	i := m.find(ref.Version)
	if i < 0 {
		return nil, s.noSuchVersion(op, ref)
	}
	e := &m.Versions[i]
	if e.Marker {
		return nil, &contracts.DeleteMarkerError{LastModified: e.LastModified, VersionID: e.ID}
	}
	return e, nil
}

// noSuchVersion is the error for a version ref names that its key does not
// have, which is also a missing object, as gas storage reports one.
func (s *Service) noSuchVersion(op string, ref contracts.ObjectRef) error {
	return fmt.Errorf("%s: %s %q version %s: %w", s.Name(), op, ref.Key, ref.Version,
		errors.Join(storage.ErrVersionNotFound, storage.ErrObjectNotFound))
}

// versionIDOf returns the version ID a read of e reports: e's ID, and for
// a key without a manifest (nil e), VersionNull, or "" when bucket is
// Unversioned.
func (s *Service) versionIDOf(bucket string, e *versionEntry) (string, error) {
	if e != nil {
		return e.ID, nil
	}
	st, err := s.versioning(bucket)
	if err != nil {
		return "", err
	}
	if st.Status == contracts.Unversioned {
		return "", nil
	}
	return contracts.VersionNull, nil
}

// statData returns the info of the data of e, a version of key in bucket
// (see dataInput), as openData reads it.
func (s *Service) statData(
	ctx context.Context,
	op, bucket, key string,
	e *versionEntry,
) (*contracts.ObjectInfo, error) {
	obj, _, err := s.openData(ctx, op, bucket, key, e)
	if err != nil {
		return nil, err
	}
	if err = obj.Body.Close(); err != nil {
		return nil, fmt.Errorf("%s: %s %q: %w", s.Name(), op, key, err)
	}
	return &obj.ObjectInfo, nil
}

// objectWrite is a write of an object at a key.
type objectWrite struct {
	// open returns the body and trailer to write and the function that
	// closes the body once it is written. On a legacy key it is called
	// once cond holds.
	open func() (io.Reader, *trailer, func() error, error)
	// check, when set, runs under the key's lock before a new version is
	// committed, and an error it returns fails the write.
	check func() error
	// done, when set, runs under the key's lock once the object is
	// written.
	done func() error
	acl  []byte
	cond contracts.Precondition
}

// writeObject writes w as the object at key in bucket: in place, as an
// Unversioned bucket writes it, when bucket is Unversioned or Suspended
// and key has no manifest, and as a new version otherwise. ctx is pinned
// to bucket and key is valid.
func (s *Service) writeObject(
	ctx context.Context,
	bucket, key string,
	w *objectWrite,
) (*contracts.ObjectInfo, error) {
	st, err := s.versioning(bucket)
	if err != nil {
		return nil, err
	}
	legacy := st.Status == contracts.Unversioned
	if st.Status == contracts.VersioningSuspended {
		m, _, rErr := s.readVersions(bucket, key)
		if rErr != nil {
			return nil, rErr
		}
		legacy = m == nil
	}
	if legacy {
		info, written, lErr := s.writeLegacy(ctx, bucket, key, w)
		if lErr != nil || written {
			return info, lErr
		}
		// The key has a manifest by now: its write is a version.
	}
	return s.writeVersion(ctx, bucket, key, w)
}

// writeLegacy writes w in place at key, as an Unversioned bucket does, if
// key has no manifest under its lock, and reports whether it did. A
// manifest is only ever created under the lock, so the key keeps none
// until the write is done. The body is read under the lock, after cond is
// checked.
func (s *Service) writeLegacy(
	ctx context.Context,
	bucket, key string,
	w *objectWrite,
) (*contracts.ObjectInfo, bool, error) {
	unlock := s.locks.lock(bucket, key)
	defer unlock()

	if m, _, err := s.readVersions(bucket, key); err != nil || m != nil {
		return nil, false, err
	}
	if err := s.checkPrecondition(ctx, bucket, key, w.cond); err != nil {
		return nil, false, err
	}
	body, tr, closeBody, err := w.open()
	if err != nil {
		return nil, false, err
	}
	info, err := s.putObject(ctx, bucket, key, body, tr)
	if cErr := closeBody(); cErr != nil {
		err = errors.Join(err, cErr)
	}
	if err != nil {
		return nil, false, err
	}
	if err = s.finishLegacy(bucket, info, w); err != nil {
		return nil, false, err
	}
	return info, true, nil
}

// finishLegacy stores w's ACL as the one of info's object, written in
// place, or removes the one stored for the object it replaced, removes
// that object's tag file, and runs w.done. The caller holds the key's
// lock.
func (s *Service) finishLegacy(bucket string, info *contracts.ObjectInfo, w *objectWrite) error {
	if err := s.setObjectACL(bucket, info, w.acl); err != nil {
		return err
	}
	if err := s.dropTagFile(bucket, info.Key); err != nil {
		return err
	}
	if w.done != nil {
		return w.done()
	}
	return nil
}

// writeVersion writes w as a new version of key: its data and ACL under a
// fresh token, without the key's lock, so a slow body holds no lock; then,
// under the lock, it checks cond against the current version, a delete
// marker counting as none, and commits the version in the bucket's state
// as of then: a new version on top when Enabled, and when Suspended, the
// null version, which replaces the one the key had, object or marker
// (S3 User Guide, "Adding objects to versioning-suspended buckets"). What
// was written is removed when the version is not committed.
func (s *Service) writeVersion(
	ctx context.Context,
	bucket, key string,
	w *objectWrite,
) (*contracts.ObjectInfo, error) {
	e := &versionEntry{Token: rand.Text()}
	info, err := s.writeData(ctx, bucket, key, e, w)
	if err == nil {
		err = s.at(stageWritten)
	}
	committed := false
	if err == nil {
		committed, err = s.commitVersion(ctx, bucket, key, e, w)
	}
	if err != nil {
		if !committed {
			err = errors.Join(err, s.removeVersion(ctx, bucket, key, e))
		}
		return nil, err
	}
	info.Key, info.VersionID = key, e.ID
	return info, nil
}

// writeData writes the data of w, and its ACL, as those of e, a version of
// key not yet committed, and fills in e's ETag, size and modification time
// from it. It returns the data's info.
func (s *Service) writeData(
	ctx context.Context,
	bucket, key string,
	e *versionEntry,
	w *objectWrite,
) (*contracts.ObjectInfo, error) {
	body, tr, closeBody, err := w.open()
	if err != nil {
		return nil, err
	}
	_, err = s.dir.PutObject(ctx, &storage.PutObjectInput{
		BaseObjectInput: dataInput(bucket, key, e),
		Body:            withTrailer(body, tr),
	})
	if cErr := closeBody(); cErr != nil {
		err = errors.Join(err, cErr)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: put %q: %w", s.Name(), key, err)
	}
	info, err := s.statData(ctx, "put", bucket, key, e)
	if err != nil {
		return nil, err
	}
	e.ETag, e.Size, e.LastModified = info.ETag, info.Size, info.LastModified
	if w.acl != nil {
		if err = s.writeObjectACL(sideFile(aclDir, bucket, key, e), info, w.acl); err != nil {
			return nil, err
		}
	}
	return info, nil
}

// commitVersion commits e, a version of key whose data is written, under
// the key's lock, if w.cond holds of the current version and w.check
// passes, and reports whether it did, also when an error follows the
// commit.
func (s *Service) commitVersion(
	ctx context.Context,
	bucket, key string,
	e *versionEntry,
	w *objectWrite,
) (bool, error) {
	unlock := s.locks.lock(bucket, key)
	defer unlock()

	m, existed, err := s.loadVersions(ctx, bucket, key)
	if err != nil {
		return false, err
	}
	cur := m.live()
	if err = preconditionHolds(w.cond, cur != nil, etagOf(cur)); err != nil {
		return false, err
	}
	if w.check != nil {
		if err = w.check(); err != nil {
			return false, err
		}
	}
	displaced, err := s.place(bucket, m, e)
	if err != nil {
		return false, err
	}
	if err = s.commit(bucket, m, existed); err != nil {
		return false, err
	}
	if err = s.at(stageCommitted); err != nil {
		return true, err
	}
	if err = s.collect(ctx, bucket, key, displaced); err != nil {
		return true, err
	}
	if w.done != nil {
		return true, w.done()
	}
	return true, nil
}

// place gives e, a new version or marker, its ID and sequence in bucket's
// state, and makes it the current version of m: in a Suspended bucket, it
// is the null version, and the one m had is returned, removed. The caller
// holds the key's lock.
func (s *Service) place(bucket string, m *versionManifest, e *versionEntry) (*versionEntry, error) {
	st, err := s.versioning(bucket)
	if err != nil {
		return nil, err
	}
	e.Seq = s.nextSeq()
	var displaced *versionEntry
	switch st.Status {
	case contracts.VersioningEnabled:
		e.ID = newVersionID(e.Seq)
	case contracts.VersioningSuspended:
		e.ID = contracts.VersionNull
		displaced = m.dropNull()
	default:
		return nil, fmt.Errorf("%s: %q: %w", s.Name(), m.Key, errUnversionedManifest)
	}
	m.push(*e)
	return displaced, nil
}

// commit indexes m's key and commits m, after which the caller calls
// s.at(stageCommitted). When a key that had neither a
// manifest nor an object fails to commit, its index entry is dropped. The
// caller holds the key's lock.
func (s *Service) commit(bucket string, m *versionManifest, existed bool) error {
	if err := s.addToIndex(bucket, m.Key); err != nil {
		return err
	}
	err := s.writeVersions(bucket, m)
	if err != nil && !existed && len(m.Versions) == 1 {
		err = errors.Join(err, s.removeFromIndex(bucket, m.Key))
	}
	return err
}

// collect removes the data and side files of e, a version of key that the
// manifest committed no longer lists; a nil e or a marker has none. It
// runs under the key's lock: the data of a Legacy entry is the object at
// keyHash, which a write in place could write again once the key has no
// manifest.
func (s *Service) collect(ctx context.Context, bucket, key string, e *versionEntry) error {
	if e == nil || e.Marker {
		return nil
	}
	return s.removeVersion(ctx, bucket, key, e)
}

// removeVersion removes the data of e, a version of key, then its ACL and
// tag files.
func (s *Service) removeVersion(ctx context.Context, bucket, key string, e *versionEntry) error {
	var dataErr, aclErr, tagErr error
	if err := s.dir.DeleteObject(ctx, &storage.DeleteObjectInput{
		BaseObjectInput: dataInput(bucket, key, e),
	}); err != nil {
		dataErr = fmt.Errorf("%s: delete version %s of %q: %w", s.Name(), e.ID, key, err)
	}
	if err := s.removeIfExists(sideFile(aclDir, bucket, key, e)); err != nil {
		aclErr = fmt.Errorf("%s: delete the ACL of version %s of %q: %w", s.Name(), e.ID, key, err)
	}
	if err := s.removeIfExists(sideFile(tagDir, bucket, key, e)); err != nil {
		tagErr = fmt.Errorf("%s: delete the tags of version %s of %q: %w",
			s.Name(), e.ID, key, err)
	}
	return errors.Join(dataErr, aclErr, tagErr)
}

// deleteCurrent is a DeleteObject without a version of a key in a bucket
// that is not Unversioned: it adds a delete marker on top, which in a
// Suspended bucket is the null marker that replaces the null version
// (DeleteObject API reference). It adds one also for a key that has no
// version, and on top of a marker. The caller holds the key's lock.
func (s *Service) deleteCurrent(
	ctx context.Context,
	bucket, key string,
) (*contracts.DeleteObjectOutput, error) {
	m, existed, err := s.loadVersions(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	marker := versionEntry{Marker: true, LastModified: time.Now()}
	displaced, err := s.place(bucket, m, &marker)
	if err != nil {
		return nil, err
	}
	if err = s.commit(bucket, m, existed); err != nil {
		return nil, err
	}
	if err = s.at(stageCommitted); err != nil {
		return nil, err
	}
	if err = s.collect(ctx, bucket, key, displaced); err != nil {
		return nil, err
	}
	return &contracts.DeleteObjectOutput{VersionID: marker.ID, DeleteMarker: true}, nil
}

// deleteVersion permanently deletes version id of key, a key with
// manifest m, version or marker; a version it does not have is deleted
// already. When the last version goes, the key's manifest and index entry
// go too (see dropVersions). The caller holds the key's lock.
func (s *Service) deleteVersion(
	ctx context.Context,
	bucket string,
	m *versionManifest,
	id string,
) (*contracts.DeleteObjectOutput, error) {
	i := m.find(id)
	if i < 0 {
		return &contracts.DeleteObjectOutput{VersionID: id}, nil
	}
	e := m.remove(i)
	out := &contracts.DeleteObjectOutput{VersionID: e.ID, DeleteMarker: e.Marker}
	if len(m.Versions) == 0 {
		return out, s.dropVersions(ctx, bucket, m.Key, e)
	}
	if err := s.commit(bucket, m, true); err != nil {
		return nil, err
	}
	if err := s.at(stageCommitted); err != nil {
		return nil, err
	}
	return out, s.collect(ctx, bucket, m.Key, e)
}

// dropVersions removes the manifest of key, whose last version, e, was
// deleted: it commits an empty manifest, then removes the object at
// keyHash, e's data and side files, the manifest, and the key's index
// entry, in this order, so a crash leaves the empty manifest, under which
// the object at keyHash, if left, does not come back. The caller holds the
// key's lock.
func (s *Service) dropVersions(ctx context.Context, bucket, key string, e *versionEntry) error {
	if err := s.writeVersions(
		bucket,
		&versionManifest{Key: key, Versions: []versionEntry{}},
	); err != nil {
		return err
	}
	if err := s.at(stageCommitted); err != nil {
		return err
	}
	if err := s.removeVersion(ctx, bucket, key, &versionEntry{
		ID: contracts.VersionNull, Legacy: true,
	}); err != nil {
		return err
	}
	if !e.Legacy {
		if err := s.collect(ctx, bucket, key, e); err != nil {
			return err
		}
	}
	if err := s.removeIfExists(versionsFile(bucket, key)); err != nil {
		return fmt.Errorf("%s: delete the versions of %q: %w", s.Name(), key, err)
	}
	return s.removeFromIndex(bucket, key)
}

// etagOf returns e's ETag, "" for nil.
func etagOf(e *versionEntry) string {
	if e == nil {
		return ""
	}
	return e.ETag
}
