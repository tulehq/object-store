package dirimpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

var versionIDRe = regexp.MustCompile(`^[0-9A-V]{26}$`)

// Version IDs are 26 characters of base32hex, never "null", and sort as
// the sequences they encode, which nextSeq draws strictly increasing,
// also from many goroutines at once.
func TestVersionIDs(t *testing.T) {
	s := newTestService(t)
	const goroutines, each = 64, 50
	seqs := make([][]int64, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for range each {
				seqs[g] = append(seqs[g], s.nextSeq(0))
			}
		})
	}
	wg.Wait()

	all := map[int64]bool{}
	for _, own := range seqs {
		if !slices.IsSorted(own) {
			t.Fatalf("one goroutine's sequences are not increasing: %v", own)
		}
		for _, seq := range own {
			all[seq] = true
		}
	}
	if len(all) != goroutines*each {
		t.Fatalf("%d distinct sequences of %d", len(all), goroutines*each)
	}
	requireIDsOf(t, all)
}

// requireIDsOf fails unless the version IDs of seqs are well formed,
// decode to their sequences and sort as them.
func requireIDsOf(t *testing.T, seqs map[int64]bool) {
	t.Helper()
	ids := make([]string, 0, len(seqs))
	for seq := range seqs {
		id := newVersionID(seq)
		got, ok := versionSeq(id)
		if !versionIDRe.MatchString(id) || id == contracts.VersionNull || !ok || got != seq {
			t.Fatalf("version id %q of %d decodes to %d, %v", id, seq, got, ok)
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for i := 1; i < len(ids); i++ {
		a, _ := versionSeq(ids[i-1])
		b, _ := versionSeq(ids[i])
		if a >= b {
			t.Fatalf("ids sort out of sequence order: %s %s", ids[i-1], ids[i])
		}
	}
}

// Only "", "null" and IDs of the issued shape are version IDs.
func TestCheckVersionID(t *testing.T) {
	valid := newVersionID(1<<62 + 5)
	for _, id := range []string{"", contracts.VersionNull, valid, "0000000000000FFFFFFFFFFFFF"} {
		if err := checkVersionID(id); err != nil {
			t.Errorf("%q: %v", id, err)
		}
	}
	for _, id := range []string{
		"NULL", "abc", valid[:25], valid + "0", "0000000000000W000000000000",
		"G000000000000000000000000Q", "0000000000000G000000000000",
		"0000000000000000000000000a",
	} {
		if err := checkVersionID(id); !errors.Is(err, contracts.ErrInvalidVersionID) {
			t.Errorf("%q: got %v, want ErrInvalidVersionID", id, err)
		}
	}
}

// A manifest reads back as it was written, and one stored for another key
// is an error.
func TestManifestRoundTrip(t *testing.T) {
	f := newFixture(t)
	m := &versionManifest{Key: "k", Versions: []versionEntry{
		{ID: newVersionID(2), Token: "T", ETag: "e", Size: 3, Seq: 2},
		{ID: contracts.VersionNull, Marker: true, Seq: 1},
	}}
	if err := f.s.writeVersions("bucket", m); err != nil {
		t.Fatal(err)
	}
	got, _, err := f.s.readVersions("bucket", "k")
	if err != nil || !slices.Equal(got.Versions, m.Versions) || got.Key != "k" {
		t.Fatalf("read back: %+v %v", got, err)
	}

	raw, err := json.Marshal(versionManifest{Key: "other", Versions: []versionEntry{}})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.writeFile(versionsFile("bucket", "k"), raw); err != nil {
		t.Fatal(err)
	}
	_, _, err = f.s.readVersions("bucket", "k")
	requireIs(t, "a manifest of another key", err, storage.ErrBackend)
}

// A legacy object is its key's null version, and the manifest its key's
// first versioned change writes lists it as such, its data left in place.
func TestLegacyNullSynthesis(t *testing.T) {
	f := newFixture(t)
	info := f.put("k", "legacy")
	if info.VersionID != "" {
		t.Fatalf("an Unversioned put reports version %q", info.VersionID)
	}
	f.state(contracts.VersioningEnabled)
	if _, got := f.read(oref("bucket", "k")); got.VersionID != contracts.VersionNull {
		t.Fatalf("a legacy object in a versioned bucket reads as version %q", got.VersionID)
	}
	m, existed, err := f.s.loadVersions(f.ctx, "bucket", "k")
	if err != nil || existed || len(m.Versions) != 1 {
		t.Fatalf("synthesized: %+v %v %v", m, existed, err)
	}
	want := versionEntry{
		ID: contracts.VersionNull, Legacy: true, ETag: info.ETag, Size: info.Size,
		LastModified: info.LastModified, Seq: info.LastModified.UnixNano(),
	}
	if e := m.Versions[0]; e != want {
		t.Fatalf("legacy entry: %+v, want %+v", e, want)
	}

	v1 := f.put("k", "v1")
	f.requireIDs(v1.VersionID, contracts.VersionNull)
	if !f.versions("k")[1].Legacy || !f.hasData(nil) {
		t.Fatal("the legacy null version's data is not left in place")
	}
	if got := f.body(vref("k", contracts.VersionNull)); got != "legacy" {
		t.Fatalf("null version: %q", got)
	}
}

// An empty manifest lists no versions, even with an object at keyHash a
// crash left behind.
func TestAnEmptyManifestIsATombstone(t *testing.T) {
	f := newFixture(t)
	f.put("k", "left")
	f.state(contracts.VersioningEnabled)
	if err := f.s.writeVersions("bucket",
		&versionManifest{Key: "k", Versions: []versionEntry{}}); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.GetObject(f.ctx, oref("bucket", "k"))
	requireIs(t, "get", err, storage.ErrObjectNotFound)
	_, err = f.s.GetObject(f.ctx, vref("k", contracts.VersionNull))
	requireIs(t, "get null", err, storage.ErrVersionNotFound)
	out, err := f.s.ListObjects(f.ctx, "bucket", contracts.ListObjectsInput{MaxKeys: 10})
	if err != nil || len(out.Objects) != 0 {
		t.Fatalf("listed: %+v %v", out, err)
	}
	if f.requireIDs(); !f.hasData(nil) {
		t.Fatal("the planted object is gone")
	}
	if err = f.deleteBucket(); err != nil {
		t.Fatalf("a bucket of tombstones is not empty: %v", err)
	}
}

// A bucket's versioning state starts Unversioned, moves between Enabled
// and Suspended, never back, and goes with the bucket.
func TestVersioningStates(t *testing.T) {
	f := newFixture(t)
	requireVersioning(t, f, contracts.VersioningConfig{})
	f.state(contracts.VersioningSuspended)
	requireVersioning(t, f, contracts.VersioningConfig{Status: contracts.VersioningSuspended})
	f.state(contracts.VersioningEnabled)
	f.state(contracts.VersioningSuspended)
	f.state(contracts.VersioningEnabled)
	requireVersioning(t, f, contracts.VersioningConfig{Status: contracts.VersioningEnabled})

	cfg := contracts.VersioningConfig{Status: contracts.VersioningEnabled, MFADelete: "Disabled"}
	if err := f.s.PutBucketVersioning(f.ctx, "bucket", cfg); err != nil {
		t.Fatal(err)
	}
	requireVersioning(t, f, cfg)

	for bad, want := range map[contracts.VersioningConfig]error{
		{}:                 contracts.ErrUnversionedState,
		{Status: "Paused"}: contracts.ErrInvalidVersioningStatus,
		{Status: contracts.VersioningEnabled, MFADelete: "Enabled"}: contracts.
			ErrMFADeleteUnsupported,
	} {
		requireIs(t, fmt.Sprintf("%+v", bad), f.s.PutBucketVersioning(f.ctx, "bucket", bad), want)
	}
	requireVersioning(t, f, cfg)
	if err := f.s.PutBucketVersioning(context.Background(), "bucket", cfg); err == nil {
		t.Error("PutBucketVersioning without a pin succeeded")
	}
	_, err := f.s.GetBucketVersioning(context.Background(), "missing")
	requireIs(t, "a missing bucket", err, storage.ErrBucketNotFound)

	if err = f.deleteBucket(); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.CreateBucket(context.Background(), "bucket", nil); err != nil {
		t.Fatal(err)
	}
	requireVersioning(t, f, contracts.VersioningConfig{})
}

// requireVersioning fails unless "bucket"'s versioning is want, read
// without a pin.
func requireVersioning(t *testing.T, f *fixture, want contracts.VersioningConfig) {
	t.Helper()
	got, err := f.s.GetBucketVersioning(context.Background(), "bucket")
	if err != nil || *got != want {
		t.Fatalf("versioning: %+v %v, want %+v", got, err, want)
	}
}

// requireMarkerError fails unless err is the DeleteMarkerError of marker
// id, the current version or not, which Is storage.ErrObjectNotFound for
// a current one and contracts.ErrMethodNotAllowed otherwise.
func requireMarkerError(t *testing.T, err error, id string, current bool) {
	t.Helper()
	var dm *contracts.DeleteMarkerError
	if !errors.As(err, &dm) || dm.Current != current || dm.VersionID != id ||
		dm.LastModified.IsZero() {
		t.Fatalf("marker error: %#v", err)
	}
	if errors.Is(err, storage.ErrObjectNotFound) != current ||
		errors.Is(err, contracts.ErrMethodNotAllowed) == current {
		t.Fatalf("marker error %v of a current marker %v", err, current)
	}
}

// Reads tell a current delete marker, a version that is one, a version
// the key does not have and an ID that is none apart.
func TestReadErrors(t *testing.T) {
	f := newFixture(t)
	f.put("legacy", "x")
	ref := vref("legacy", contracts.VersionNull)
	if got, info := f.read(ref); got != "x" || info.VersionID != "" {
		t.Fatalf("null in an Unversioned bucket: %q %q", got, info.VersionID)
	}
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	marker := f.del(oref("bucket", "k"))

	_, err := f.s.GetObject(f.ctx, oref("bucket", "k"))
	requireMarkerError(t, err, marker.VersionID, true)
	_, err = f.s.StatObject(f.ctx, vref("k", marker.VersionID))
	requireMarkerError(t, err, marker.VersionID, false)

	for _, id := range []string{newVersionID(1), contracts.VersionNull} {
		_, err = f.s.GetObject(f.ctx, vref("k", id))
		requireIs(t, "missing version "+id, err, storage.ErrVersionNotFound)
		requireIs(t, "missing version "+id, err, storage.ErrObjectNotFound)
	}
	_, err = f.s.GetObjectTagging(f.ctx, vref("k", "nope"))
	requireIs(t, "invalid id", err, contracts.ErrInvalidVersionID)
	if got := f.body(vref("k", v1.VersionID)); got != "v1" {
		t.Fatalf("v1: %q", got)
	}
}

// keyState is a state of the key "k" a test starts from.
type keyState struct {
	setup func(f *fixture)
	name  string
	// versioned is set for a key state only a versioned bucket reaches.
	versioned bool
}

var keyStates = []keyState{
	{name: "absent", setup: func(*fixture) {}},
	{name: "legacy", setup: func(f *fixture) { f.put("k", "old") }},
	{name: "listed legacy null", versioned: true, setup: func(f *fixture) {
		f.put("k", "old")
		f.state(contracts.VersioningEnabled)
		f.put("k", "v1")
	}},
	{name: "null object", versioned: true, setup: func(f *fixture) {
		f.state(contracts.VersioningEnabled)
		f.put("k", "v1")
		f.state(contracts.VersioningSuspended)
		f.put("k", "null")
	}},
	{name: "null marker", versioned: true, setup: func(f *fixture) {
		f.state(contracts.VersioningEnabled)
		f.put("k", "v1")
		f.state(contracts.VersioningSuspended)
		f.del(oref("bucket", "k"))
	}},
	{name: "only non-null", versioned: true, setup: func(f *fixture) {
		f.state(contracts.VersioningEnabled)
		f.put("k", "v1")
		f.put("k", "v2")
	}},
}

var bucketStates = []contracts.Versioning{
	contracts.Unversioned, contracts.VersioningEnabled, contracts.VersioningSuspended,
}

// forEachCase runs run for each bucket state and key state a bucket in it
// can have, from a fixture whose "k" is in the key state, in the bucket
// state.
func forEachCase(t *testing.T, run func(t *testing.T, f *fixture, st contracts.Versioning)) {
	t.Helper()
	for _, st := range bucketStates {
		for _, ks := range keyStates {
			if st == contracts.Unversioned && ks.versioned {
				continue
			}
			t.Run(cmpName(st)+"/"+ks.name, func(t *testing.T) {
				f := newFixture(t)
				f.put("src", "new")
				ks.setup(f)
				if st != contracts.Unversioned {
					f.state(st)
				}
				run(t, f, st)
			})
		}
	}
}

func cmpName(st contracts.Versioning) string {
	if st == contracts.Unversioned {
		return "Unversioned"
	}
	return string(st)
}

// writeOps are the writes of body "new" at "k": a put, a copy of "src",
// which holds it, and a completed upload.
var writeOps = map[string]func(f *fixture) (*contracts.ObjectInfo, error){
	"put": func(f *fixture) (*contracts.ObjectInfo, error) {
		return f.s.PutObject(f.ctx, "bucket", "k", strings.NewReader("new"),
			contracts.PutObjectInput{ACL: []byte("<new/>")}, contracts.Precondition{})
	},
	"copy": func(f *fixture) (*contracts.ObjectInfo, error) {
		out, err := f.s.CopyObject(f.ctx, contracts.CopyObjectInput{
			SrcBucket: "bucket", SrcKey: "src", DstBucket: "bucket", DstKey: "k",
			ACL: []byte("<new/>"),
		})
		if err != nil {
			return nil, err
		}
		return &out.ObjectInfo, nil
	},
	"complete": func(f *fixture) (*contracts.ObjectInfo, error) {
		id, parts := f.upload("k", "new")
		return f.s.CompleteMultipartUpload(f.ctx, "bucket", "k", id, parts,
			contracts.Precondition{})
	},
}

// A write is a new version on top in an Enabled bucket, replaces the null
// version, object or marker, in a Suspended one, and replaces the object
// in an Unversioned one; it never touches the other versions.
func TestWriteMatrix(t *testing.T) {
	for name, write := range writeOps {
		t.Run(name, func(t *testing.T) {
			forEachCase(t, func(t *testing.T, f *fixture, st contracts.Versioning) {
				before, listed := f.versions("k"), f.hasManifest()
				info, err := write(f)
				if err != nil {
					t.Fatal(err)
				}
				requireWritten(t, f, st, before, listed, info)
			})
		})
	}
}

// requireWritten fails unless "k" was written as a write in state st of a
// key whose versions were before, listed in a manifest or not, should,
// with info the write's.
func requireWritten(
	t *testing.T,
	f *fixture,
	st contracts.Versioning,
	before []versionEntry,
	listed bool,
	info *contracts.ObjectInfo,
) {
	t.Helper()
	after := f.versions("k")
	requireNewID(t, st, after[0].ID)
	kept, displaced := before, (*versionEntry)(nil)
	if st != contracts.VersioningEnabled {
		kept, displaced = withoutNull(before, listed)
	}
	reported := after[0].ID
	if st == contracts.Unversioned {
		reported = ""
	}
	if info.VersionID != reported || !slices.Equal(after[1:], kept) {
		t.Fatalf("wrote %q, versions %+v; want %q on %+v", info.VersionID, after, reported, kept)
	}
	if f.body(oref("bucket", "k")) != "new" || f.body(vref("k", after[0].ID)) != "new" {
		t.Fatal("the new version is not current")
	}
	requireData(t, f, after)
	requireGone(t, f, displaced)
	if want := st == contracts.VersioningEnabled || listed; f.hasManifest() != want {
		t.Fatalf("manifest: %v, want %v", !want, want)
	}
}

// requireNewID fails unless id is the ID of a version a change in state
// st adds: an issued one when Enabled, and null otherwise.
func requireNewID(t *testing.T, st contracts.Versioning, id string) {
	t.Helper()
	if st == contracts.VersioningEnabled != isIssued(id) ||
		st != contracts.VersioningEnabled && id != contracts.VersionNull {
		t.Fatalf("new version %q in state %q", id, st)
	}
}

// requireGone fails unless the data of e, a version that was removed, is
// gone; nil and a marker have none.
func requireGone(t *testing.T, f *fixture, e *versionEntry) {
	t.Helper()
	if e != nil && !e.Marker && f.hasData(e) {
		t.Fatalf("the data of removed version %+v is left", e)
	}
}

// requireData fails unless the backend holds the data of every version of
// versions, which are newest first.
func requireData(t *testing.T, f *fixture, versions []versionEntry) {
	t.Helper()
	for i, e := range versions {
		if !e.Marker && !f.hasData(&e) {
			t.Fatalf("no data for version %+v", e)
		}
		if i > 0 && e.Seq >= versions[i-1].Seq {
			t.Fatalf("versions out of commit order: %+v", versions)
		}
	}
}

// withoutNull returns before without its null version, and that version,
// for a key whose versions before are listed in a manifest; one without
// keeps none, its null version written over in place.
func withoutNull(before []versionEntry, listed bool) ([]versionEntry, *versionEntry) {
	if !listed {
		return []versionEntry{}, nil
	}
	i := slices.IndexFunc(
		before,
		func(e versionEntry) bool { return e.ID == contracts.VersionNull },
	)
	if i < 0 {
		return before, nil
	}
	return slices.Delete(slices.Clone(before), i, i+1), &before[i]
}

// A delete without a version removes the object in an Unversioned bucket,
// adds a delete marker in an Enabled one, also on a marker or a key
// without versions, and replaces the null version with a null marker in
// a Suspended one.
func TestSimpleDeleteMatrix(t *testing.T) {
	forEachCase(t, requireSimpleDelete)
}

// requireSimpleDelete deletes "k" without a version and fails unless the
// delete did what it does in state st.
func requireSimpleDelete(t *testing.T, f *fixture, st contracts.Versioning) {
	t.Helper()
	before := f.versions("k")
	out := f.del(oref("bucket", "k"))
	after := f.versions("k")
	if st == contracts.Unversioned {
		requireDeletedInPlace(t, f, out)
		return
	}
	requireNewID(t, st, out.VersionID)
	kept, displaced := before, (*versionEntry)(nil)
	if st == contracts.VersioningSuspended {
		kept, displaced = withoutNull(before, true)
	}
	if !out.DeleteMarker || !slices.Equal(after[1:], kept) {
		t.Fatalf("delete: %+v, versions %+v, want a marker on %+v", out, after, kept)
	}
	if e := after[0]; !e.Marker || e.ID != out.VersionID || e.LastModified.IsZero() {
		t.Fatalf("marker %+v", e)
	}
	requireGone(t, f, displaced)
	requireData(t, f, after)
	_, err := f.s.StatObject(f.ctx, oref("bucket", "k"))
	requireIs(t, "a read of the marker", err, storage.ErrObjectNotFound)
}

// requireDeletedInPlace fails unless out, the output of a delete of "k" in
// an Unversioned bucket, is empty, and the key is gone.
func requireDeletedInPlace(t *testing.T, f *fixture, out *contracts.DeleteObjectOutput) {
	t.Helper()
	if *out != (contracts.DeleteObjectOutput{}) || len(f.versions("k")) != 0 ||
		f.exists(indexEntry("bucket", "k")) || f.hasData(nil) {
		t.Fatalf("delete: %+v, versions %+v", out, f.versions("k"))
	}
}

// A delete of a version removes it for good, marker or object; the last
// version's goes with its key's manifest, index entry and object at
// keyHash. A version the key does not have is deleted already.
func TestVersionedDeleteMatrix(t *testing.T) {
	forEachCase(t, func(t *testing.T, f *fixture, st contracts.Versioning) {
		missing := newVersionID(f.s.nextSeq(0))
		out := f.del(vref("k", missing))
		if want := missingDeleted(st, missing); *out != want {
			t.Fatalf("delete of a missing version: %+v, want %+v", out, want)
		}
		for before := f.versions("k"); len(before) > 0; before = f.versions("k") {
			requireVersionDelete(t, f, st, before, before[len(before)/2])
		}
		if f.hasManifest() || f.exists(indexEntry("bucket", "k")) || f.hasData(nil) {
			t.Fatal("the key outlives its last version")
		}
	})
}

// requireVersionDelete deletes e, one of the versions before of "k", and
// fails unless the delete removed it for good in state st.
func requireVersionDelete(
	t *testing.T,
	f *fixture,
	st contracts.Versioning,
	before []versionEntry,
	e versionEntry,
) {
	t.Helper()
	out := f.del(vref("k", e.ID))
	want := contracts.DeleteObjectOutput{VersionID: e.ID, DeleteMarker: e.Marker}
	if st == contracts.Unversioned {
		want.VersionID = ""
	}
	after := f.versions("k")
	if *out != want || len(after) != len(before)-1 || slices.Contains(after, e) {
		t.Fatalf("delete of %+v: %+v, versions %+v", e, out, after)
	}
	requireGone(t, f, &e)
	requireData(t, f, after)
}

// missingDeleted is the output of a delete of a version that does not
// exist, missing, in state st.
func missingDeleted(st contracts.Versioning, missing string) contracts.DeleteObjectOutput {
	if st == contracts.Unversioned {
		return contracts.DeleteObjectOutput{}
	}
	return contracts.DeleteObjectOutput{VersionID: missing}
}

// A bucket holding only delete markers, or versions behind a current
// marker, is not empty; it is once every version is deleted.
func TestDeleteBucketCountsVersionsAndMarkers(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	f.del(oref("bucket", "marker-only"))
	v := f.put("k", "v1")
	f.del(oref("bucket", "k"))
	requireIs(t, "markers and versions", f.s.checkEmpty(f.ctx, "bucket"),
		contracts.ErrBucketNotEmpty)

	f.del(vref("k", v.VersionID))
	requireIs(t, "markers only", f.s.checkEmpty(f.ctx, "bucket"), contracts.ErrBucketNotEmpty)
	for _, key := range []string{"k", "marker-only"} {
		for _, id := range f.ids(key) {
			f.del(vref(key, id))
		}
	}
	if err := f.deleteBucket(); err != nil {
		t.Fatal(err)
	}
	if f.exists(filepath.Join(versionsDir, "bucket")) ||
		f.exists(versioningFile("bucket")) {
		t.Fatal("the bucket's versions or versioning outlive it")
	}
}

// A versioned write's precondition holds of the current version, a delete
// marker counting as none, and one that fails leaves nothing behind.
func TestVersionedPreconditions(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	put := func(cond contracts.Precondition) error {
		return errOf(f.s.PutObject(f.ctx, "bucket", "k", strings.NewReader("x"),
			contracts.PutObjectInput{}, cond))
	}
	requireIs(t, "If-None-Match over an object", put(contracts.Precondition{IfNoneMatch: true}),
		contracts.ErrPreconditionFailed)
	requireIs(t, "a stale If-Match", put(contracts.Precondition{IfMatch: "stale"}),
		contracts.ErrPreconditionFailed)
	if n := f.backendObjects(); n != 1 {
		t.Fatalf("failed writes left %d backend objects", n)
	}
	if err := put(contracts.Precondition{IfMatch: v1.ETag}); err != nil {
		t.Fatal(err)
	}

	f.del(oref("bucket", "k"))
	requireIs(t, "If-Match over a marker", put(contracts.Precondition{IfMatch: "*"}),
		storage.ErrObjectNotFound)
	if err := put(contracts.Precondition{IfNoneMatch: true}); err != nil {
		t.Fatalf("If-None-Match over a marker: %v", err)
	}
	if n := len(f.versions("k")); n != 4 {
		t.Fatalf("%d versions, want v1, v2, the marker and v3", n)
	}
}
