package dirimpl

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// Concurrent writes of one key in an Enabled bucket all become versions,
// in commit order, the last committed current.
func TestConcurrentWritesAreAllKept(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	const n = 16
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			info, err := f.s.PutObject(f.ctx, "bucket", "k",
				strings.NewReader(fmt.Sprint("body-", i)), contracts.PutObjectInput{},
				contracts.Precondition{})
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = info.VersionID
		})
	}
	wg.Wait()

	versions := f.versions("k")
	got := f.ids("k")
	if len(versions) != n || !slices.IsSortedFunc(got, func(a, b string) int {
		return strings.Compare(b, a)
	}) {
		t.Fatalf("versions: %v", got)
	}
	slices.Sort(ids)
	slices.Reverse(ids)
	if !slices.Equal(got, ids) {
		t.Fatalf("versions %v, want the ones reported, %v", got, ids)
	}
	requireData(t, f, versions)
	if cur, last := f.body(oref("bucket", "k")), f.body(vref("k", got[0])); cur != last {
		t.Fatalf("current body %q, want the last committed's, %q", cur, last)
	}
}

// Concurrent writes and deletes of one key leave every write a version
// and every delete a marker.
func TestConcurrentWritesAndDeletes(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	const n = 8
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if _, err := f.s.PutObject(f.ctx, "bucket", "k", strings.NewReader("x"),
				contracts.PutObjectInput{}, contracts.Precondition{}); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if _, err := f.s.DeleteObject(f.ctx, oref("bucket", "k")); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	versions := f.versions("k")
	markers := 0
	for _, e := range versions {
		if e.Marker {
			markers++
		}
	}
	if len(versions) != 2*n || markers != n {
		t.Fatalf("%d versions, %d markers", len(versions), markers)
	}
	requireData(t, f, versions)
}

// A read whose version is deleted for good between reading the manifest
// and opening the data reads the manifest again.
func TestAReadRacingAPermanentDeleteRetries(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	f.put("k", "v1")
	v2 := f.put("k", "v2")
	f.s.hook = func(stage string) error {
		if stage != stageResolved {
			return nil
		}
		f.s.hook = nil
		_, err := f.s.DeleteObject(f.ctx, vref("k", v2.VersionID))
		return err
	}
	if got := f.body(oref("bucket", "k")); got != "v1" {
		t.Fatalf("read: %q, want v1", got)
	}

	// A version asked for by ID is gone.
	v3 := f.put("k", "v3")
	f.s.hook = func(stage string) error {
		if stage != stageResolved {
			return nil
		}
		f.s.hook = nil
		_, err := f.s.DeleteObject(f.ctx, vref("k", v3.VersionID))
		return err
	}
	_, err := f.s.GetObject(f.ctx, vref("k", v3.VersionID))
	requireIs(t, "a version deleted meanwhile", err, storage.ErrVersionNotFound)
}

// A read whose manifest keeps changing gives up, and data missing under a
// manifest that does not change is damage, never a missing object.
func TestAReadOfMissingData(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	f.put("k", "v1")
	f.s.hook = func(stage string) error {
		if stage != stageResolved {
			return nil
		}
		e := f.versions("k")[0]
		if err := f.s.removeVersion(f.ctx, "bucket", "k", &e); err != nil {
			return err
		}
		_, err := f.s.PutObject(f.ctx, "bucket", "k", strings.NewReader("next"),
			contracts.PutObjectInput{}, contracts.Precondition{})
		return err
	}
	_, err := f.s.GetObject(f.ctx, oref("bucket", "k"))
	requireIs(t, "a manifest that keeps changing", err, contracts.ErrOperationAborted)

	f.s.hook = nil
	e := f.versions("k")[0]
	if err = f.s.removeVersion(f.ctx, "bucket", "k", &e); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.GetObject(f.ctx, oref("bucket", "k"))
	requireIs(t, "missing data", err, storage.ErrBackend)
	if errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("missing data reads as a missing object: %v", err)
	}
}

// A write in place that read the bucket Unversioned, held up while the
// bucket is Enabled, is the null version the next write keeps.
func TestALegacyWriteRacingEnableKeepsTheNullVersion(t *testing.T) {
	f := newFixture(t)
	unlock := f.s.locks.lock("bucket", "k")
	done := make(chan error)
	go func() {
		_, err := f.s.PutObject(f.ctx, "bucket", "k", strings.NewReader("legacy"),
			contracts.PutObjectInput{}, contracts.Precondition{})
		done <- err
	}()
	waitForLockWaiters(t, f.s, "bucket/k", 2)
	f.state(contracts.VersioningEnabled)
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.hasManifest() {
		t.Fatal("the write in place wrote a manifest")
	}
	v1 := f.put("k", "v1")
	f.requireIDs(v1.VersionID, contracts.VersionNull)
	if got := f.body(vref("k", contracts.VersionNull)); got != "legacy" {
		t.Fatalf("null version: %q", got)
	}
}

// waitForLockWaiters waits until name's key lock has refs references: its
// holder and those waiting for it.
func waitForLockWaiters(t *testing.T, s *Service, name string, refs int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.locks.mu.Lock()
		kl := s.locks.m[name]
		n := 0
		if kl != nil {
			n = kl.refs
		}
		s.locks.mu.Unlock()
		if n >= refs {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock %s: %d references, want %d", name, n, refs)
		}
		time.Sleep(time.Millisecond)
	}
}

// Two completions of one upload write at once, and only one commits.
func TestConcurrentCompletionsOfOneUpload(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	id, parts := f.upload("k", "body")
	unlock := f.s.locks.lock("bucket", "k")
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := f.s.CompleteMultipartUpload(f.ctx, "bucket", "k", id, parts,
				contracts.Precondition{})
			errs <- err
		}()
	}
	waitForLockWaiters(t, f.s, "bucket/k", 3)
	unlock()
	first, second := <-errs, <-errs
	if (first == nil) == (second == nil) {
		t.Fatalf("completions: %v, %v; want one to succeed", first, second)
	}
	requireIs(t, "the other completion", errors.Join(first, second), contracts.ErrNoSuchUpload)
	if len(f.versions("k")) != 1 || f.backendObjects() != 1 {
		t.Fatalf("versions %v", f.ids("k"))
	}
}

// The data of a displaced legacy null version is the object at keyHash,
// so it is removed under the key's lock: removed after, it could remove
// an object a later write put there in place. A suspended overwrite (A)
// displaces the legacy null; while it is about to remove it, a delete of
// the new null (B) empties the key and a suspended write (C) writes in
// place. C's object must survive A.
func TestADisplacedLegacyNullIsCollectedUnderTheLock(t *testing.T) {
	f := newFixture(t)
	f.put("k", "legacy")
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	f.del(vref("k", v1.VersionID))
	f.state(contracts.VersioningSuspended)
	f.requireIDs(contracts.VersionNull) // the legacy null, listed

	later := make(chan error, 1)
	f.s.hook = func(stage string) error {
		if stage != stageCollecting {
			return nil
		}
		f.s.hook = nil
		go func() { // B, then C
			if _, err := f.s.DeleteObject(f.ctx, vref("k", contracts.VersionNull)); err != nil {
				later <- err
				return
			}
			later <- errOf(f.s.PutObject(f.ctx, "bucket", "k", strings.NewReader("C"),
				contracts.PutObjectInput{}, contracts.Precondition{}))
		}()
		// B and C get the key's lock only after A releases it; give them
		// the time to run first if they could.
		select {
		case err := <-later:
			later <- err
		case <-time.After(200 * time.Millisecond):
		}
		return nil
	}
	if _, err := f.s.PutObject(f.ctx, "bucket", "k", strings.NewReader("A"),
		contracts.PutObjectInput{}, contracts.Precondition{}); err != nil {
		t.Fatal(err)
	}
	if err := <-later; err != nil {
		t.Fatal(err)
	}
	if got := f.body(oref("bucket", "k")); got != "C" {
		t.Fatalf("k: %q, want C's write", got)
	}
}
