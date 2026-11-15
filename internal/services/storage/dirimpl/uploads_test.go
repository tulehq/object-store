package dirimpl

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tule.dev/object-store/internal/contracts"
)

// multipartEntries returns the names in the multipart directory of the
// storage root.
func (f *fixture) multipartEntries() []string {
	f.t.Helper()
	entries, err := fs.ReadDir(f.s.root.FS(), multipartDir)
	if err != nil {
		f.t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// CreateMultipartUpload and ListParts answer the time the upload was
// initiated, which x-amz-abort-date counts from.
func TestAnUploadReportsWhenItWasInitiated(t *testing.T) {
	f, _ := newClockFixture(t)
	id, initiated, err := f.s.CreateMultipartUpload(f.ctx, "bucket", "k",
		contracts.PutObjectInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !initiated.Equal(clockStart) {
		t.Fatalf("initiated at %v, want %v", initiated, clockStart)
	}
	up, err := f.s.ListParts(f.ctx, "bucket", "k", id)
	if err != nil {
		t.Fatal(err)
	}
	if !up.Initiated.Equal(clockStart) || len(up.Parts) != 0 {
		t.Fatalf("ListParts answered %v and %v, want %v and no parts",
			up.Initiated, up.Parts, clockStart)
	}
}

// An upload is made under a temporary name and renamed into place with its
// manifest, so its directory is never seen without one.
func TestAnUploadIsMadeWithItsManifest(t *testing.T) {
	f := newFixture(t)
	id, _ := f.upload("k", "part")
	names := f.multipartEntries()
	if len(names) != 1 || names[0] != id || !f.exists(filepath.Join(uploadDir(id), manifestFile)) {
		t.Fatalf("multipart directory %v, want only upload %s with its manifest", names, id)
	}
}

// leaveAbandonedUploads leaves in f's root what crashes leave: an upload
// directory without its manifest, with a part, and directories of uploads
// being made, one an hour old and one just made; it returns the names of
// the two last.
func leaveAbandonedUploads(f *fixture, orphan string) (string, string) {
	f.t.Helper()
	if err := f.s.root.MkdirAll(uploadDir(orphan), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.root.WriteFile(partFile(orphan, 1), []byte("part"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	stale, fresh := uploadTempPrefix+"STALE", uploadTempPrefix+"FRESH"
	for _, name := range []string{stale, fresh} {
		dir := filepath.Join(multipartDir, name)
		if err := f.s.root.Mkdir(dir, 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := f.s.root.WriteFile(filepath.Join(dir, manifestFile), []byte("{}"),
			0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleUploadTemp)
	if err := os.Chtimes(filepath.Join(f.s.settings.RootDir, multipartDir, stale),
		old, old); err != nil {
		f.t.Fatal(err)
	}
	return stale, fresh
}

// Opening the store removes the upload directories crashes left: one
// without its manifest, and one being made long ago, but not one being
// made now, nor an upload.
func TestOpeningRemovesAbandonedUploads(t *testing.T) {
	root := t.TempDir()
	s := openTestService(t, root)
	if _, err := s.CreateBucket(context.Background(), "bucket", nil); err != nil {
		t.Fatal(err)
	}
	f := pinnedFixture(t, s)
	live, _ := f.upload("k", "part")
	orphan := "ORPHANORPHANORPHANORPHAN22"
	_, fresh := leaveAbandonedUploads(f, orphan)
	f.release()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	g := pinnedFixture(t, openTestService(t, root))
	names := g.multipartEntries()
	if len(names) != 2 || !g.exists(uploadDir(live)) ||
		!g.exists(filepath.Join(multipartDir, fresh)) {
		t.Fatalf("multipart directory %v, want upload %s and %s", names, live, fresh)
	}
	if ids := g.uploadIDs(); len(ids) != 1 || ids[0] != live {
		t.Fatalf("uploads %v", ids)
	}
}

// DeleteBucket removes the upload directories left without a manifest,
// whatever bucket they were of, with its own uploads.
func TestDeleteBucketRemovesAbandonedUploads(t *testing.T) {
	f := newFixture(t)
	if _, err := f.s.CreateBucket(context.Background(), "other", nil); err != nil {
		t.Fatal(err)
	}
	f.upload("k", "part")
	orphan := "ORPHANORPHANORPHANORPHAN22"
	stale, fresh := leaveAbandonedUploads(f, orphan)
	if err := f.deleteBucket(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{orphan, stale} {
		if f.exists(filepath.Join(multipartDir, name)) {
			t.Fatalf("%s is left", name)
		}
	}
	if !f.exists(filepath.Join(multipartDir, fresh)) {
		t.Fatal("an upload being made was removed")
	}
}

// A part cannot be stored into an upload directory left without its
// manifest, so it never becomes an upload again.
func TestAnAbandonedUploadTakesNoParts(t *testing.T) {
	f := newFixture(t)
	orphan := "ORPHANORPHANORPHANORPHAN22"
	leaveAbandonedUploads(f, orphan)
	_, _, err := f.s.UploadPart(f.ctx, "bucket", "k", orphan, 2, nil)
	if !errors.Is(err, contracts.ErrNoSuchUpload) {
		t.Fatalf("upload part: %v", err)
	}
}
