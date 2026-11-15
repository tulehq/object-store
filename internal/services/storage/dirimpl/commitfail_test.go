package dirimpl

import (
	"path/filepath"
	"strings"
	"testing"

	"tule.dev/object-store/internal/contracts"
)

// failDirWrites makes dir, in f's storage root, refuse new files until
// the returned function, or the end of the test, restores it.
func failDirWrites(f *fixture, dir string) func() {
	f.t.Helper()
	if err := f.s.root.MkdirAll(dir, 0o750); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.root.Chmod(dir, 0o500); err != nil {
		f.t.Fatal(err)
	}
	restore := func() {
		if err := f.s.root.Chmod(dir, 0o750); err != nil {
			f.t.Error(err)
		}
	}
	f.t.Cleanup(restore)
	return restore
}

// failManifestWrites makes the commit of every manifest of "bucket" fail.
func failManifestWrites(f *fixture) func() {
	f.t.Helper()
	return failDirWrites(f, filepath.Join(versionsDir, "bucket"))
}

// requireStillListed fails unless "k" still holds body, is listed, and
// keeps "bucket" from being deleted.
func requireStillListed(t *testing.T, f *fixture, body string) {
	t.Helper()
	if got := f.body(oref("bucket", "k")); got != body {
		t.Fatalf("k: %q, want %q", got, body)
	}
	out, err := f.s.ListObjects(f.ctx, "bucket", contracts.ListObjectsInput{MaxKeys: 10})
	if err != nil || len(out.Objects) != 1 || out.Objects[0].Key != "k" {
		t.Fatalf("listed %+v, %v", out, err)
	}
	requireIs(t, "DeleteBucket", f.s.checkEmpty(f.ctx, "bucket"), contracts.ErrBucketNotEmpty)
}

// A delete marker whose commit fails on a key with an object in place
// leaves the key indexed, so the object is still listed and its bucket
// not empty, whatever the null version the marker would replace.
func TestAFailedMarkerKeepsALegacyObjectListed(t *testing.T) {
	for _, st := range []contracts.Versioning{
		contracts.VersioningSuspended, contracts.VersioningEnabled,
	} {
		t.Run(string(st), func(t *testing.T) {
			f := newFixture(t)
			f.put("k", "live")
			f.state(st)
			restore := failManifestWrites(f)
			_, err := f.s.DeleteObject(f.ctx, oref("bucket", "k"))
			if err == nil {
				t.Fatal("the delete committed")
			}
			restore()
			requireStillListed(t, f, "live")
			if err = f.deleteBucket(); err == nil {
				t.Fatal("DeleteBucket deleted a bucket holding an object")
			}
		})
	}
}

// A version whose commit fails, after the bucket was Suspended while its
// data was written, leaves the object in place of its key listed.
func TestAFailedVersionKeepsALegacyObjectListed(t *testing.T) {
	f := newFixture(t)
	f.put("k", "live")
	f.state(contracts.VersioningEnabled)
	var restore func()
	f.s.hook = func(stage string) error {
		if stage == stageWritten {
			f.s.hook = nil
			f.state(contracts.VersioningSuspended)
			restore = failManifestWrites(f)
		}
		return nil
	}
	_, err := f.s.PutObject(f.ctx, "bucket", "k", strings.NewReader("new"),
		contracts.PutObjectInput{}, contracts.Precondition{})
	if err == nil {
		t.Fatal("the write committed")
	}
	restore()
	requireStillListed(t, f, "live")
	if n := f.backendObjects(); n != 1 {
		t.Fatalf("backend objects: %d, want the object in place only", n)
	}
}

// A first change of a key whose commit fails leaves the key unindexed.
func TestAFailedFirstCommitUnindexes(t *testing.T) {
	for name, change := range map[string]func(f *fixture) error{
		"put": func(f *fixture) error { return errOf(writeOps["put"](f)) },
		"delete": func(f *fixture) error {
			return errOf(f.s.DeleteObject(f.ctx, oref("bucket", "k")))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.state(contracts.VersioningEnabled)
			failManifestWrites(f)
			if err := change(f); err == nil {
				t.Fatal("the change committed")
			}
			if f.exists(indexEntry("bucket", "k")) || f.backendObjects() != 0 {
				t.Fatal("the failed change left the key indexed or its data")
			}
		})
	}
}

// A delete of a version whose commit fails, the last version or not,
// leaves every version as it was.
func TestAFailedVersionDeleteKeepsTheVersions(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	v2 := f.put("k", "v2")
	restore := failManifestWrites(f)
	_, err := f.s.DeleteObject(f.ctx, vref("k", v1.VersionID))
	if err == nil {
		t.Fatal("the delete committed")
	}
	restore()
	f.requireIDs(v2.VersionID, v1.VersionID)
	f.del(vref("k", v2.VersionID))

	restore = failManifestWrites(f)
	if _, err = f.s.DeleteObject(f.ctx, vref("k", v1.VersionID)); err == nil {
		t.Fatal("the delete of the last version committed")
	}
	restore()
	f.requireIDs(v1.VersionID)
	requireStillListed(t, f, "v1")
}

// Once a change is committed it has happened: a failure to remove what it
// displaced is not its error, and the change is made once.
func TestAFailedCollectIsNotTheWritesError(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningSuspended)
	f.putIn("k", "old", contracts.PutObjectInput{ACL: []byte("<old/>")})
	f.state(contracts.VersioningEnabled)
	f.put("k", "v1")
	f.state(contracts.VersioningSuspended)
	f.putIn("k", "null", contracts.PutObjectInput{ACL: []byte("<null/>")})
	old := f.versions("k")[0]

	failDirWrites(f, filepath.Join(aclDir, "bucket"))
	info, err := writeOps["complete"](f)
	if err != nil {
		t.Fatalf("a write whose collect failed: %v", err)
	}
	if got := f.versions("k"); len(got) != 2 || got[0].ID != info.VersionID ||
		got[0].ETag != info.ETag {
		t.Fatalf("versions: %+v", got)
	}
	if !f.exists(sideFile(aclDir, "bucket", "k", &old)) {
		t.Fatal("the displaced version's ACL was removed after all")
	}
	uploads, err := f.s.ListMultipartUploads(f.ctx, "bucket")
	if err != nil || len(uploads) != 0 {
		t.Fatalf("the completed upload is left: %v %v", uploads, err)
	}
}
