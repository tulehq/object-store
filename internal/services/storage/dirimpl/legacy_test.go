package dirimpl

import (
	"context"
	"maps"
	"testing"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/config"
	"github.com/gasmod/gas/config/configtest"
	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// openTestService returns an initialized Service on root, closed when the
// test ends unless the test closes it first.
func openTestService(t *testing.T, root string) *Service {
	t.Helper()
	cfg := config.New(config.WithProvider(configtest.NewStaticProvider(nil)))
	if err := cfg.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}
	s := NewService(&Settings{RootDir: root}, contracts.SystemClock)(cfg, &gas.NopLogger{})
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// pinnedFixture is a fixture on s, pinned to its bucket "bucket".
func pinnedFixture(t *testing.T, s *Service) *fixture {
	t.Helper()
	ctx, release := pin(t, s)
	return &fixture{t: t, s: s, ctx: ctx, release: release}
}

// legacyACL is the ACL document the legacy object "k" is written with.
var legacyACL = []byte(`{"grants":"public-read"}`)

// Keys written while their bucket was Unversioned keep the path they had
// before versioning existed, and, once it is enabled, are their null
// versions, with their tags and ACL, until a write replaces them, also
// after the store is opened again. TestDataDirFromV0_10_1 checks the same
// on a root v0.10.1 wrote.
func TestLegacyKeysBecomeNullVersions(t *testing.T) {
	root := t.TempDir()
	s := openTestService(t, root)
	if _, err := s.CreateBucket(context.Background(), "bucket", nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	f := pinnedFixture(t, s)
	uploadID, parts := writeLegacy(f)

	f.state(contracts.VersioningEnabled)
	requireLegacyNull(f, oref("bucket", "k"))
	v1 := f.put("k", "v1")
	requireNewID(t, contracts.VersioningEnabled, v1.VersionID)
	requireLegacyNull(f, vref("k", contracts.VersionNull))
	requireObjectTags(f, vref("k", v1.VersionID), map[string]string{})
	done, err := s.CompleteMultipartUpload(f.ctx, "bucket", "up", uploadID, parts,
		contracts.Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	requireNewID(t, contracts.VersioningEnabled, done.VersionID)

	f.state(contracts.VersioningSuspended)
	if n := f.put("k", "suspended"); n.VersionID != contracts.VersionNull {
		t.Fatalf("a Suspended write answered version %q", n.VersionID)
	}
	if f.hasData(nil) {
		t.Fatal("the replaced legacy null version's data is left")
	}
	f.del(vref("other", contracts.VersionNull))
	requireReplacedLegacy(f, v1.VersionID, done.VersionID)

	f.release()
	if err = s.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	requireReplacedLegacy(pinnedFixture(t, openTestService(t, root)), v1.VersionID, done.VersionID)
}

// writeLegacy writes, in the Unversioned "bucket", "k" with tags it is
// retagged over and an ACL, and "other", and starts an upload of "up",
// returning its ID and parts, and checks that they keep the legacy path.
func writeLegacy(f *fixture) (string, []contracts.CompletedPart) {
	f.t.Helper()
	f.putIn("k", "legacy", contracts.PutObjectInput{
		Tags: []contracts.Tag{{Key: "stage", Value: "old"}}, ACL: legacyACL,
	})
	if _, err := f.s.PutObjectTagging(f.ctx, oref("bucket", "k"),
		[]contracts.Tag{{Key: "stage", Value: "new"}}, nil); err != nil {
		f.t.Fatal(err)
	}
	f.put("other", "other legacy")
	uploadID, parts := f.upload("up", "pending")
	if f.hasManifest() || f.exists(versioningFile("bucket")) || !f.hasData(nil) ||
		!f.exists(tagFile("bucket", "k")) || !f.exists(aclFile("bucket", "k")) {
		f.t.Fatal("an Unversioned write did not keep the legacy path")
	}
	return uploadID, parts
}

// requireLegacyNull checks that ref reads as the legacy object "k", the
// null version, with its tags and ACL.
func requireLegacyNull(f *fixture, ref contracts.ObjectRef) {
	f.t.Helper()
	body, info := f.read(ref)
	if body != "legacy" || info.VersionID != contracts.VersionNull {
		f.t.Fatalf("%+v: %q as version %q", ref, body, info.VersionID)
	}
	requireObjectTags(f, ref, map[string]string{"stage": "new"})
	acl, id, err := f.s.GetObjectACL(f.ctx, ref, nil)
	if err != nil || string(acl) != string(legacyACL) || id != contracts.VersionNull {
		f.t.Fatalf("ACL of %+v: %q of %q, %v", ref, acl, id, err)
	}
}

// requireObjectTags checks the tags of ref.
func requireObjectTags(f *fixture, ref contracts.ObjectRef, want map[string]string) {
	f.t.Helper()
	tags, err := f.s.GetObjectTagging(f.ctx, ref)
	if err != nil {
		f.t.Fatalf("tags of %+v: %v", ref, err)
	}
	got := map[string]string{}
	for _, tag := range tags {
		got[tag.Key] = tag.Value
	}
	if !maps.Equal(got, want) {
		f.t.Fatalf("tags of %+v: %v, want %v", ref, got, want)
	}
}

// requireReplacedLegacy checks the store as TestLegacyKeysBecomeNullVersions
// leaves it: "k" with a null version that replaced the legacy one and
// kept none of its tags or ACL, over v1; "up" completed as version done;
// "other" deleted permanently.
func requireReplacedLegacy(f *fixture, v1, done string) {
	f.t.Helper()
	f.requireIDs(contracts.VersionNull, v1)
	if got := f.body(vref("k", contracts.VersionNull)); got != "suspended" {
		f.t.Fatalf("null version of k: %q", got)
	}
	if got := f.body(vref("k", v1)); got != "v1" {
		f.t.Fatalf("version v1 of k: %q", got)
	}
	requireObjectTags(f, vref("k", contracts.VersionNull), map[string]string{})
	_, _, err := f.s.GetObjectACL(f.ctx, vref("k", contracts.VersionNull), nil)
	requireIs(f.t, "ACL of the new null version", err, contracts.ErrNoSuchBucketConfig)

	if body, info := f.read(oref("bucket", "up")); body != "pending" || info.VersionID != done {
		f.t.Fatalf("completed upload: %q as version %q, want %q", body, info.VersionID, done)
	}
	if ids := f.ids("other"); len(ids) != 0 {
		f.t.Fatalf("versions of the deleted key: %v", ids)
	}
	_, err = f.s.GetObject(f.ctx, oref("bucket", "other"))
	requireIs(f.t, "get the deleted key", err, storage.ErrObjectNotFound)
	out, err := f.s.ListObjectVersions(f.ctx, "bucket",
		contracts.ListObjectVersionsInput{MaxKeys: 10})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, v := range out.Versions {
		if v.Key == "other" {
			f.t.Fatalf("the deleted key is listed: %+v", v)
		}
	}
}
