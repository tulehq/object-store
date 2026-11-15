package dirimpl

import (
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// loseIndexEntry removes the index entry of "k", as a power loss can drop
// an entry that was never synced.
func loseIndexEntry(f *fixture) {
	f.t.Helper()
	if err := f.s.root.Remove(indexEntry("bucket", "k")); err != nil {
		f.t.Fatal(err)
	}
}

// requireNotDeletable fails unless DeleteBucket refuses "bucket" as not
// empty and "k" then still reads as body, or as a delete marker for "".
func requireNotDeletable(t *testing.T, f *fixture, body string) {
	t.Helper()
	requireIs(t, "DeleteBucket", f.deleteBucket(), contracts.ErrBucketNotEmpty)
	ctx, _ := pin(t, f.s)
	f.ctx = ctx
	if body == "" {
		if f.versions("k")[0].Marker {
			return
		}
		t.Fatal("the marker is gone")
	}
	if got := f.body(oref("bucket", "k")); got != body {
		t.Fatalf("k: %q, want %q", got, body)
	}
}

// A bucket is not empty whatever its key index lost: an object in place,
// a version or a delete marker keeps it from being deleted, and from its
// data being purged.
func TestDeleteBucketDoesNotTrustTheIndex(t *testing.T) {
	for name, tt := range map[string]struct {
		setup func(f *fixture)
		body  string
	}{
		"object in place": {func(f *fixture) { f.put("k", "legacy") }, "legacy"},
		"version": {func(f *fixture) {
			f.state(contracts.VersioningEnabled)
			f.put("k", "v1")
		}, "v1"},
		"listed legacy null": {func(f *fixture) {
			f.put("k", "legacy")
			f.state(contracts.VersioningEnabled)
			f.put("k", "v1")
		}, "v1"},
		"marker only": {func(f *fixture) {
			f.state(contracts.VersioningEnabled)
			f.del(oref("bucket", "k"))
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			tt.setup(f)
			loseIndexEntry(f)
			requireNotDeletable(t, f, tt.body)
		})
	}
}

// An index entry whose content was lost reads as an empty key, which does
// not keep an empty bucket from being deleted.
func TestDeleteBucketPassesAnEmptyIndexEntry(t *testing.T) {
	f := newFixture(t)
	if err := f.s.root.WriteFile(filepath.Join(indexDir, "bucket", keyHash("ghost")), nil,
		0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.deleteBucket(); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
}

// Version data no manifest lists and an object at keyHash behind an empty
// manifest are garbage, which DeleteBucket purges.
func TestDeleteBucketPurgesGarbage(t *testing.T) {
	f := newFixture(t)
	f.put("k", "left")
	f.state(contracts.VersioningEnabled)
	// The data of a version a crash kept from being committed.
	if _, err := f.s.dir.PutObject(f.ctx, &storage.PutObjectInput{
		BaseObjectInput: dataInput("bucket", "orphan", &versionEntry{Token: rand.Text()}),
		Body:            withTrailer(strings.NewReader("orphan"), &trailer{}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.writeVersions("bucket",
		&versionManifest{Key: "k", Versions: []versionEntry{}}); err != nil {
		t.Fatal(err)
	}
	if err := f.deleteBucket(); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	if n := f.backendObjects(); n != 0 {
		t.Fatalf("%d backend objects left", n)
	}
}

// Setting a bucket's versioning creates the directory of its manifests.
func TestVersioningCreatesTheManifestDirectory(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningSuspended)
	if !f.exists(filepath.Join(versionsDir, "bucket")) {
		t.Fatal("no manifest directory")
	}
}
