package dirimpl

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// stubDecider decides as its funcs say, nothing for a nil one, and records
// what it was asked.
type stubDecider struct {
	plan  func(*contracts.LifecycleKey) (contracts.LifecyclePlan, error)
	sole  func(string, contracts.LifecycleVersion) (string, error)
	keys  []*contracts.LifecycleKey
	soles []contracts.LifecycleVersion
}

func (d *stubDecider) Plan(k *contracts.LifecycleKey) (contracts.LifecyclePlan, error) {
	d.keys = append(d.keys, k)
	if d.plan == nil {
		return contracts.LifecyclePlan{}, nil
	}
	return d.plan(k)
}

func (d *stubDecider) RemoveSoleMarker(
	key string,
	marker contracts.LifecycleVersion,
) (string, error) {
	d.soles = append(d.soles, marker)
	if d.sole == nil {
		return "", nil
	}
	return d.sole(key, marker)
}

// planOf returns a decider that plans p for every key.
func planOf(p contracts.LifecyclePlan) *stubDecider {
	return &stubDecider{plan: func(*contracts.LifecycleKey) (contracts.LifecyclePlan, error) {
		return p, nil
	}}
}

// midnight is the UTC midnight after clockStart, the marker time of the
// plans that expire a current version.
var midnight = time.Date(2014, 1, 16, 0, 0, 0, 0, time.UTC)

// expire is a plan that expires the current version by rule "cur".
var expire = contracts.LifecyclePlan{ExpireCurrentRule: "cur", MarkerTime: midnight}

// apply runs ApplyLifecycle on "k" with d.
func (f *fixture) apply(d contracts.LifecycleDecider) *contracts.LifecycleOutcome {
	f.t.Helper()
	out, err := f.s.ApplyLifecycle(f.ctx, "bucket", "k", d)
	if err != nil {
		f.t.Fatalf("apply lifecycle: %v", err)
	}
	return out
}

// requireDeleted fails unless out deleted want, in order, and added no
// marker when marker is "".
func requireDeleted(
	t *testing.T,
	out *contracts.LifecycleOutcome,
	marker string,
	want ...contracts.LifecycleDeletion,
) {
	t.Helper()
	if !slices.Equal(out.Deleted, want) || out.Busy {
		t.Fatalf("outcome %+v, want deletions %+v", out, want)
	}
	if (out.MarkerID != "") != (marker != "") || out.MarkerRule != marker {
		t.Fatalf("outcome %+v, want a marker by %q", out, marker)
	}
}

// requireGone fails unless "k" is gone: no manifest, object, index entry,
// ACL or tag file.
func (f *fixture) requireGone() {
	f.t.Helper()
	if f.hasManifest() || f.hasData(nil) || f.exists(indexEntry("bucket", "k")) ||
		f.exists(aclFile("bucket", "k")) || f.exists(tagFile("bucket", "k")) {
		f.t.Fatal("the key is left")
	}
	if n := f.backendObjects(); n != 0 {
		f.t.Fatalf("backend objects: %d", n)
	}
}

func TestApplyLifecycleUnversioned(t *testing.T) {
	f, _ := newClockFixture(t)
	d := &stubDecider{}
	requireDeleted(t, f.apply(d), "")
	if len(d.keys) != 0 {
		t.Fatal("a key without an object was decided")
	}

	f.putIn("k", "data", contracts.PutObjectInput{
		ACL: legacyACL, Tags: []contracts.Tag{{Key: "a", Value: "1"}},
	})
	requireDeleted(t, f.apply(d), "")
	k := d.keys[0]
	if k.Key != "k" || k.Versioning != contracts.Unversioned || len(k.Versions) != 1 {
		t.Fatalf("decided %+v", k)
	}
	requireCurrent(t, k.Versions[0], "", 4, clockStart)
	requireTagsOf(t, k.Versions[0], []contracts.Tag{{Key: "a", Value: "1"}})

	_, err := f.s.ApplyLifecycle(f.ctx, "bucket", "k", planOf(contracts.LifecyclePlan{
		Remove: map[string]string{contracts.VersionNull: "nc"},
	}))
	if err == nil || f.body(oref("bucket", "k")) != "data" {
		t.Fatalf("a plan removing noncurrent versions of an unversioned key: %v", err)
	}

	requireDeleted(t, f.apply(planOf(expire)), "", contracts.LifecycleDeletion{RuleID: "cur"})
	f.requireGone()
}

// requireCurrent fails unless v is the current version id of size bytes,
// last modified at lastModified.
func requireCurrent(
	t *testing.T,
	v contracts.LifecycleVersion,
	id string,
	size int64,
	lastModified time.Time,
) {
	t.Helper()
	if v.VersionID != id || v.Size != size || v.Marker || !v.LastModified.Equal(lastModified) ||
		!v.NoncurrentSince.IsZero() {
		t.Fatalf("decided version %+v", v)
	}
}

// requireTagsOf fails unless v's Tags returns want.
func requireTagsOf(t *testing.T, v contracts.LifecycleVersion, want []contracts.Tag) {
	t.Helper()
	if tags, err := v.Tags(); err != nil || !slices.Equal(tags, want) {
		t.Fatalf("tags %v, %v; want %v", tags, err, want)
	}
}

// Expiring a legacy key adds a marker over its null version, which is
// kept, noncurrent since the marker.
func TestApplyLifecycleEnabledLegacyKey(t *testing.T) {
	f, _ := newClockFixture(t)
	f.put("k", "legacy")
	f.state(contracts.VersioningEnabled)
	d := planOf(expire)
	out := f.apply(d)
	requireDeleted(t, out, "cur")
	if v := d.keys[0].Versions; len(v) != 1 || v[0].VersionID != contracts.VersionNull {
		t.Fatalf("decided %+v", v)
	}
	f.requireIDs(out.MarkerID, contracts.VersionNull)
	f.requireSince(time.Time{}, midnight)
	if m := f.versions("k")[0]; !m.Marker || !m.LastModified.Equal(midnight) {
		t.Fatalf("marker %+v", m)
	}
	if f.body(vref("k", contracts.VersionNull)) != "legacy" {
		t.Fatal("the expired null version is not kept")
	}
}

// A plan removes noncurrent versions and expires the current one in one
// commit.
func TestApplyLifecycleEnabledVersions(t *testing.T) {
	f, clk := newClockFixture(t)
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	clk.set(hoursIn(1))
	v2 := f.put("k", "v2")
	e1 := f.versions("k")[1]
	d := planOf(contracts.LifecyclePlan{
		ExpireCurrentRule: "cur", MarkerTime: midnight,
		Remove: map[string]string{v1.VersionID: "nc"},
	})
	out := f.apply(d)
	requireDeleted(
		t,
		out,
		"cur",
		contracts.LifecycleDeletion{VersionID: v1.VersionID, RuleID: "nc"},
	)
	vs := d.keys[0].Versions
	requireCurrent(t, vs[0], v2.VersionID, 2, hoursIn(1))
	if len(vs) != 2 || !vs[1].NoncurrentSince.Equal(hoursIn(1)) || vs[1].VersionID != v1.VersionID {
		t.Fatalf("decided %+v", vs)
	}
	f.requireIDs(out.MarkerID, v2.VersionID)
	f.requireSince(time.Time{}, midnight)
	if f.hasData(&e1) || len(d.soles) != 0 {
		t.Fatalf("the removed version's data is left, or a marker over a version was "+
			"asked about (%d)", len(d.soles))
	}
}

// A marker left as the only version, once a run removed the last version
// below it, is removed in that run when the decider says so, and the key
// goes with it (design L8); a marker over a version is not asked about.
func TestApplyLifecycleEnabledSoleMarker(t *testing.T) {
	f, _ := newClockFixture(t)
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	marker := f.del(oref("bucket", "k"))
	keep := &stubDecider{}
	requireDeleted(t, f.apply(keep), "")
	if len(keep.soles) != 0 {
		t.Fatal("a marker over a version was asked about")
	}

	d := planOf(contracts.LifecyclePlan{Remove: map[string]string{v1.VersionID: "nc"}})
	d.sole = func(key string, m contracts.LifecycleVersion) (string, error) {
		if key != "k" || !m.Marker || m.VersionID != marker.VersionID {
			t.Errorf("asked about %q %+v", key, m)
		}
		return "eodm", nil
	}
	requireDeleted(t, f.apply(d), "",
		contracts.LifecycleDeletion{VersionID: v1.VersionID, RuleID: "nc"},
		contracts.LifecycleDeletion{VersionID: marker.VersionID, RuleID: "eodm", Marker: true})
	f.requireGone()
}

// A sole marker the decider keeps leaves the manifest as it was.
func TestApplyLifecycleEnabledSoleMarkerKept(t *testing.T) {
	f, _ := newClockFixture(t)
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	f.del(oref("bucket", "k"))
	f.del(vref("k", v1.VersionID))
	_, before, err := f.s.readVersions("bucket", "k")
	if err != nil {
		t.Fatal(err)
	}
	d := &stubDecider{}
	requireDeleted(t, f.apply(d), "")
	_, after, err := f.s.readVersions("bucket", "k")
	if err != nil || string(before) != string(after) || len(d.soles) != 1 {
		t.Fatalf("a kept sole marker changed the manifest: %s, %v", after, err)
	}
}

func TestApplyLifecycleSuspended(t *testing.T) {
	t.Run("null current", func(t *testing.T) {
		f, _ := newClockFixture(t)
		f.state(contracts.VersioningEnabled)
		v1 := f.put("k", "v1")
		f.state(contracts.VersioningSuspended)
		f.put("k", "null")
		null := f.versions("k")[0]
		out := f.apply(planOf(expire))
		requireDeleted(t, out, "cur",
			contracts.LifecycleDeletion{VersionID: contracts.VersionNull, RuleID: "cur"})
		f.requireIDs(contracts.VersionNull, v1.VersionID)
		if !f.versions("k")[0].Marker || f.hasData(&null) {
			t.Fatalf("the null version was not replaced by a marker: %+v", f.versions("k"))
		}
	})
	t.Run("noncurrent null", func(t *testing.T) {
		f, _ := newClockFixture(t)
		f.put("k", "legacy")
		f.state(contracts.VersioningEnabled)
		v2 := f.put("k", "v2")
		f.state(contracts.VersioningSuspended)
		out := f.apply(planOf(expire))
		requireDeleted(t, out, "cur",
			contracts.LifecycleDeletion{VersionID: contracts.VersionNull, RuleID: "cur"})
		f.requireIDs(contracts.VersionNull, v2.VersionID)
		f.requireSince(time.Time{}, midnight)
		if f.hasData(nil) || f.body(vref("k", v2.VersionID)) != "v2" {
			t.Fatal("the noncurrent null version is left, or the current one is not kept")
		}
	})
	t.Run("legacy key", func(t *testing.T) {
		// The null marker that replaces the key's only version is left the
		// only version, and stays, however the decider would answer: no
		// run removes a marker it added.
		f, _ := newClockFixture(t)
		f.put("k", "legacy")
		f.state(contracts.VersioningSuspended)
		d := planOf(expire)
		d.sole = func(string, contracts.LifecycleVersion) (string, error) { return "eodm", nil }
		requireDeleted(t, f.apply(d), "cur",
			contracts.LifecycleDeletion{VersionID: contracts.VersionNull, RuleID: "cur"})
		f.requireIDs(contracts.VersionNull)
		if !f.versions("k")[0].Marker || f.hasData(nil) || len(d.soles) != 0 {
			t.Fatalf("versions %+v, sole marker asked %d times", f.versions("k"), len(d.soles))
		}
		// A later run removes it.
		later := &stubDecider{sole: d.sole}
		requireDeleted(
			t,
			f.apply(later),
			"",
			contracts.LifecycleDeletion{
				VersionID: contracts.VersionNull,
				RuleID:    "eodm",
				Marker:    true,
			},
		)
		f.requireGone()
	})
}

// A plan that is not one of the key as it is fails, and changes nothing.
func TestApplyLifecycleRefusesWrongPlans(t *testing.T) {
	f, _ := newClockFixture(t)
	f.state(contracts.VersioningEnabled)
	f.put("k", "v1")
	v2 := f.put("k", "v2")
	ids := f.ids("k")
	for name, p := range map[string]contracts.LifecyclePlan{
		"current":     {Remove: map[string]string{v2.VersionID: "nc"}},
		"unknown":     {Remove: map[string]string{newVersionID(1): "nc"}},
		"marker time": {ExpireCurrentRule: "cur"},
	} {
		if _, err := f.s.ApplyLifecycle(f.ctx, "bucket", "k", planOf(p)); err == nil {
			t.Errorf("%s: applied", name)
		}
		f.requireIDs(ids...)
	}
	marker := f.del(oref("bucket", "k"))
	if _, err := f.s.ApplyLifecycle(f.ctx, "bucket", "k", planOf(expire)); err == nil {
		t.Error("a current marker was expired")
	}
	f.requireIDs(append([]string{marker.VersionID}, ids...)...)

	failing := errors.New("decider failed")
	_, err := f.s.ApplyLifecycle(f.ctx, "bucket", "k", &stubDecider{
		plan: func(*contracts.LifecycleKey) (contracts.LifecyclePlan, error) {
			return contracts.LifecyclePlan{}, failing
		},
	})
	requireIs(t, "a failing decider", err, failing)
}

// A key whose lock is held is not waited for.
func TestApplyLifecycleBusy(t *testing.T) {
	f := newFixture(t)
	f.put("k", "v")
	unlock := f.s.locks.lock("bucket", "k")
	d := planOf(expire)
	out := f.apply(d)
	unlock()
	if !out.Busy || len(d.keys) != 0 || f.body(oref("bucket", "k")) != "v" {
		t.Fatalf("outcome %+v, decided %d", out, len(d.keys))
	}
}

// Each version's tags are read as GetObjectTagging reads them, those
// PutObjectTagging stored over those it was written with, and a marker
// has none.
func TestApplyLifecycleTags(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	v1 := f.putIn(
		"k",
		"v1",
		contracts.PutObjectInput{Tags: []contracts.Tag{{Key: "a", Value: "1"}}},
	)
	retag := []contracts.Tag{{Key: "a", Value: "2"}}
	if _, err := f.s.PutObjectTagging(f.ctx, vref("k", v1.VersionID), retag, nil); err != nil {
		t.Fatal(err)
	}
	f.del(oref("bucket", "k"))
	d := &stubDecider{}
	f.apply(d)
	vs := d.keys[0].Versions
	if tags, err := vs[0].Tags(); err != nil || tags != nil {
		t.Fatalf("marker tags %v, %v", tags, err)
	}
	for range 2 {
		requireTagsOf(t, vs[1], retag)
	}
}

// A manifest written before noncurrentSince was recorded is decided on
// with the times derived from the successors, and changed with them
// recorded.
func TestApplyLifecycleOnAV0_11_0Manifest(t *testing.T) {
	f, clk := newClockFixture(t)
	f.state(contracts.VersioningEnabled)
	f.put("k", "v1")
	clk.set(hoursIn(1))
	v2 := f.put("k", "v2")
	clk.set(hoursIn(2))
	f.put("k", "v3")
	f.stripNoncurrent()

	d := planOf(contracts.LifecyclePlan{Remove: map[string]string{v2.VersionID: "nc"}})
	requireDeleted(
		t,
		f.apply(d),
		"",
		contracts.LifecycleDeletion{VersionID: v2.VersionID, RuleID: "nc"},
	)
	vs := d.keys[0].Versions
	if !vs[1].NoncurrentSince.Equal(hoursIn(2)) || !vs[2].NoncurrentSince.Equal(hoursIn(1)) {
		t.Fatalf("decided %+v", vs)
	}
	f.requireSince(time.Time{}, hoursIn(1))
}

// A change that stops after its commit, before the data it removed is
// collected, reads as committed, and DeleteBucket removes what it left.
func TestApplyLifecycleCrashes(t *testing.T) {
	f, _ := newClockFixture(t)
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	v2 := f.put("k", "v2")
	plan := contracts.LifecyclePlan{
		ExpireCurrentRule: "cur", MarkerTime: midnight,
		Remove: map[string]string{v1.VersionID: "nc"},
	}

	crashAt(f, stageLifecycleDecided)
	_, err := f.s.ApplyLifecycle(f.ctx, "bucket", "k", planOf(plan))
	requireIs(t, "stopped before applying", err, errCrash)
	f.requireIDs(v2.VersionID, v1.VersionID)

	crashAt(f, stageCommitted)
	_, err = f.s.ApplyLifecycle(f.ctx, "bucket", "k", planOf(plan))
	requireIs(t, "stopped after the commit", err, errCrash)
	ids := f.ids("k")
	if len(ids) != 2 || ids[1] != v2.VersionID || f.backendObjects() != 2 {
		t.Fatalf("versions %v, %d backend objects; want a marker over v2 and v1's garbage",
			ids, f.backendObjects())
	}
	_, err = f.s.GetObject(f.ctx, vref("k", v1.VersionID))
	requireIs(t, "the removed version", err, storage.ErrVersionNotFound)

	for _, id := range ids {
		f.del(vref("k", id))
	}
	if err = f.deleteBucket(); err != nil {
		t.Fatal(err)
	}
	if n := f.backendObjects(); n != 0 {
		t.Fatalf("DeleteBucket left %d backend objects", n)
	}
}

// scanAll scans "bucket" n keys at a time, calling between after each page,
// and returns the keys in the order scanned.
func (f *fixture) scanAll(n int, between func()) []string {
	f.t.Helper()
	var all []string
	cursor := ""
	for range 1000 {
		keys, next, err := f.s.ScanKeys(f.ctx, "bucket", cursor, n)
		if err != nil {
			f.t.Fatalf("scan after %q: %v", cursor, err)
		}
		if len(keys) > n {
			f.t.Fatalf("scan of %d returned %d keys", n, len(keys))
		}
		all = append(all, keys...)
		if next == "" {
			return all
		}
		cursor = next
		if between != nil {
			between()
		}
	}
	f.t.Fatal("the scan does not end")
	return nil
}

// putKeys writes n keys named by prefix and a letter, and returns them.
func (f *fixture) putKeys(prefix string, n int) []string {
	f.t.Helper()
	keys := make([]string, 0, n)
	for i := range n {
		key := prefix + string(rune('a'+i))
		f.put(key, "x")
		keys = append(keys, key)
	}
	return keys
}

// Scanning by any page size returns every key once.
func TestScanKeys(t *testing.T) {
	f := newFixture(t)
	want := f.putKeys("key-", 7)
	// A temp file of an entry being written is no key.
	tmp := filepath.Join(indexDir, "bucket", keyHash("pending")+".tmp-x")
	if err := f.s.root.WriteFile(tmp, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= len(want)+1; n++ {
		got := f.scanAll(n, nil)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("scan by %d: %v, want %v", n, got, want)
		}
	}
}

// Keys indexed and removed meanwhile neither repeat a key nor stop the
// scan; every key there all along is scanned.
func TestScanKeysWhileKeysChange(t *testing.T) {
	f := newFixture(t)
	want := f.putKeys("key-", 7)
	removed := map[string]bool{"key-c": true, "key-f": true}
	pages := 0
	got := f.scanAll(2, func() {
		pages++
		if pages == 1 {
			for key := range removed {
				f.del(oref("bucket", key))
			}
			f.putKeys("new-", 5)
		}
	})
	seen := map[string]bool{}
	for _, key := range got {
		if seen[key] {
			t.Fatalf("key %q scanned twice", key)
		}
		seen[key] = true
	}
	for _, key := range want {
		if !removed[key] && !seen[key] {
			t.Fatalf("key %q not scanned: %v", key, got)
		}
	}
}

func TestScanKeysRefuses(t *testing.T) {
	f := newFixture(t)
	for name, scan := range map[string]struct {
		ctx    context.Context //nolint:revive // the context the scan runs in
		bucket string
		cursor string
		n      int
	}{
		"no keys":  {ctx: f.ctx, bucket: "bucket", n: 0},
		"cursor":   {ctx: f.ctx, bucket: "bucket", cursor: "../x", n: 1},
		"no pin":   {ctx: context.Background(), bucket: "bucket", n: 1},
		"unpinned": {ctx: f.ctx, bucket: "missing", n: 1},
	} {
		if _, _, err := f.s.ScanKeys(scan.ctx, scan.bucket, scan.cursor, scan.n); err == nil {
			t.Errorf("%s: scanned", name)
		}
	}
}

// envelope returns a stored lifecycle configuration of generation gen.
func envelope(t *testing.T, gen string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"generation": gen, "document": "<L/>"})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// targets returns the names of the lifecycle targets and their configs.
func targets(t *testing.T, s *Service) map[string]contracts.LifecycleTarget {
	t.Helper()
	ts, err := s.LifecycleTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]contracts.LifecycleTarget{}
	for _, target := range ts {
		out[target.Ref.Name] = target
	}
	return out
}

func TestLifecycleTargets(t *testing.T) {
	f := newFixture(t)
	if _, err := f.s.CreateBucket(context.Background(), "other", nil); err != nil {
		t.Fatal(err)
	}
	if got := targets(t, f.s); len(got) != 0 {
		t.Fatalf("targets without configurations: %v", got)
	}
	r := ref(t, f.s, "bucket")
	doc := envelope(t, r.Generation)
	if err := f.s.PutBucketConfig(f.ctx, "bucket", contracts.ConfigLifecycle, doc); err != nil {
		t.Fatal(err)
	}
	got := targets(t, f.s)
	if len(got) != 1 || got["bucket"].Ref != r || string(got["bucket"].Config) != string(doc) {
		t.Fatalf("targets %v", got)
	}

	// A damaged configuration is skipped, and keeps no other bucket's.
	f.writeLifecycle("other", []byte("{"))
	if got = targets(t, f.s); len(got) != 1 || got["bucket"].Ref != r {
		t.Fatalf("targets with a damaged configuration: %v", got)
	}
}

// A configuration that cannot be read is skipped, and keeps no other
// bucket's: only a failure to list the buckets fails LifecycleTargets.
func TestLifecycleTargetsSkipAnUnreadableConfiguration(t *testing.T) {
	f := newFixture(t)
	if _, err := f.s.CreateBucket(context.Background(), "other", nil); err != nil {
		t.Fatal(err)
	}
	r := ref(t, f.s, "bucket")
	f.writeLifecycle("bucket", envelope(t, r.Generation))
	// A directory in place of the file fails the read.
	if err := f.s.root.Mkdir(configFile(contracts.ConfigLifecycle, "other"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := targets(t, f.s); len(got) != 1 || got["bucket"].Ref != r {
		t.Fatalf("targets with an unreadable configuration: %v", got)
	}
}

// writeLifecycle writes doc as bucket's lifecycle configuration file, as
// it was left on disk.
func (f *fixture) writeLifecycle(bucket string, doc []byte) {
	f.t.Helper()
	if err := f.s.writeFile(configFile(contracts.ConfigLifecycle, bucket), doc); err != nil {
		f.t.Fatal(err)
	}
}

// A lifecycle configuration goes with its bucket, and one a bucket of the
// same name left, as a version that does not remove it on DeleteBucket
// would, is not the new bucket's.
func TestALifecycleConfigurationGoesWithItsBucket(t *testing.T) {
	f := newFixture(t)
	doc := envelope(t, ref(t, f.s, "bucket").Generation)
	f.writeLifecycle("bucket", doc)
	if err := f.deleteBucket(); err != nil {
		t.Fatal(err)
	}
	if f.exists(configFile(contracts.ConfigLifecycle, "bucket")) {
		t.Fatal("DeleteBucket left the lifecycle configuration")
	}
	f.writeLifecycle("bucket", doc)
	if _, err := f.s.CreateBucket(context.Background(), "bucket", nil); err != nil {
		t.Fatal(err)
	}
	if f.exists(configFile(contracts.ConfigLifecycle, "bucket")) {
		t.Fatal("CreateBucket kept a lifecycle configuration left behind")
	}
	f.writeLifecycle("bucket", doc)
	if got := targets(t, f.s); len(got) != 0 {
		t.Fatalf("a configuration of another generation is a target: %v", got)
	}
}

// uploadIDs returns the IDs of the uploads into "bucket".
func (f *fixture) uploadIDs() []string {
	f.t.Helper()
	uploads, err := f.s.ListMultipartUploads(f.ctx, "bucket")
	if err != nil {
		f.t.Fatal(err)
	}
	ids := make([]string, len(uploads))
	for i, u := range uploads {
		ids[i] = u.UploadID
	}
	return ids
}

// abortLifecycle runs AbortLifecycleUpload on upload id of key, which must
// not be busy, and reports whether it aborted it.
func (f *fixture) abortLifecycle(key, id string, initiated time.Time) (bool, error) {
	f.t.Helper()
	out, err := f.s.AbortLifecycleUpload(f.ctx, "bucket", key, id, initiated)
	if out == nil {
		return false, err
	}
	if out.Busy {
		f.t.Fatalf("abort of upload %s: busy", id)
	}
	return out.Aborted, err
}

func TestAbortLifecycleUpload(t *testing.T) {
	f, _ := newClockFixture(t)
	id, _ := f.upload("k", "part")
	requireNotAborted(f, id)
	done, err := f.abortLifecycle("k", id, clockStart)
	if !done || err != nil || f.exists(uploadDir(id)) || len(f.uploadIDs()) != 0 {
		t.Fatalf("abort: %v, %v", done, err)
	}
	done, err = f.abortLifecycle("k", id, clockStart)
	if done || err != nil {
		t.Fatalf("second abort: %v, %v", done, err)
	}

	// The client abort removes all of it too.
	id, _ = f.upload("k", "part")
	err = f.s.AbortMultipartUpload(f.ctx, "bucket", "k", id)
	if err != nil || f.exists(uploadDir(id)) {
		t.Fatalf("client abort: %v", err)
	}
}

// requireNotAborted fails unless upload id of "k", initiated at clockStart,
// is kept by lifecycle aborts that name another one.
func requireNotAborted(f *fixture, id string) {
	f.t.Helper()
	other := strings.Repeat("A", 26)
	for name, try := range map[string]func() (bool, error){
		"another initiation": func() (bool, error) {
			return f.abortLifecycle("k", id, hoursIn(1))
		},
		"another key": func() (bool, error) {
			return f.abortLifecycle("other", id, clockStart)
		},
		"another upload": func() (bool, error) {
			return f.abortLifecycle("k", other, clockStart)
		},
	} {
		if done, err := try(); done || err != nil {
			f.t.Fatalf("%s: aborted %v, %v", name, done, err)
		}
	}
	if !slices.Equal(f.uploadIDs(), []string{id}) {
		f.t.Fatal("the upload was not kept")
	}
}

// An upload whose manifest is removed, as a crash in an abort right after
// it leaves it, is gone.
func TestAnUploadWithoutItsManifestIsGone(t *testing.T) {
	f, _ := newClockFixture(t)
	id, parts := f.upload("k", "part")
	if err := f.s.root.Remove(filepath.Join(uploadDir(id), manifestFile)); err != nil {
		t.Fatal(err)
	}
	if len(f.uploadIDs()) != 0 {
		t.Fatal("the upload is listed")
	}
	_, _, err := f.s.UploadPart(f.ctx, "bucket", "k", id, 2, strings.NewReader("x"))
	requireIs(t, "upload part", err, contracts.ErrNoSuchUpload)
	_, err = f.s.CompleteMultipartUpload(f.ctx, "bucket", "k", id, parts, contracts.Precondition{})
	requireIs(t, "complete", err, contracts.ErrNoSuchUpload)
	requireIs(t, "abort", f.s.AbortMultipartUpload(f.ctx, "bucket", "k", id),
		contracts.ErrNoSuchUpload)
	if done, aErr := f.abortLifecycle("k", id, clockStart); done || aErr != nil {
		t.Fatalf("lifecycle abort: %v, %v", done, aErr)
	}
}

// requireNoUploadWrite fails unless "k" has no version, and nothing of a
// completion is left in the backend.
func (f *fixture) requireNoUploadWrite(err error) {
	f.t.Helper()
	requireIs(f.t, "the completion", err, contracts.ErrNoSuchUpload)
	if ids := f.ids("k"); len(ids) != 0 || f.backendObjects() != 0 {
		f.t.Fatalf("versions %v, %d backend objects", ids, f.backendObjects())
	}
}

// A lifecycle abort that wins over a versioned completion, whose data is
// written, fails the completion as NoSuchUpload, and removes its data.
func TestALifecycleAbortRacingAVersionedCompletion(t *testing.T) {
	f, _ := newClockFixture(t)
	f.state(contracts.VersioningEnabled)
	id, parts := f.upload("k", "part")
	f.s.hook = func(stage string) error {
		if stage != stageWritten {
			return nil
		}
		f.s.hook = nil
		done, err := f.abortLifecycle("k", id, clockStart)
		if err == nil && !done {
			err = errors.New("the upload was not aborted")
		}
		return err
	}
	_, err := f.s.CompleteMultipartUpload(f.ctx, "bucket", "k", id, parts, contracts.Precondition{})
	f.requireNoUploadWrite(err)
}

// A completion whose upload is aborted after it was looked up, before its
// parts are read, fails as NoSuchUpload, not as a missing part.
func TestAnAbortBeforeTheCompletionReadsItsParts(t *testing.T) {
	f := newFixture(t)
	id, parts := f.upload("k", "part")
	unlock := f.s.locks.lock("bucket", "k")
	errs := make(chan error, 1)
	go func() {
		errs <- errOf(f.s.CompleteMultipartUpload(f.ctx, "bucket", "k", id, parts,
			contracts.Precondition{}))
	}()
	// The completion in place waits for the lock to read the parts.
	waitForLockWaiters(t, f.s, "bucket/k", 2)
	if err := f.s.AbortMultipartUpload(f.ctx, "bucket", "k", id); err != nil {
		t.Fatal(err)
	}
	unlock()
	f.requireNoUploadWrite(<-errs)
}

// A versioned completion racing an abort either commits or fails as
// NoSuchUpload, whichever point of its part reads the abort lands at.
func TestAnAbortRacingAVersionedCompletion(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	raceCompletions(f)
	abortWhileReadingParts(f)
	vs := f.versions("k")
	if objects := f.backendObjects(); objects != len(vs) {
		t.Fatalf("%d backend objects for %d versions", objects, len(vs))
	}
}

// A plan that removes noncurrent versions of a key in an Unversioned
// bucket, which has none, is refused, and the key is kept.
func TestApplyLifecycleRefusesRemoveWhenUnversioned(t *testing.T) {
	f := newFixture(t)
	f.put("k", "data")
	_, err := f.s.ApplyLifecycle(f.ctx, "bucket", "k", planOf(contracts.LifecyclePlan{
		ExpireCurrentRule: "cur", MarkerTime: midnight,
		Remove: map[string]string{contracts.VersionNull: "nc"},
	}))
	if err == nil || f.body(oref("bucket", "k")) != "data" {
		t.Fatalf("a plan removing noncurrent versions of an unversioned key: %v", err)
	}
}

// A sole-marker removal that empties a key whose removed versions include
// its legacy null version, and stops right after committing the empty
// manifest, leaves a key with no versions, the object at keyHash hidden
// behind it, and DeleteBucket removes what is left.
func TestApplyLifecycleCrashEmptyingALegacyKey(t *testing.T) {
	f := newFixture(t)
	f.put("k", "legacy")
	f.state(contracts.VersioningEnabled)
	marker := f.del(oref("bucket", "k"))
	f.requireIDs(marker.VersionID, contracts.VersionNull)
	d := planOf(contracts.LifecyclePlan{Remove: map[string]string{contracts.VersionNull: "nc"}})
	d.sole = func(string, contracts.LifecycleVersion) (string, error) { return "eodm", nil }

	crashAt(f, stageCommitted)
	_, err := f.s.ApplyLifecycle(f.ctx, "bucket", "k", d)
	requireIs(t, "the stopped removal", err, errCrash)
	if !f.hasManifest() || !f.hasData(nil) {
		t.Fatal("the removal went past its commit")
	}
	f.requireIDs()
	_, err = f.s.GetObject(f.ctx, oref("bucket", "k"))
	requireIs(t, "get", err, storage.ErrObjectNotFound)
	_, err = f.s.GetObject(f.ctx, vref("k", contracts.VersionNull))
	requireIs(t, "get the null version", err, storage.ErrVersionNotFound)

	v1 := f.put("k", "v1")
	f.requireIDs(v1.VersionID)
	f.del(vref("k", v1.VersionID))
	if err = f.deleteBucket(); err != nil {
		t.Fatal(err)
	}
	if n := f.backendObjects(); n != 0 {
		t.Fatalf("DeleteBucket left %d backend objects", n)
	}
}

// A lifecycle abort of an upload whose key's lock is held does not wait:
// the upload is reported busy, and kept.
func TestAbortLifecycleUploadBusy(t *testing.T) {
	f, _ := newClockFixture(t)
	id, _ := f.upload("k", "part")
	unlock := f.s.locks.lock("bucket", "k")
	out, err := f.s.AbortLifecycleUpload(f.ctx, "bucket", "k", id, clockStart)
	unlock()
	if err != nil || !out.Busy || out.Aborted || !slices.Equal(f.uploadIDs(), []string{id}) {
		t.Fatalf("abort of a busy upload: %+v, %v", out, err)
	}
}

// A completion in place racing a client abort, which takes no key lock,
// either commits or fails as NoSuchUpload, also when the abort lands while
// its parts are read.
func TestAnAbortRacingACompletionInPlace(t *testing.T) {
	f := newFixture(t)
	raceCompletions(f)
	abortWhileReadingParts(f)
}

// abortWhileReadingParts completes an upload of "k" that a client abort
// removes once its parts are checked, before they are read, and fails
// unless the completion fails as NoSuchUpload and writes nothing.
func abortWhileReadingParts(f *fixture) {
	f.t.Helper()
	before := f.ids("k")
	id, parts := f.upload("k", "part")
	f.s.hook = func(stage string) error {
		if stage != stagePartsChecked {
			return nil
		}
		f.s.hook = nil
		return f.s.AbortMultipartUpload(f.ctx, "bucket", "k", id)
	}
	_, err := f.s.CompleteMultipartUpload(f.ctx, "bucket", "k", id, parts, contracts.Precondition{})
	requireIs(f.t, "the completion", err, contracts.ErrNoSuchUpload)
	if ids := f.ids("k"); !slices.Equal(ids, before) {
		f.t.Fatalf("versions %v, want %v", ids, before)
	}
}

// raceCompletions completes uploads of "k" racing client aborts of them,
// and fails unless each completion commits or fails as NoSuchUpload.
func raceCompletions(f *fixture) {
	f.t.Helper()
	big := strings.Repeat("x", 4<<20)
	for range 10 {
		id, parts := f.upload("k", big)
		errs := make(chan error, 2)
		go func() {
			errs <- errOf(f.s.CompleteMultipartUpload(f.ctx, "bucket", "k", id, parts,
				contracts.Precondition{}))
		}()
		go func() {
			err := f.s.AbortMultipartUpload(f.ctx, "bucket", "k", id)
			if errors.Is(err, contracts.ErrNoSuchUpload) {
				err = nil
			}
			errs <- err
		}()
		for range 2 {
			if err := <-errs; err != nil && !errors.Is(err, contracts.ErrNoSuchUpload) {
				f.t.Fatalf("a completion racing an abort: %v", err)
			}
		}
	}
}
