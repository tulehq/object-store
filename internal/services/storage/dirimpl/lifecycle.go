package dirimpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// What lifecycle rules do, this package does as S3 does itself: without a
// principal, an ObjectCheck or a policy. What to do to a key is decided by
// a contracts.LifecycleDecider on the key's versions as read under its
// lock, and done in one commit of its manifest, so the decision never
// applies to a view that changed meanwhile.

// lifecycleEnvelope is the part of a stored lifecycle configuration this
// package reads: the generation of the bucket it was stored for. A bucket
// deleted and created again by a version that did not remove the
// configuration keeps the file, which is then of another generation.
type lifecycleEnvelope struct {
	Generation string `json:"generation"`
}

// LifecycleTargets returns every bucket with a lifecycle configuration
// stored for its current generation, each read under the bucket's lock, so
// the configuration and generation are read together. A bucket whose
// configuration or generation cannot be read, or whose envelope does not
// decode, is logged and skipped, so it keeps no other bucket's rules from
// applying; only a failure to list the buckets fails the call.
func (s *Service) LifecycleTargets(ctx context.Context) ([]contracts.LifecycleTarget, error) {
	buckets, err := s.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	targets := make([]contracts.LifecycleTarget, 0, len(buckets))
	for _, b := range buckets {
		t, ok, tErr := s.lifecycleTarget(ctx, b.Name)
		if tErr != nil {
			s.logger.Warn("skipped a lifecycle configuration that could not be read").
				Str("bucket", b.Name).Err("error", tErr).Send()
			continue
		}
		if ok {
			targets = append(targets, t)
		}
	}
	return targets, nil
}

// lifecycleTarget returns bucket's lifecycle configuration and generation,
// and false when the bucket is gone or has no configuration stored for its
// generation.
func (s *Service) lifecycleTarget(
	ctx context.Context,
	bucket string,
) (contracts.LifecycleTarget, bool, error) {
	unlock := s.bucketLocks.rlock(bucket)
	defer unlock()
	gen, err := s.generation(ctx, bucket)
	if errors.Is(err, storage.ErrBucketNotFound) {
		// Deleted since it was listed.
		return contracts.LifecycleTarget{}, false, nil
	}
	if err != nil {
		return contracts.LifecycleTarget{}, false, err
	}
	raw, err := s.root.ReadFile(configFile(contracts.ConfigLifecycle, bucket))
	if errors.Is(err, fs.ErrNotExist) {
		return contracts.LifecycleTarget{}, false, nil
	}
	if err != nil {
		return contracts.LifecycleTarget{}, false, fmt.Errorf(
			"%s: failed to read the lifecycle configuration of bucket %s: %w",
			s.Name(), bucket, err)
	}
	var env lifecycleEnvelope
	if err = json.Unmarshal(raw, &env); err != nil {
		// Only damage leaves one.
		s.logger.Warn("skipped a damaged lifecycle configuration").
			Str("bucket", bucket).Err("error", err).Send()
		return contracts.LifecycleTarget{}, false, nil
	}
	if env.Generation != gen {
		return contracts.LifecycleTarget{}, false, nil
	}
	return contracts.LifecycleTarget{
		Ref: contracts.BucketRef{Name: bucket, Generation: gen}, Config: raw,
	}, true, nil
}

// ScanKeys returns up to n of the keys in bucket's index after cursor, in
// the order of their index entries' names, and the last of those names as
// the cursor to pass next, "" once no entry is left after it. It lists the
// index's names, and reads only the entries it returns. A key indexed or
// removed meanwhile does not move the others, so none is returned twice,
// and one indexed behind the cursor is found by the next scan. ctx must be
// pinned to bucket.
func (s *Service) ScanKeys(
	ctx context.Context,
	bucket, cursor string,
	n int,
) ([]string, string, error) {
	if err := s.checkScan(ctx, bucket, cursor, n); err != nil {
		return nil, "", err
	}
	d := filepath.Join(indexDir, bucket)
	// ReadDir returns the names sorted.
	entries, err := fs.ReadDir(s.root.FS(), d)
	if err != nil {
		return nil, "", fmt.Errorf("%s: read index of bucket %s: %w", s.Name(), bucket, err)
	}
	names, more := scanNames(entries, cursor, n)

	keys := make([]string, 0, len(names))
	for _, name := range names {
		b, rErr := s.root.ReadFile(filepath.Join(d, name))
		if errors.Is(rErr, fs.ErrNotExist) {
			// Removed by a delete while scanning.
			continue
		}
		if rErr != nil {
			return nil, "", fmt.Errorf("%s: read index of bucket %s: %w", s.Name(), bucket, rErr)
		}
		keys = append(keys, string(b))
	}
	next := ""
	if more {
		next = names[len(names)-1]
	}
	return keys, next, nil
}

// checkScan returns the error ScanKeys fails with for its arguments, nil
// when it can scan.
func (s *Service) checkScan(ctx context.Context, bucket, cursor string, n int) error {
	if _, err := requirePin(ctx, bucket); err != nil {
		return err
	}
	if n < 1 {
		return fmt.Errorf("%s: scan the keys of bucket %s: %d keys asked for", s.Name(), bucket, n)
	}
	if cursor != "" && !indexEntryRe.MatchString(cursor) {
		return fmt.Errorf("%s: scan the keys of bucket %s: invalid cursor %q",
			s.Name(), bucket, cursor)
	}
	_, err := s.HeadBucket(ctx, bucket)
	return err
}

// scanNames returns the first n index entry names of entries, sorted by
// name, after cursor, and whether another follows them.
func scanNames(entries []fs.DirEntry, cursor string, n int) ([]string, bool) {
	start, found := slices.BinarySearchFunc(entries, cursor,
		func(e fs.DirEntry, c string) int { return strings.Compare(e.Name(), c) })
	if found {
		start++
	}
	names := make([]string, 0, n)
	for _, e := range entries[start:] {
		// Anything else is a temp file of an entry being written.
		if !indexEntryRe.MatchString(e.Name()) {
			continue
		}
		if len(names) == n {
			return names, true
		}
		names = append(names, e.Name())
	}
	return names, false
}

// ApplyLifecycle applies what d decides for key, under the key's lock,
// which it only tries, so a write holding it, as one in place does while
// it reads its body, does not stall it: the key is then reported Busy. In
// an Unversioned bucket, expiring the current version deletes it as
// DeleteObject does there. Otherwise the noncurrent versions the plan
// lists are removed, the current version is expired as DeleteObject
// without a version does, with the plan's marker time, and a delete
// marker left as the key's only version is removed if d says so; all in
// one commit, after which the data of what was removed is. ctx must be
// pinned to bucket.
func (s *Service) ApplyLifecycle(
	ctx context.Context,
	bucket, key string,
	d contracts.LifecycleDecider,
) (*contracts.LifecycleOutcome, error) {
	if _, err := requirePin(ctx, bucket); err != nil {
		return nil, err
	}
	if err := s.checkRef(
		ctx,
		"lifecycle",
		contracts.ObjectRef{Bucket: bucket, Key: key},
	); err != nil {
		return nil, err
	}
	unlock, ok := s.locks.tryLock(bucket, key)
	if !ok {
		return &contracts.LifecycleOutcome{Busy: true}, nil
	}
	defer unlock()

	st, err := s.versioning(bucket)
	if err != nil {
		return nil, err
	}
	if st.Status == contracts.Unversioned {
		return s.applyUnversioned(ctx, bucket, key, d)
	}
	return s.applyVersioned(ctx, bucket, key, st.Status, d)
}

// applyUnversioned is ApplyLifecycle in an Unversioned bucket, where a key
// has one version, its object, and no manifest. The caller holds the key's
// lock.
func (s *Service) applyUnversioned(
	ctx context.Context,
	bucket, key string,
	d contracts.LifecycleDecider,
) (*contracts.LifecycleOutcome, error) {
	m, _, err := s.readVersions(bucket, key)
	if err != nil {
		return nil, err
	}
	if m != nil {
		return nil, fmt.Errorf("%s: %q: %w", s.Name(), key, errUnversionedManifest)
	}
	e, err := s.legacyEntry(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	out := &contracts.LifecycleOutcome{}
	if e == nil {
		return out, nil
	}
	plan, err := s.decide(key, d, &contracts.LifecycleKey{
		Key:        key,
		Versioning: contracts.Unversioned,
		Versions: []contracts.LifecycleVersion{
			s.lifecycleVersion(ctx, bucket, key, *e, "", time.Time{}),
		},
	})
	if err != nil {
		return nil, err
	}
	if len(plan.Remove) > 0 {
		return nil, fmt.Errorf("%s: lifecycle of %q: the plan removes noncurrent versions of "+
			"a key in an unversioned bucket", s.Name(), key)
	}
	if plan.ExpireCurrentRule == "" {
		return out, nil
	}
	if err = s.deleteLegacy(ctx, bucket, key); err != nil {
		return nil, err
	}
	if err = s.at(stageCommitted); err != nil {
		return nil, err
	}
	out.Deleted = []contracts.LifecycleDeletion{{RuleID: plan.ExpireCurrentRule}}
	return out, nil
}

// applyVersioned is ApplyLifecycle in a bucket of versioning state st, not
// Unversioned. The caller holds the key's lock.
func (s *Service) applyVersioned(
	ctx context.Context,
	bucket, key string,
	st contracts.Versioning,
	d contracts.LifecycleDecider,
) (*contracts.LifecycleOutcome, error) {
	m, _, err := s.loadVersions(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	if len(m.Versions) == 0 {
		return &contracts.LifecycleOutcome{}, nil
	}
	k := &contracts.LifecycleKey{
		Key: key, Versioning: st, Versions: make([]contracts.LifecycleVersion, len(m.Versions)),
	}
	for i, e := range m.Versions {
		k.Versions[i] = s.lifecycleVersion(ctx, bucket, key, e, e.ID, noncurrentSince(m, i))
	}
	plan, err := s.decide(key, d, k)
	if err != nil {
		return nil, err
	}
	a := &lifecycleChange{out: &contracts.LifecycleOutcome{}}
	if err = s.applyPlan(bucket, m, plan, a); err != nil {
		return nil, err
	}
	if err = s.removeSoleMarker(ctx, bucket, m, d, a); err != nil {
		return nil, err
	}
	if err = s.commitLifecycle(ctx, bucket, m, a); err != nil {
		return nil, err
	}
	return a.out, nil
}

// decide asks d for the plan for k, the versions of key, then calls the
// stageLifecycleDecided hook.
func (s *Service) decide(
	key string,
	d contracts.LifecycleDecider,
	k *contracts.LifecycleKey,
) (contracts.LifecyclePlan, error) {
	plan, err := d.Plan(k)
	if err != nil {
		return contracts.LifecyclePlan{}, fmt.Errorf("%s: lifecycle of %q: %w", s.Name(), key, err)
	}
	if err = s.at(stageLifecycleDecided); err != nil {
		return contracts.LifecyclePlan{}, err
	}
	return plan, nil
}

// lifecycleChange is what ApplyLifecycle changed in a key's manifest, not
// yet committed: the outcome, and the entries removed, whose data goes once
// the change is committed.
type lifecycleChange struct {
	out     *contracts.LifecycleOutcome
	removed []versionEntry
}

// changed reports whether there is anything to commit.
func (a *lifecycleChange) changed() bool {
	return len(a.removed) > 0 || a.out.MarkerID != ""
}

// remove records e, removed permanently by rule.
func (a *lifecycleChange) remove(e *versionEntry, rule string) {
	a.removed = append(a.removed, *e)
	a.out.Deleted = append(a.out.Deleted, contracts.LifecycleDeletion{
		VersionID: e.ID, RuleID: rule, Marker: e.Marker,
	})
}

// applyPlan applies plan to m, in memory, and records it in a: it removes
// the noncurrent versions the plan lists, then expires the current version
// as deleteCurrent does. A plan that lists a version m does not hold as
// noncurrent, or expires a current delete marker, is not one d could have
// made of m, and fails. The caller holds the key's lock.
func (s *Service) applyPlan(
	bucket string,
	m *versionManifest,
	plan contracts.LifecyclePlan,
	a *lifecycleChange,
) error {
	if len(plan.Remove) > 0 || plan.ExpireCurrentRule != "" {
		m.recordNoncurrent()
	}
	for _, id := range slices.Sorted(maps.Keys(plan.Remove)) {
		i := m.find(id)
		if i < 1 {
			return fmt.Errorf("%s: lifecycle of %q: the plan removes %s, which is not a "+
				"noncurrent version", s.Name(), m.Key, id)
		}
		a.remove(m.remove(i), plan.Remove[id])
	}
	if plan.ExpireCurrentRule == "" {
		return nil
	}
	if m.live() == nil {
		return fmt.Errorf("%s: lifecycle of %q: the plan expires a current delete marker",
			s.Name(), m.Key)
	}
	if plan.MarkerTime.IsZero() {
		return fmt.Errorf("%s: lifecycle of %q: the plan expires the current version "+
			"without a marker time", s.Name(), m.Key)
	}
	marker := versionEntry{Marker: true, LastModified: plan.MarkerTime}
	displaced, err := s.place(bucket, m, &marker)
	if err != nil {
		return err
	}
	if displaced != nil {
		a.remove(displaced, plan.ExpireCurrentRule)
	}
	a.out.MarkerID, a.out.MarkerRule = marker.ID, plan.ExpireCurrentRule
	return nil
}

// removeSoleMarker asks d whether to remove the delete marker that is the
// only version m has left, if it has, and removes it, in memory, when d
// names a rule. A marker this change added is not asked about: it stays
// until a later run, as an object gets one lifecycle action a day. The
// caller holds the key's lock.
func (s *Service) removeSoleMarker(
	ctx context.Context,
	bucket string,
	m *versionManifest,
	d contracts.LifecycleDecider,
	a *lifecycleChange,
) error {
	if len(m.Versions) != 1 || !m.Versions[0].Marker || m.Versions[0].ID == a.out.MarkerID {
		return nil
	}
	e := m.Versions[0]
	rule, err := d.RemoveSoleMarker(m.Key, s.lifecycleVersion(ctx, bucket, m.Key, e, e.ID,
		time.Time{}))
	if err != nil {
		return fmt.Errorf("%s: lifecycle of %q: %w", s.Name(), m.Key, err)
	}
	if rule != "" {
		a.remove(m.remove(0), rule)
	}
	return nil
}

// commitLifecycle commits m, changed as a records, if anything changed:
// as the key's manifest, or when it lists no version left, by removing it
// (see dropVersions). Then it removes the data of the entries removed,
// logging a failure (see warnAfterCommit). The caller holds the key's lock.
func (s *Service) commitLifecycle(
	ctx context.Context,
	bucket string,
	m *versionManifest,
	a *lifecycleChange,
) error {
	if !a.changed() {
		return nil
	}
	removed := a.removed
	if len(m.Versions) == 0 {
		last := removed[len(removed)-1]
		removed = removed[:len(removed)-1]
		if err := s.dropVersions(ctx, bucket, m.Key, &last, nil); err != nil {
			return err
		}
	} else {
		if err := s.commit(bucket, m, false); err != nil {
			return err
		}
		if err := s.at(stageCommitted); err != nil {
			return err
		}
	}
	for i := range removed {
		s.warnAfterCommit("collect a version lifecycle rules deleted", bucket, m.Key,
			s.collect(ctx, bucket, m.Key, &removed[i]))
	}
	return nil
}

// lifecycleVersion returns e, a version of key reported as id, as lifecycle
// rules see it, noncurrent since since. Its tags are read from its data
// and tag file on the first call of Tags, so only rules that filter on
// tags pay for them; a marker has none.
func (s *Service) lifecycleVersion(
	ctx context.Context,
	bucket, key string,
	e versionEntry,
	id string,
	since time.Time,
) contracts.LifecycleVersion {
	v := contracts.LifecycleVersion{
		LastModified: e.LastModified, NoncurrentSince: since, VersionID: id, Size: e.Size,
		Marker: e.Marker,
	}
	if e.Marker {
		v.Tags = func() ([]contracts.Tag, error) { return nil, nil }
		return v
	}
	var (
		read bool
		tags []contracts.Tag
		err  error
	)
	v.Tags = func() ([]contracts.Tag, error) {
		if !read {
			tags, err = s.versionTags(ctx, bucket, key, &e)
			read = true
		}
		return tags, err
	}
	return v
}

// versionTags returns the tags of e, a version of key, as GetObjectTagging
// does (see readObjectTags).
func (s *Service) versionTags(
	ctx context.Context,
	bucket, key string,
	e *versionEntry,
) ([]contracts.Tag, error) {
	obj, written, err := s.openData(ctx, "lifecycle", bucket, key, e)
	if err != nil {
		return nil, err
	}
	if err = obj.Body.Close(); err != nil {
		return nil, fmt.Errorf("%s: lifecycle %q: %w", s.Name(), key, err)
	}
	return s.readObjectTags(sideFile(tagDir, bucket, key, e), &obj.ObjectInfo, written)
}

// AbortLifecycleUpload aborts upload uploadID of key, as
// AbortMultipartUpload does, under the key's lock, so it does not
// interleave with a completion in place, which reads the parts under it,
// if it still is the upload initiated at initiated. It only tries the
// lock, as ApplyLifecycle does, so a write holding it does not stall it:
// the upload is then reported Busy. Aborted is set also with an error when
// the upload is gone but its parts could not all be removed. ctx must be
// pinned to bucket.
func (s *Service) AbortLifecycleUpload(
	ctx context.Context,
	bucket, key, uploadID string,
	initiated time.Time,
) (*contracts.LifecycleAbort, error) {
	if _, err := requirePin(ctx, bucket); err != nil {
		return nil, err
	}
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	unlock, ok := s.locks.tryLock(bucket, key)
	if !ok {
		return &contracts.LifecycleAbort{Busy: true}, nil
	}
	defer unlock()

	out := &contracts.LifecycleAbort{}
	m, err := s.loadUpload(ctx, bucket, key, uploadID)
	if errors.Is(err, contracts.ErrNoSuchUpload) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if !m.Initiated.Equal(initiated) {
		return out, nil
	}
	out.Aborted, err = s.removeUpload(uploadID)
	if err != nil {
		return out, fmt.Errorf("%s: abort upload %s: %w", s.Name(), uploadID, err)
	}
	return out, nil
}
