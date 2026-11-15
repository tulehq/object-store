package dirimpl

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"tule.dev/object-store/internal/contracts"
)

// fakeClock is a clock a test sets. Its After never fires.
type fakeClock struct {
	now time.Time
	mu  sync.Mutex
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (*fakeClock) After(time.Duration) <-chan time.Time {
	return make(chan time.Time)
}

// set makes the clock read t.
func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// clockStart is when the fixtures' fake clocks start: the example of the S3
// User Guide, "Lifecycle configuration elements".
var clockStart = time.Date(2014, 1, 15, 10, 30, 0, 0, time.UTC)

// newClockFixture is newFixture on a service stamping with a fake clock set
// to clockStart.
func newClockFixture(t *testing.T) (*fixture, *fakeClock) {
	t.Helper()
	f := newFixture(t)
	clk := &fakeClock{now: clockStart}
	f.s.clock = clk
	return f, clk
}

// hoursIn returns the time n hours after clockStart.
func hoursIn(n int) time.Time {
	return clockStart.Add(time.Duration(n) * time.Hour)
}

// manifest returns the manifest of "k", failing when it has none.
func (f *fixture) manifest() *versionManifest {
	f.t.Helper()
	m, _, err := f.s.readVersions("bucket", "k")
	if err != nil || m == nil {
		f.t.Fatalf("manifest of k: %v, %v", m, err)
	}
	return m
}

// requireSince fails unless the entries of "k" were recorded noncurrent
// since want, newest first, a zero time for none recorded.
func (f *fixture) requireSince(want ...time.Time) {
	f.t.Helper()
	m := f.manifest()
	if len(m.Versions) != len(want) {
		f.t.Fatalf("versions of k: %v, want %d", f.ids("k"), len(want))
	}
	for i, e := range m.Versions {
		if !e.NoncurrentSince.Equal(want[i]) {
			f.t.Fatalf("entry %d (%s) noncurrent since %v, want %v",
				i, e.ID, e.NoncurrentSince, want[i])
		}
	}
}

// A version becomes noncurrent when the version or marker placed above it
// is created, and is current again, and not noncurrent, when what was
// above it is deleted.
func TestNoncurrentSinceEnabled(t *testing.T) {
	f, clk := newClockFixture(t)
	f.state(contracts.VersioningEnabled)
	f.put("k", "v1")
	clk.set(hoursIn(1))
	v2 := f.put("k", "v2")
	f.requireSince(time.Time{}, hoursIn(1))

	clk.set(hoursIn(2))
	marker := f.del(oref("bucket", "k"))
	f.requireSince(time.Time{}, hoursIn(2), hoursIn(1))

	// Deleting the current marker makes v2 current again.
	f.del(vref("k", marker.VersionID))
	f.requireSince(time.Time{}, hoursIn(1))

	// Deleting a version between two keeps the time the one below it
	// became noncurrent.
	clk.set(hoursIn(3))
	f.put("k", "v3")
	f.del(vref("k", v2.VersionID))
	f.requireSince(time.Time{}, hoursIn(1))
}

// A Suspended write replaces the null version: a current null leaves the
// versions below it as they were, and a noncurrent null goes with the
// current version becoming noncurrent.
func TestNoncurrentSinceSuspended(t *testing.T) {
	f, clk := newClockFixture(t)
	f.state(contracts.VersioningEnabled)
	f.put("k", "v1")
	f.state(contracts.VersioningSuspended)
	clk.set(hoursIn(1))
	f.put("k", "null")
	f.requireSince(time.Time{}, hoursIn(1))
	clk.set(hoursIn(2))
	f.put("k", "null again")
	f.requireSince(time.Time{}, hoursIn(1))

	g, gclk := newClockFixture(t)
	g.put("k", "legacy")
	g.state(contracts.VersioningEnabled)
	gclk.set(hoursIn(1))
	v2 := g.put("k", "v2")
	g.requireIDs(v2.VersionID, contracts.VersionNull)
	g.requireSince(time.Time{}, hoursIn(1))
	g.state(contracts.VersioningSuspended)
	gclk.set(hoursIn(2))
	g.put("k", "null")
	g.requireIDs(contracts.VersionNull, v2.VersionID)
	g.requireSince(time.Time{}, hoursIn(2))
}

// stripNoncurrent rewrites the manifest of "k" as a version that did not
// record noncurrentSince wrote it.
func (f *fixture) stripNoncurrent() {
	f.t.Helper()
	m := f.manifest()
	for i := range m.Versions {
		m.Versions[i].NoncurrentSince = time.Time{}
	}
	if err := f.s.writeVersions("bucket", m); err != nil {
		f.t.Fatal(err)
	}
	_, raw, err := f.s.readVersions("bucket", "k")
	if err != nil {
		f.t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("noncurrentSince")) {
		f.t.Fatalf("a manifest without the times records them: %s", raw)
	}
}

// A manifest written before noncurrentSince was recorded dates each
// noncurrent version from its successor, and a delete between two records
// the times first, so the one below keeps its successor's.
func TestNoncurrentSinceFallback(t *testing.T) {
	f, clk := newClockFixture(t)
	f.state(contracts.VersioningEnabled)
	f.put("k", "v1")
	clk.set(hoursIn(1))
	v2 := f.put("k", "v2")
	clk.set(hoursIn(2))
	f.put("k", "v3")
	f.stripNoncurrent()

	m := f.manifest()
	for i, want := range []time.Time{{}, hoursIn(2), hoursIn(1)} {
		if got := noncurrentSince(m, i); !got.Equal(want) {
			t.Fatalf("entry %d: noncurrent since %v, want %v", i, got, want)
		}
	}
	f.del(vref("k", v2.VersionID))
	f.requireSince(time.Time{}, hoursIn(1))
}

// What the store stamps, an object's LastModified, a marker's and an
// upload's initiation, is its clock's time.
func TestStoredTimesFollowTheClock(t *testing.T) {
	f, clk := newClockFixture(t)
	if got := f.put("k", "legacy").LastModified; !got.Equal(clockStart) {
		t.Fatalf("legacy write: %v, want %v", got, clockStart)
	}
	f.state(contracts.VersioningEnabled)
	clk.set(hoursIn(1))
	v1 := f.put("k", "v1")
	if _, info := f.read(vref("k", v1.VersionID)); !info.LastModified.Equal(hoursIn(1)) {
		t.Fatalf("version: %v, want %v", info.LastModified, hoursIn(1))
	}
	clk.set(hoursIn(2))
	f.del(oref("bucket", "k"))
	if got := f.versions("k")[0]; !got.Marker || !got.LastModified.Equal(hoursIn(2)) {
		t.Fatalf("marker: %+v, want one of %v", got, hoursIn(2))
	}
	clk.set(hoursIn(3))
	f.upload("up", "part")
	uploads, err := f.s.ListMultipartUploads(f.ctx, "bucket")
	if err != nil || len(uploads) != 1 || !uploads[0].Initiated.Equal(hoursIn(3)) {
		t.Fatalf("uploads: %+v, %v; want one initiated %v", uploads, err, hoursIn(3))
	}
}

// IsLatest tells whether the version read or written is its key's current
// one.
func TestIsLatest(t *testing.T) {
	f := newFixture(t)
	if !f.put("k", "legacy").IsLatest {
		t.Fatal("an Unversioned write is not the latest")
	}
	requireLatest(f, oref("bucket", "k"), true)
	f.state(contracts.VersioningEnabled)
	requireLatest(f, vref("k", contracts.VersionNull), true)

	v1 := f.put("k", "v1")
	if !v1.IsLatest {
		t.Fatal("a new version is not the latest")
	}
	requireLatest(f, oref("bucket", "k"), true)
	requireLatest(f, vref("k", v1.VersionID), true)
	requireLatest(f, vref("k", contracts.VersionNull), false)
	f.put("k", "v2")
	requireLatest(f, vref("k", v1.VersionID), false)
	info, err := f.s.StatObject(f.ctx, vref("k", v1.VersionID))
	if err != nil || info.IsLatest {
		t.Fatalf("stat of a noncurrent version: %+v, %v", info, err)
	}
}

// requireLatest fails unless ref reads with IsLatest latest.
func requireLatest(f *fixture, ref contracts.ObjectRef, latest bool) {
	f.t.Helper()
	if _, info := f.read(ref); info.IsLatest != latest {
		f.t.Fatalf("%+v: IsLatest %v, want %v", ref, info.IsLatest, latest)
	}
}

// tryLock takes a free key's lock, never waits for a held one, and leaves
// no entry behind.
func TestKeyLocksTryLock(t *testing.T) {
	var l keyLocks
	unlock, ok := l.tryLock("b", "k")
	if !ok {
		t.Fatal("a free lock was not taken")
	}
	if _, held := l.tryLock("b", "k"); held {
		t.Fatal("a held lock was taken")
	}
	other, ok := l.tryLock("b", "other")
	if !ok {
		t.Fatal("another key's lock was not taken")
	}
	other()
	unlock()
	again, ok := l.tryLock("b", "k")
	if !ok {
		t.Fatal("a released lock was not taken")
	}
	again()
	if len(l.m) != 0 {
		t.Fatalf("locks left: %v", l.m)
	}
}

// A Suspended write that replaces a noncurrent null version of a manifest
// written before the times were recorded records them first, so the
// version below the null keeps the null as its successor.
func TestNoncurrentSinceFallbackOnASuspendedOverwrite(t *testing.T) {
	f, clk := newClockFixture(t)
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	f.state(contracts.VersioningSuspended)
	clk.set(hoursIn(1))
	f.put("k", "null")
	f.state(contracts.VersioningEnabled)
	clk.set(hoursIn(2))
	v3 := f.put("k", "v3")
	f.requireIDs(v3.VersionID, contracts.VersionNull, v1.VersionID)
	f.stripNoncurrent()

	f.state(contracts.VersioningSuspended)
	clk.set(hoursIn(3))
	f.put("k", "null again")
	f.requireIDs(contracts.VersionNull, v3.VersionID, v1.VersionID)
	f.requireSince(time.Time{}, hoursIn(3), hoursIn(1))
}

// A service needs a clock, which it is refused without when it is made.
func TestNewServiceRefusesANilClock(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a service was made without a clock")
		}
	}()
	NewService(&Settings{RootDir: t.TempDir()}, nil, Events{})
}
