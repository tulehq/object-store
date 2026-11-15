package dirimpl

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"tule.dev/object-store/internal/contracts"
)

// listingFixture writes keys a, b/1, b/2, c/1 and d with versions and
// delete markers interleaved, d with a legacy null version, and returns
// the full listing, in S3's order.
func listingFixture(t *testing.T) (*fixture, []contracts.ObjectVersion) {
	t.Helper()
	f := newFixture(t)
	f.put("d", "legacy")
	f.put("stale-legacy", "legacy")
	f.state(contracts.VersioningEnabled)
	for _, key := range []string{"a", "b/1", "b/2", "c/1", "d"} {
		f.put(key, "v1")
		f.del(oref("bucket", key))
		f.put(key, "v2")
	}
	f.del(oref("bucket", "c/1"))
	f.state(contracts.VersioningSuspended)
	f.put("a", "null")
	f.state(contracts.VersioningEnabled)
	f.put("a", "v3")

	var want []contracts.ObjectVersion
	for _, key := range []string{"a", "b/1", "b/2", "c/1", "d", "stale-legacy"} {
		for i, e := range f.versions(key) {
			want = append(want, contracts.ObjectVersion{
				Key: key, VersionID: e.ID, ETag: e.ETag, Size: e.Size,
				LastModified: e.LastModified, IsLatest: i == 0, DeleteMarker: e.Marker,
			})
		}
	}
	return f, want
}

// list lists "bucket" with in.
func (f *fixture) list(in contracts.ListObjectVersionsInput) *contracts.ListObjectVersionsOutput {
	f.t.Helper()
	out, err := f.s.ListObjectVersions(f.ctx, "bucket", in)
	if err != nil {
		f.t.Fatalf("list %+v: %v", in, err)
	}
	return out
}

// Versions are listed by key, each key's versions and markers interleaved
// newest first, the first the latest, a legacy object as its null version.
func TestListObjectVersionsOrder(t *testing.T) {
	f, want := listingFixture(t)
	out := f.list(contracts.ListObjectVersionsInput{MaxKeys: 1000})
	if !slices.Equal(out.Versions, want) || out.IsTruncated || out.NextKeyMarker != "" {
		t.Fatalf("listing:\n%+v\nwant\n%+v", out, want)
	}
	// a's versions: v3, null, v2, a marker, v1.
	requireShape(t, out.Versions[:5], "LNVMV")
	requireShape(t, out.Versions[len(out.Versions)-1:], "L")
	if last := out.Versions[len(out.Versions)-1]; last.Key != "stale-legacy" ||
		last.VersionID != contracts.VersionNull {
		t.Fatalf("legacy: %+v", last)
	}
}

// requireShape fails unless versions, of one key, are shape, a letter
// each: L for the latest, N for the null version, M for a delete marker
// and V for another version.
func requireShape(t *testing.T, versions []contracts.ObjectVersion, shape string) {
	t.Helper()
	for i, v := range versions {
		got := "V"
		switch {
		case v.IsLatest:
			got = "L"
		case v.DeleteMarker:
			got = "M"
		case v.VersionID == contracts.VersionNull:
			got = "N"
		}
		if got != shape[i:i+1] || v.Key != versions[0].Key {
			t.Fatalf("version %d: %+v, want %s", i, v, shape[i:i+1])
		}
	}
}

// Prefix and delimiter roll keys up into common prefixes, each counted
// once, and the key marker is exclusive, a common prefix not after it
// left out.
func TestListObjectVersionsPrefixesAndMarkers(t *testing.T) {
	f, want := listingFixture(t)
	out := f.list(contracts.ListObjectVersionsInput{Delimiter: "/", MaxKeys: 1000})
	if !slices.Equal(out.CommonPrefixes, []string{"b/", "c/"}) ||
		!slices.Equal(out.Versions, only(want, "a", "d", "stale-legacy")) {
		t.Fatalf("delimited: %+v", out)
	}
	out = f.list(contracts.ListObjectVersionsInput{Prefix: "b/", MaxKeys: 1000})
	if len(out.CommonPrefixes) != 0 || !slices.Equal(out.Versions, only(want, "b/1", "b/2")) {
		t.Fatalf("prefix b/: %+v", out)
	}
	out = f.list(contracts.ListObjectVersionsInput{KeyMarker: "b/1", MaxKeys: 1000})
	if !slices.Equal(out.Versions, only(want, "b/2", "c/1", "d", "stale-legacy")) {
		t.Fatalf("after b/1: %+v", out)
	}
	out = f.list(contracts.ListObjectVersionsInput{
		KeyMarker: "b/", Delimiter: "/", MaxKeys: 1000,
	})
	if !slices.Equal(out.CommonPrefixes, []string{"c/"}) {
		t.Fatalf("after prefix b/: %+v", out)
	}
	out = f.list(contracts.ListObjectVersionsInput{
		KeyMarker: "b/1", Delimiter: "/", MaxKeys: 1000,
	})
	if !slices.Equal(out.CommonPrefixes, []string{"c/"}) {
		t.Fatalf("after b/1 delimited: %+v", out)
	}
}

// only returns the versions of keys in versions.
func only(versions []contracts.ObjectVersion, keys ...string) []contracts.ObjectVersion {
	return slices.DeleteFunc(slices.Clone(versions), func(v contracts.ObjectVersion) bool {
		return !slices.Contains(keys, v.Key)
	})
}

// Paging through a listing with any page size, resuming from each page's
// markers, lists every entry once, in order.
func TestListObjectVersionsPaging(t *testing.T) {
	f, want := listingFixture(t)
	for _, delimiter := range []string{"", "/"} {
		full := f.list(contracts.ListObjectVersionsInput{Delimiter: delimiter, MaxKeys: 1000})
		for size := 1; size <= len(want)+1; size++ {
			t.Run(fmt.Sprintf("%q/%d", delimiter, size), func(t *testing.T) {
				got := f.pages(contracts.ListObjectVersionsInput{
					Delimiter: delimiter, MaxKeys: size,
				})
				if !slices.Equal(got.Versions, full.Versions) ||
					!slices.Equal(got.CommonPrefixes, full.CommonPrefixes) {
					t.Fatalf("paged:\n%+v\nwant\n%+v", got, full)
				}
			})
		}
	}
}

// pages lists every page of in's listing, at most 100, and returns them
// joined.
func (f *fixture) pages(in contracts.ListObjectVersionsInput) *contracts.ListObjectVersionsOutput {
	f.t.Helper()
	all := &contracts.ListObjectVersionsOutput{}
	for range 100 {
		out := f.list(in)
		if n := len(out.Versions) + len(out.CommonPrefixes); n > in.MaxKeys ||
			n < in.MaxKeys && out.IsTruncated {
			f.t.Fatalf("page of %d entries for %d, truncated %v", n, in.MaxKeys, out.IsTruncated)
		}
		all.Versions = append(all.Versions, out.Versions...)
		all.CommonPrefixes = append(all.CommonPrefixes, out.CommonPrefixes...)
		if !out.IsTruncated {
			return all
		}
		in.KeyMarker, in.VersionIDMarker = out.NextKeyMarker, out.NextVersionIDMarker
	}
	f.t.Fatal("the listing does not end")
	return nil
}

// A page whose version-id marker was deleted meanwhile resumes at the
// first version committed before it, and one whose null marker was, at
// the key's first: entries are repeated, never skipped.
func TestListObjectVersionsVanishedMarker(t *testing.T) {
	f, want := listingFixture(t)
	a := only(want, "a")
	// a's versions: v3, null, v2, a marker, v1.
	page := f.list(contracts.ListObjectVersionsInput{MaxKeys: 3})
	if page.NextKeyMarker != "a" || page.NextVersionIDMarker != a[2].VersionID {
		t.Fatalf("page: %+v", page)
	}
	f.del(vref("a", a[2].VersionID))
	next := f.list(contracts.ListObjectVersionsInput{
		KeyMarker: "a", VersionIDMarker: a[2].VersionID, MaxKeys: 1,
	})
	if !slices.Equal(next.Versions, a[3:4]) {
		t.Fatalf("after a vanished marker: %+v, want %+v", next.Versions, a[3:4])
	}

	f.del(vref("a", contracts.VersionNull))
	next = f.list(contracts.ListObjectVersionsInput{
		KeyMarker: "a", VersionIDMarker: contracts.VersionNull, MaxKeys: 1,
	})
	if !slices.Equal(next.Versions, a[:1]) {
		t.Fatalf("after a vanished null marker: %+v, want %+v", next.Versions, a[:1])
	}
}

// A version-id marker needs a key marker, and must be a version ID.
func TestListObjectVersionsBadMarkers(t *testing.T) {
	f := newFixture(t)
	_, err := f.s.ListObjectVersions(f.ctx, "bucket", contracts.ListObjectVersionsInput{
		VersionIDMarker: contracts.VersionNull, MaxKeys: 1,
	})
	requireIs(t, "version-id marker alone", err, contracts.ErrVersionMarkerWithoutKey)
	_, err = f.s.ListObjectVersions(f.ctx, "bucket", contracts.ListObjectVersionsInput{
		KeyMarker: "k", VersionIDMarker: "nope", MaxKeys: 1,
	})
	requireIs(t, "an invalid version-id marker", err, contracts.ErrInvalidVersionID)
}

// An Unversioned bucket lists each object as its null version.
func TestListObjectVersionsUnversioned(t *testing.T) {
	f := newFixture(t)
	info := f.put("k", "x")
	out := f.list(contracts.ListObjectVersionsInput{MaxKeys: 10})
	want := []contracts.ObjectVersion{{
		Key: "k", VersionID: contracts.VersionNull, ETag: info.ETag, Size: info.Size,
		LastModified: info.LastModified, IsLatest: true,
	}}
	if !slices.Equal(out.Versions, want) {
		t.Fatalf("listing: %+v", out)
	}
}

// ListObjects hides the keys whose current version is a delete marker,
// and a common prefix whose keys all have one, and lists the others' from
// their manifests.
func TestListObjectsHidesDeleteMarkers(t *testing.T) {
	f, _ := listingFixture(t)
	out, err := f.s.ListObjects(f.ctx, "bucket", contracts.ListObjectsInput{MaxKeys: 1000})
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(out.Objects))
	for _, o := range out.Objects {
		keys = append(keys, o.Key)
		if o.VersionID != f.ids(o.Key)[0] || o.ETag == "" || o.LastModified.IsZero() {
			t.Fatalf("listed %+v", o)
		}
	}
	if !slices.Equal(keys, []string{"a", "b/1", "b/2", "d", "stale-legacy"}) {
		t.Fatalf("listed %v", keys)
	}
	out, err = f.s.ListObjects(f.ctx, "bucket", contracts.ListObjectsInput{
		Delimiter: "/", MaxKeys: 1000,
	})
	if err != nil || !slices.Equal(out.CommonPrefixes, []string{"b/"}) {
		t.Fatalf("delimited: %+v %v", out, err)
	}
}

// A key whose current version is a delete marker takes no room in a page
// of ListObjects, so a page is not truncated on it.
func TestListObjectsPagesCountOnlyListedKeys(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	f.put("a", "x")
	f.del(oref("bucket", "b"))
	f.requirePage("", "a", false)
	f.put("c", "x")
	f.requirePage("", "a", true)
	f.requirePage("a", "c", false)
}

// requirePage fails unless the page of one key after startAfter lists key
// alone, truncated or not.
func (f *fixture) requirePage(startAfter, key string, truncated bool) {
	f.t.Helper()
	out, err := f.s.ListObjects(f.ctx, "bucket", contracts.ListObjectsInput{
		MaxKeys: 1, StartAfter: startAfter,
	})
	if err != nil || len(out.Objects) != 1 || out.Objects[0].Key != key ||
		out.IsTruncated != truncated {
		f.t.Fatalf("page after %q: %+v %v, want %s, truncated %v",
			startAfter, out, err, key, truncated)
	}
}

// A version listing of no keys at most is an empty page, not truncated.
func TestListObjectVersionsMaxKeysZero(t *testing.T) {
	f, _ := listingFixture(t)
	for _, n := range []int{0, -1} {
		out := f.list(contracts.ListObjectVersionsInput{MaxKeys: n})
		if out.IsTruncated || len(out.Versions) != 0 || len(out.CommonPrefixes) != 0 {
			t.Fatalf("MaxKeys %d: %+v", n, out)
		}
	}
}

// A version committed on a clock behind the key's newest version, as after
// a restart on a clock that went back, still sorts above it.
func TestASequenceStaysAboveTheKeys(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	ahead := time.Now().Add(time.Hour).UnixNano()
	if err := f.s.writeVersions("bucket", &versionManifest{Key: "k", Versions: []versionEntry{
		{ID: newVersionID(ahead), Marker: true, Seq: ahead},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.addToIndex("bucket", "k"); err != nil {
		t.Fatal(err)
	}
	v := f.put("k", "x")
	vs := f.versions("k")
	if vs[0].ID != v.VersionID || vs[0].Seq <= ahead || v.VersionID <= vs[1].ID {
		t.Fatalf("versions %+v", vs)
	}
	next := f.list(contracts.ListObjectVersionsInput{
		KeyMarker: "k", VersionIDMarker: v.VersionID, MaxKeys: 10,
	})
	if len(next.Versions) != 1 || next.Versions[0].VersionID != vs[1].ID {
		t.Fatalf("after %s: %+v", v.VersionID, next.Versions)
	}
}
