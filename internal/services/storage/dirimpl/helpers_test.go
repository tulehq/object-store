package dirimpl

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// oref names the current version of key in bucket.
func oref(bucket, key string) contracts.ObjectRef {
	return contracts.ObjectRef{Bucket: bucket, Key: key}
}

// vref names version id of key in "bucket".
func vref(key, id string) contracts.ObjectRef {
	return contracts.ObjectRef{Bucket: "bucket", Key: key, Version: id}
}

// asIs is the failure mapping of a write that fails with its error as it
// is.
func asIs(err error) error {
	return err
}

// errOf returns the error of a call that returns a value with it.
func errOf[T any](_ T, err error) error {
	return err
}

// fixture is a test's service, with a bucket named "bucket" and a context
// pinned to it.
type fixture struct {
	t       *testing.T
	s       *Service
	ctx     context.Context
	release func()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s := newTestService(t)
	ctx, release := pin(t, s)
	return &fixture{t: t, s: s, ctx: ctx, release: release}
}

// deleteBucket releases the fixture's pin and deletes "bucket".
func (f *fixture) deleteBucket() error {
	f.t.Helper()
	f.release()
	return f.s.DeleteBucket(context.Background(), ref(f.t, f.s, "bucket"))
}

// state sets the versioning state of "bucket".
func (f *fixture) state(st contracts.Versioning) {
	f.t.Helper()
	if err := f.s.PutBucketVersioning(f.ctx, "bucket",
		contracts.VersioningConfig{Status: st}); err != nil {
		f.t.Fatalf("set versioning %q: %v", st, err)
	}
}

// put writes body at key.
func (f *fixture) put(key, body string) *contracts.ObjectInfo {
	f.t.Helper()
	return f.putIn(key, body, contracts.PutObjectInput{})
}

// putIn writes body at key with in.
func (f *fixture) putIn(key, body string, in contracts.PutObjectInput) *contracts.ObjectInfo {
	f.t.Helper()
	info, err := f.s.PutObject(f.ctx, "bucket", key, strings.NewReader(body), in,
		contracts.Precondition{})
	if err != nil {
		f.t.Fatalf("put %q: %v", key, err)
	}
	return info
}

// upload starts an upload of body at key, as one part, and returns its ID
// and parts.
func (f *fixture) upload(key, body string) (string, []contracts.CompletedPart) {
	f.t.Helper()
	id, _, err := f.s.CreateMultipartUpload(f.ctx, "bucket", key, contracts.PutObjectInput{})
	if err != nil {
		f.t.Fatal(err)
	}
	etag, _, err := f.s.UploadPart(f.ctx, "bucket", key, id, 1, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	return id, []contracts.CompletedPart{{PartNumber: 1, ETag: etag}}
}

// del deletes ref.
func (f *fixture) del(ref contracts.ObjectRef) *contracts.DeleteObjectOutput {
	f.t.Helper()
	out, err := f.s.DeleteObject(f.ctx, ref)
	if err != nil {
		f.t.Fatalf("delete %+v: %v", ref, err)
	}
	return out
}

// read returns the body of ref and its info.
func (f *fixture) read(ref contracts.ObjectRef) (string, *contracts.ObjectInfo) {
	f.t.Helper()
	obj, err := f.s.GetObject(f.ctx, ref)
	if err != nil {
		f.t.Fatalf("get %+v: %v", ref, err)
	}
	b, err := io.ReadAll(obj.Body)
	if cErr := obj.Body.Close(); err != nil || cErr != nil {
		f.t.Fatal(err, cErr)
	}
	return string(b), &obj.ObjectInfo
}

// body returns the body of ref.
func (f *fixture) body(ref contracts.ObjectRef) string {
	f.t.Helper()
	b, _ := f.read(ref)
	return b
}

// versions returns the versions of key, newest first, a key without a
// manifest's object as its null version.
func (f *fixture) versions(key string) []versionEntry {
	f.t.Helper()
	vs, err := f.s.keyVersions(f.ctx, "bucket", key)
	if err != nil {
		f.t.Fatal(err)
	}
	return vs
}

// ids returns the IDs of the versions of key, newest first.
func (f *fixture) ids(key string) []string {
	f.t.Helper()
	vs := f.versions(key)
	out := make([]string, len(vs))
	for i, e := range vs {
		out[i] = e.ID
	}
	return out
}

// requireIDs fails unless the versions of "k" have IDs want, newest
// first.
func (f *fixture) requireIDs(want ...string) {
	f.t.Helper()
	if got := f.ids("k"); !slices.Equal(got, want) {
		f.t.Fatalf("versions of k: %v, want %v", got, want)
	}
}

// hasManifest reports whether "k" has a version manifest.
func (f *fixture) hasManifest() bool {
	f.t.Helper()
	m, _, err := f.s.readVersions("bucket", "k")
	if err != nil {
		f.t.Fatal(err)
	}
	return m != nil
}

// hasData reports whether the backend holds the data of e, a version of
// "k", nil for the object at keyHash.
func (f *fixture) hasData(e *versionEntry) bool {
	f.t.Helper()
	_, err := f.s.dir.HeadObject(f.ctx, &storage.HeadObjectInput{
		BaseObjectInput: dataInput("bucket", "k", e),
	})
	if errors.Is(err, storage.ErrObjectNotFound) {
		return false
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return true
}

// backendObjects returns how many objects the backend holds in "bucket".
func (f *fixture) backendObjects() int {
	f.t.Helper()
	bucket := "bucket"
	out, err := f.s.dir.ListObjects(f.ctx, &storage.ListObjectsInput{Bucket: &bucket})
	if err != nil {
		f.t.Fatal(err)
	}
	return len(out.Objects)
}

// exists reports whether name exists in the storage root.
func (f *fixture) exists(name string) bool {
	f.t.Helper()
	_, err := f.s.root.Stat(name)
	return err == nil
}

// isIssued reports whether id is a version ID this package issues.
func isIssued(id string) bool {
	_, ok := versionSeq(id)
	return ok
}
