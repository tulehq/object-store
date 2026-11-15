package dirimpl

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

var errCrash = errors.New("crash")

// crashAt makes f's service fail once at stage, as a crash there would
// stop it.
func crashAt(f *fixture, stage string) {
	f.s.hook = func(at string) error {
		if at != stage {
			return nil
		}
		f.s.hook = nil
		return errCrash
	}
}

// aclFiles returns the names of the ACL files of "bucket".
func (f *fixture) aclFiles() []string {
	f.t.Helper()
	entries, err := fs.ReadDir(f.s.root.FS(), filepath.Join(aclDir, "bucket"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// A write that stops before its manifest is committed leaves no version,
// and no data or ACL of its own.
func TestACrashBeforeTheCommitLeavesNothing(t *testing.T) {
	for name, write := range writeOps {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.put("src", "new")
			f.state(contracts.VersioningEnabled)
			v1 := f.put("k", "v1")
			objects, acls := f.backendObjects(), f.aclFiles()
			crashAt(f, stageWritten)
			_, err := write(f)
			requireIs(t, "the write", err, errCrash)
			f.requireIDs(v1.VersionID)
			if n := f.backendObjects(); n != objects {
				t.Fatalf("backend objects: %d, want %d", n, objects)
			}
			if got := f.aclFiles(); len(got) != len(acls) {
				t.Fatalf("ACL files %v, want %v", got, acls)
			}
			if f.body(oref("bucket", "k")) != "v1" {
				t.Fatal("the current version changed")
			}
		})
	}
}

// A first write of a key that stops before its commit does not leave the
// key indexed.
func TestACrashBeforeTheFirstCommitUnindexes(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	crashAt(f, stageWritten)
	_, err := writeOps["put"](f)
	requireIs(t, "put", err, errCrash)
	if f.backendObjects() != 0 || len(f.aclFiles()) != 0 || f.hasManifest() ||
		f.exists(indexEntry("bucket", "k")) {
		t.Fatal("the stopped write left something")
	}
}

// A change that stops after its commit, before the data it displaced is
// removed, reads as committed; the garbage it leaves is invisible, and
// DeleteBucket removes it.
func TestACrashAfterTheCommitReadsCommitted(t *testing.T) {
	f := newFixture(t)
	f.putIn("k", "legacy", contracts.PutObjectInput{ACL: []byte("<legacy/>")})
	f.state(contracts.VersioningSuspended)
	f.del(oref("bucket", "k")) // the legacy null version becomes a null marker
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	f.state(contracts.VersioningSuspended)
	f.put("k", "null")

	crashAt(f, stageCommitted)
	_, err := writeOps["put"](f)
	requireIs(t, "the stopped overwrite", err, errCrash)
	f.requireIDs(contracts.VersionNull, v1.VersionID)
	if got := f.body(vref("k", contracts.VersionNull)); got != "new" {
		t.Fatalf("null version: %q", got)
	}
	if n := f.backendObjects(); n != 3 {
		t.Fatalf("backend objects: %d, want v1, the new null and the old one's garbage", n)
	}

	for _, id := range f.ids("k") {
		f.del(vref("k", id))
	}
	if err = f.deleteBucket(); err != nil {
		t.Fatal(err)
	}
	if n := f.backendObjects(); n != 0 {
		t.Fatalf("DeleteBucket left %d backend objects", n)
	}
}

// Deleting a key's last version commits an empty manifest first, so a
// crash after it leaves the key with no versions, even with its object at
// keyHash left.
func TestACrashDeletingTheLastVersion(t *testing.T) {
	f := newFixture(t)
	f.put("k", "legacy")
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	f.del(vref("k", v1.VersionID))
	f.requireIDs(contracts.VersionNull)

	crashAt(f, stageCommitted)
	_, err := f.s.DeleteObject(f.ctx, vref("k", contracts.VersionNull))
	requireIs(t, "the stopped delete", err, errCrash)
	if !f.hasManifest() || !f.hasData(nil) {
		t.Fatal("the delete went past its commit")
	}
	f.requireIDs()
	_, err = f.s.GetObject(f.ctx, oref("bucket", "k"))
	requireIs(t, "get", err, storage.ErrObjectNotFound)

	// The next change of the key starts from the empty manifest.
	v2 := f.put("k", "v2")
	f.requireIDs(v2.VersionID)
	f.del(vref("k", v2.VersionID))
	if f.hasManifest() || f.hasData(nil) || f.exists(indexEntry("bucket", "k")) {
		t.Fatal("the key outlives its last version")
	}
}
