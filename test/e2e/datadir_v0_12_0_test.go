//go:build e2e

package e2e

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"
)

// The test here loads a storage root v0.12.0 wrote, the last release
// before event notifications, the one testdata/genroot made (see
// testdata/genroot/README.md), and records events of the data it holds.

// versionedRootFixture is the fixture.json genroot writes beside the
// v0.12.0 root.
type versionedRootFixture struct {
	// Written is a time after the last write, which the test's storage
	// clock starts at.
	Written  time.Time                 `json:"written"`
	Versions map[string]fixtureVersion `json:"versions"` // by label
	// NoncurrentSince is when the versions labeled became noncurrent, as
	// v0.12.0 recorded it, a successor ago: the LastModified of the
	// successor genroot deleted.
	NoncurrentSince map[string]time.Time `json:"noncurrentSince"`
	Upload          struct {
		Bucket     string `json:"bucket"`
		Key        string `json:"key"`
		ID         string `json:"id"`
		PartETag   string `json:"partETag"`
		PartSHA256 string `json:"partSHA256"`
		PartSize   int64  `json:"partSize"`
	} `json:"upload"`
}

// fixtureVersion is a version or delete marker genroot wrote; a marker
// has no body.
type fixtureVersion struct {
	LastModified time.Time `json:"lastModified"`
	Bucket       string    `json:"bucket"`
	Key          string    `json:"key"`
	VersionID    string    `json:"versionId"`
	SHA256       string    `json:"sha256"`
	ETag         string    `json:"etag"`
	Size         int64     `json:"size"`
}

// id returns the version ID of the version labeled label.
func (fx *versionedRootFixture) id(t *testing.T, label string) string {
	t.Helper()
	v, ok := fx.Versions[label]
	if !ok {
		t.Fatalf("the fixture has no version %s", label)
	}
	return v.VersionID
}

// listed is a version or marker a listing holds: the fixture's labeled
// label, or, unlabeled, one the test wrote.
type listed struct {
	label string
	entry
}

// entry returns the listing entry of the fixture's version label, the
// key's latest or not; a label of a marker ends in "-marker".
func (fx *versionedRootFixture) entry(t *testing.T, label string, latest bool) listed {
	t.Helper()
	v := fx.Versions[label]
	return listed{label: label, entry: entry{
		Key: v.Key, VersionID: fx.id(t, label), Marker: strings.HasSuffix(label, "-marker"),
		Latest: latest,
	}}
}

// historyListing is the listing of "history" as v0.12.0 left it, but
// with legacy.txt's versions legacy, newest first.
func historyListing(t *testing.T, fx *versionedRootFixture, legacy ...listed) []listed {
	t.Helper()
	out := []listed{
		fx.entry(t, "archive-v3", true), fx.entry(t, "archive-v1", false),
		fx.entry(t, "doc-marker", true), fx.entry(t, "doc-v3", false),
		fx.entry(t, "doc-v2", false), fx.entry(t, "doc-v1", false),
	}
	out = append(out, legacy...)
	return append(out, fx.entry(t, "report-v1", true), fx.entry(t, "untouched-null", true))
}

// The labels of the bodies genroot wrote, as its README lists them.
var fixtureBodies = []string{
	"legacy-null", "untouched-null", "legacy-v1", "doc-v1", "doc-v2", "doc-v3",
	"report-v1", "note-v1", "note-null", "gone-v1", "archive-v1", "archive-v3",
}

// fixtureRules is the lifecycle configuration genroot gave "history".
func fixtureRules() []types.LifecycleRule {
	return []types.LifecycleRule{
		{
			ID: aws.String("expire-reports"), Status: types.ExpirationStatusEnabled,
			Filter: prefixFilter("reports/"), Expiration: days(1),
			NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{
				NoncurrentDays: aws.Int32(30),
			},
		},
		{
			ID: aws.String("expire-archive"), Status: types.ExpirationStatusEnabled,
			Filter: prefixFilter("archive/"),
			NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{
				NoncurrentDays: aws.Int32(4),
			},
		},
		{
			ID: aws.String("disabled-all"), Status: types.ExpirationStatusDisabled,
			Filter: prefixFilter(""), Expiration: days(1),
		},
	}
}

// The bodies the test writes over the fixture's keys.
const (
	legacyV2 = "legacy.txt, written over after the upgrade"
	goneNull = "gone.txt, written over its null delete marker while Suspended"
	docV4    = "doc.txt, written over its delete marker after a restart"
)

// fixtureEvents are the events the test configures "history" to send.
var fixtureEvents = []types.Event{
	"s3:ObjectCreated:*", "s3:ObjectRemoved:*", "s3:ObjectTagging:*",
	"s3:LifecycleExpiration:*",
}

// TestDataDirFromV0_12_0 starts the server, in the notification stack,
// on a root v0.12.0 wrote: its versions, delete marker, tags, ACL,
// lifecycle configuration and upload read as they were written, and so
// does the listing's ETag and size of each version. Then, with
// notifications configured, operations on its legacy null version and a
// write over it send their events in order, the first event creates the
// event state v0.12.0 never had, which a restart resumes from; a write
// over a Suspended null delete marker replaces it; and the lifecycle
// rules v0.12.0 stored send their expiration events once the storage
// clock reaches them, a noncurrent version's counted from when v0.12.0
// recorded it became noncurrent.
//
// Only the storage clock is fake, starting when genroot finished, as in
// TestLifecycleEvents: delivery keeps the system clock.
func TestDataDirFromV0_12_0(t *testing.T) {
	var fx versionedRootFixture
	dir := extractTarball(t, "testdata/v0.12.0-root.tar.gz", &fx)
	if fx.Upload.ID == "" || fx.Written.IsZero() {
		t.Fatal("the fixture has no fixture.json")
	}
	requireNoFile(t, dir, eventsDir, "in the v0.12.0 root")
	requireNoFile(t, dir, eventSeqFile, "in the v0.12.0 root")
	clk := newFakeClock(fx.Written)
	s := startNotificationStackWith(t, stackOptions{live: liveOptions{root: dir, clock: clk}})

	if !t.Run("loaded unchanged", func(t *testing.T) {
		requireFixtureRoot(t, s, &fx)
		requireNoEvents(t, dir)
	}) {
		return
	}
	q := s.queueFor(t, "history", queueConf("fixture", fixtureEvents))
	requireNoEvents(t, dir)

	var written string
	if !t.Run("events of the legacy null version", func(t *testing.T) {
		written = requireLegacyNullEvents(t, s, &fx, q)
	}) {
		return
	}
	if !t.Run("a write over a Suspended null marker", func(t *testing.T) {
		requireSuspendedMarkerWrite(t, s, &fx)
	}) {
		return
	}
	if !t.Run("lifecycle expiration", func(t *testing.T) {
		requireFixtureExpiration(t, s, &fx, clk, q, written)
	}) {
		return
	}
	t.Run("after a restart", func(t *testing.T) {
		requireResumedSequence(t, s, &fx, q)
	})
}

// requireFixtureRoot checks the root as v0.12.0 left it.
func requireFixtureRoot(t *testing.T, s *notificationStack, fx *versionedRootFixture) {
	t.Helper()
	c := s.s3
	out, err := c.ListBuckets(t.Context(), &s3.ListBucketsInput{})
	requireNoErr(t, err, "list buckets")
	names := make([]string, 0, len(out.Buckets))
	for _, b := range out.Buckets {
		names = append(names, aws.ToString(b.Name))
	}
	requireEqual(t, strings.Join(names, ","), "history,paused", "buckets")
	requireVersioningStatus(t, c, "history", types.BucketVersioningStatusEnabled)
	requireVersioningStatus(t, c, "paused", types.BucketVersioningStatusSuspended)

	requireListed(t, s, fx, "history", historyListing(t, fx,
		fx.entry(t, "legacy-v1", true), fx.entry(t, "legacy-null", false)))
	requireListed(t, s, fx, "paused", []listed{
		fx.entry(t, "gone-marker", true), fx.entry(t, "gone-v1", false),
		fx.entry(t, "note-null", true), fx.entry(t, "note-v1", false),
	})
	for _, label := range fixtureBodies {
		requireFixtureBody(t, c, fx.Versions[label])
	}
	_, err = c.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("history"), Key: aws.String("doc.txt"),
	})
	requireMarkerResponse(t, err, http.StatusNotFound, fx.id(t, "doc-marker"))
	// The null version of gone.txt is a marker, the Suspended delete's.
	_, err = c.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("paused"), Key: aws.String("gone.txt"),
	})
	requireMarkerResponse(t, err, http.StatusNotFound, "null")
	_, err = c.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("paused"), Key: aws.String("gone.txt"), VersionId: aws.String("null"),
	})
	requireMarkerResponse(t, err, http.StatusMethodNotAllowed, "null")

	for label, want := range map[string]map[string]string{
		"legacy-null": {"origin": "unversioned"},
		"legacy-v1":   {},
		"doc-v1":      {},
		"doc-v2":      {"stage": "review"},
		"doc-v3":      {},
	} {
		requireVersionTags(t, c, fx.Versions[label], want)
	}
	requireVersionACL(t, c, fx.Versions["doc-v1"], true)
	requireVersionACL(t, c, fx.Versions["doc-v2"], false)

	lc := getLifecycle(t, c, "history")
	sameJSON(t, lc.Rules, fixtureRules(), "the lifecycle rules")
	requireEqual(t, lc.TransitionDefaultMinimumObjectSize,
		types.TransitionDefaultMinimumObjectSizeVariesByStorageClass,
		"the lifecycle default minimum size")
	report := fx.Versions["report-v1"]
	requireExpiration(t, c, "history", report.Key, "", `expiry-date="`+
		reportExpiry(fx).Format(http.TimeFormat)+`", rule-id="expire-reports"`)
	requireFixtureUpload(t, c, fx)
}

// midnightAfter is the midnight UTC strictly after at, when a lifecycle
// rule counting to at makes an object eligible.
func midnightAfter(at time.Time) time.Time {
	return at.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
}

// reportExpiry is when the rule expire-reports expires reports/q3.txt:
// the midnight UTC strictly after a day past its LastModified.
func reportExpiry(fx *versionedRootFixture) time.Time {
	return midnightAfter(fx.Versions["report-v1"].LastModified.Add(24 * time.Hour))
}

// requireListed checks that bucket lists want, versions and markers in
// order, and the fixture's among them with the ETag, size and
// LastModified v0.12.0 gave them.
func requireListed(
	t *testing.T, s *notificationStack, fx *versionedRootFixture, bucket string, want []listed,
) {
	t.Helper()
	got := rootListing(t, s, bucket)
	gotEntries, wantEntries := make([]entry, len(got)), make([]entry, len(want))
	for i := range got {
		gotEntries[i] = got[i].entry
	}
	for i := range want {
		wantEntries[i] = want[i].entry
	}
	if !slices.Equal(gotEntries, wantEntries) {
		t.Fatalf("%s lists:\n got %v\nwant %v", bucket, gotEntries, wantEntries)
	}
	for i, w := range want {
		if w.label == "" {
			continue
		}
		v, g := fx.Versions[w.label], got[i]
		if g.ETag != v.ETag || g.Size != v.Size || !g.LastModified.Equal(v.LastModified) {
			t.Fatalf("%s lists ETag %s, size %d, LastModified %v; want %s, %d, %v", w.label,
				g.ETag, g.Size, g.LastModified, v.ETag, v.Size, v.LastModified)
		}
	}
}

// listedVersion is a Version or DeleteMarker element of a listing; a
// marker has no ETag and no size.
type listedVersion struct {
	LastModified time.Time
	ETag         string
	entry
	Size int64
}

// rootListing lists bucket's versions over raw HTTP, signed with the root
// key, in the order the response has them.
func rootListing(t *testing.T, s *notificationStack, bucket string) []listedVersion {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		s.srv.URL+"/"+bucket+"?versions", http.NoBody)
	requireNoErr(t, err, "build request")
	req.Header.Set("X-Amz-Content-Sha256", emptyHash)
	requireNoErr(t, v4.NewSigner().SignHTTP(t.Context(), rootCreds(), req, emptyHash,
		"s3", "us-east-1", time.Now()), "sign request")
	res := sendLive(t, req)
	requireEqual(t, res.StatusCode, http.StatusOK, "list versions status: "+res.Body)
	dec := xml.NewDecoder(strings.NewReader(res.Body))
	var out []listedVersion
	for {
		tok, tErr := dec.Token()
		if errors.Is(tErr, io.EOF) {
			return out
		}
		requireNoErr(t, tErr, "decode the listing")
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Version" && se.Name.Local != "DeleteMarker" {
			continue
		}
		var e struct {
			LastModified time.Time `xml:"LastModified"`
			Key          string    `xml:"Key"`
			VersionID    string    `xml:"VersionId"`
			ETag         string    `xml:"ETag"`
			Size         int64     `xml:"Size"`
			IsLatest     bool      `xml:"IsLatest"`
		}
		requireNoErr(t, dec.DecodeElement(&e, &se), "decode "+se.Name.Local)
		out = append(out, listedVersion{
			entry: entry{
				Key: e.Key, VersionID: e.VersionID, Marker: se.Name.Local == "DeleteMarker",
				Latest: e.IsLatest,
			},
			LastModified: e.LastModified, ETag: e.ETag, Size: e.Size,
		})
	}
}

// requireFixtureBody checks that GET and HEAD of v, by its version ID,
// answer the body, ETag, size and version genroot wrote.
func requireFixtureBody(t *testing.T, c *s3.Client, v fixtureVersion) {
	t.Helper()
	in := &s3.GetObjectInput{
		Bucket: aws.String(v.Bucket), Key: aws.String(v.Key), VersionId: aws.String(v.VersionID),
	}
	what := v.Bucket + "/" + v.Key + "?versionId=" + v.VersionID
	out, body := get(t, c, in)
	requireEqual(t, sha256Hex(body), v.SHA256, what+" body")
	requireEqual(t, aws.ToString(out.ETag), v.ETag, what+" ETag")
	requireEqual(t, aws.ToString(out.VersionId), v.VersionID, what+" GET x-amz-version-id")
	head, err := c.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: in.Bucket, Key: in.Key, VersionId: in.VersionId,
	})
	requireNoErr(t, err, "head "+what)
	requireEqual(t, aws.ToInt64(head.ContentLength), v.Size, what+" size")
	requireEqual(t, aws.ToString(head.ETag), v.ETag, what+" HEAD ETag")
}

// requireVersionTags checks the tags of v.
func requireVersionTags(t *testing.T, c *s3.Client, v fixtureVersion, want map[string]string) {
	t.Helper()
	out, err := c.GetObjectTagging(t.Context(), &s3.GetObjectTaggingInput{
		Bucket: aws.String(v.Bucket), Key: aws.String(v.Key), VersionId: aws.String(v.VersionID),
	})
	requireNoErr(t, err, "get tags of "+v.Key+" "+v.VersionID)
	got := map[string]string{}
	for _, tag := range out.TagSet {
		got[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("tags of %s %s: %v, want %v", v.Key, v.VersionID, got, want)
	}
	requireEqual(t, aws.ToString(out.VersionId), v.VersionID, "tagging x-amz-version-id")
}

// requireVersionACL checks that v has the public-read ACL, or the owner's
// grant alone.
func requireVersionACL(t *testing.T, c *s3.Client, v fixtureVersion, publicRead bool) {
	t.Helper()
	acl, err := c.GetObjectAcl(t.Context(), &s3.GetObjectAclInput{
		Bucket: aws.String(v.Bucket), Key: aws.String(v.Key), VersionId: aws.String(v.VersionID),
	})
	requireNoErr(t, err, "get the ACL of "+v.Key+" "+v.VersionID)
	if !publicRead {
		requireEqual(t, len(acl.Grants), 1, "grants of "+v.VersionID)
		requireEqual(t, acl.Grants[0].Permission, types.PermissionFullControl, "owner permission")
		return
	}
	requireEqual(t, len(acl.Grants), 2, "public-read grants of "+v.VersionID)
	requireEqual(t, aws.ToString(acl.Grants[1].Grantee.URI),
		"http://acs.amazonaws.com/groups/global/AllUsers", "AllUsers grantee")
	requireEqual(t, acl.Grants[1].Permission, types.PermissionRead, "AllUsers permission")
}

// requireFixtureUpload checks that the upload v0.12.0 left in progress is
// listed, with its part and metadata, and that "history" has none.
func requireFixtureUpload(t *testing.T, c *s3.Client, fx *versionedRootFixture) {
	t.Helper()
	up := fx.Upload
	requireEqual(t, strings.Join(uploadKeys(t, c, "history"), ","), "", "history uploads")
	ups, err := c.ListMultipartUploads(t.Context(),
		&s3.ListMultipartUploadsInput{Bucket: aws.String(up.Bucket)})
	requireNoErr(t, err, "list multipart uploads")
	requireEqual(t, len(ups.Uploads), 1, "uploads in progress")
	requireEqual(t, aws.ToString(ups.Uploads[0].UploadId), up.ID, "upload ID")
	requireEqual(t, aws.ToString(ups.Uploads[0].Key), up.Key, "upload key")
	parts, err := c.ListParts(t.Context(), &s3.ListPartsInput{
		Bucket: aws.String(up.Bucket), Key: aws.String(up.Key), UploadId: aws.String(up.ID),
	})
	requireNoErr(t, err, "list parts")
	requireEqual(t, len(parts.Parts), 1, "parts uploaded")
	requireEqual(t, aws.ToString(parts.Parts[0].ETag), up.PartETag, "part ETag")
	requireEqual(t, aws.ToInt64(parts.Parts[0].Size), up.PartSize, "part size")
}

// The event state of a storage root, which v0.12.0 never wrote.
var (
	eventsDir    = filepath.Join(".tule", "events")
	eventSeqFile = filepath.Join(".tule", "eventseq")
)

// requireNoFile fails unless the storage root dir has no file name.
func requireNoFile(t *testing.T, dir, name, when string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s %s: %v", name, when, err)
	}
}

// requireNoEvents checks that the storage root dir, started on, has the
// events directory every start creates, empty, and no sequence lease,
// which only an event writes.
func requireNoEvents(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, eventsDir))
	requireNoErr(t, err, "read the events directory")
	requireEqual(t, len(entries), 0, "entries of the events directory before any event")
	requireNoFile(t, dir, eventSeqFile, "before any event")
}

// seqLease returns the sequence lease of the storage root dir, as a
// sequencer.
func seqLease(t *testing.T, dir string) string {
	t.Helper()
	//nolint:gosec // G304: a file of the test's own storage root
	raw, err := os.ReadFile(filepath.Join(dir, eventSeqFile))
	requireNoErr(t, err, "read the sequence lease")
	n, err := strconv.ParseInt(string(raw), 10, 64)
	requireNoErr(t, err, "decode the sequence lease")
	return fmt.Sprintf("%016X", n)
}

// sequencer returns r's sequencer, failing unless it has one.
func sequencer(t *testing.T, r *eventRecord) string {
	t.Helper()
	requireSequencer(t, r)
	return aws.ToString(r.S3.Object.Sequencer)
}

// requireLegacyNullEvents tags the legacy null version of history/
// legacy.txt, deletes it by versionId=null and writes over the key,
// checks their events, and that the first event created history's events
// directory and the sequence lease, above every sequencer. It returns the
// sequencer of the write.
func requireLegacyNullEvents(
	t *testing.T, s *notificationStack, fx *versionedRootFixture, q string,
) string {
	t.Helper()
	ctx := t.Context()
	c := s.s3
	b, k, null := aws.String("history"), aws.String("legacy.txt"), aws.String("null")
	tag, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: b, Key: k, VersionId: null, Tagging: &types.Tagging{TagSet: []types.Tag{
			{Key: aws.String("origin"), Value: aws.String("retagged")},
		}},
	})
	requireNoErr(t, err, "tag the legacy null version")
	requireEqual(t, aws.ToString(tag.VersionId), "null", "tagged version")
	del, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: b, Key: k, VersionId: null})
	requireNoErr(t, err, "delete the legacy null version")
	requireEqual(t, aws.ToString(del.VersionId), "null", "deleted version")
	requireEqual(t, aws.ToBool(del.DeleteMarker), false, "a delete marker deleted")
	written := put(t, c, "history", "legacy.txt", []byte(legacyV2))
	id := aws.ToString(written.VersionId)
	if id <= fx.id(t, "legacy-v1") {
		t.Fatalf("the new version %s sorts before the fixture's %s", id, fx.id(t, "legacy-v1"))
	}

	last := requireLegacyNullRecords(t, s.awaitEvents(t, q, 3), fx,
		[]middleware.Metadata{tag.ResultMetadata, del.ResultMetadata, written.ResultMetadata},
		written)
	if info, sErr := os.Stat(filepath.Join(s.srv.Root, eventsDir, "history")); sErr != nil ||
		!info.IsDir() {
		t.Fatalf("history's events directory: %v", sErr)
	}
	s.requireOutboxEmpty(t, "history")
	if lease := seqLease(t, s.srv.Root); lease <= last {
		t.Fatalf("the sequence lease %s is not above the last sequencer %s", lease, last)
	}
	requireListed(t, s, fx, "history", historyListing(t, fx,
		listed{entry: entry{Key: "legacy.txt", VersionID: id, Latest: true}},
		fx.entry(t, "legacy-v1", false)))
	return last
}

// requireLegacyNullRecords checks recs, the events of the tagging, the
// delete and the write requireLegacyNullEvents makes, whose responses have
// the metadata mds, the write answering written, and returns the write's
// sequencer: the tagging's event names the null version and its eTag,
// without a sequencer, the delete's the null version with a sequencer, and
// the write's a sequencer after it.
func requireLegacyNullRecords(
	t *testing.T, recs []eventRecord, fx *versionedRootFixture, mds []middleware.Metadata,
	written *s3.PutObjectOutput,
) string {
	t.Helper()
	for i := range recs {
		req, host := sdkRequestIDs(t, mds[i])
		requireCommon(t, &recs[i], "history", "fixture", req, host)
	}
	r := &recs[0]
	requireEqual(t, r.EventName, "ObjectTagging:Put", "eventName")
	requireEqual(t, r.S3.Object.Key, "legacy.txt", "key")
	requireVersion(t, r, "null")
	o := r.S3.Object
	if o.ETag == nil || *o.ETag != strings.Trim(fx.Versions["legacy-null"].ETag, `"`) ||
		o.Size != nil || o.Sequencer != nil {
		t.Errorf("tagging event: object %+v, want the legacy eTag, without size or sequencer", o)
	}

	r = &recs[1]
	requireRemoved(t, r, "ObjectRemoved:Delete", "legacy.txt")
	requireVersion(t, r, "null")
	deleted := sequencer(t, r)

	r = &recs[2]
	requireCreated(t, r, "ObjectCreated:Put", "legacy.txt", int64(len(legacyV2)),
		aws.ToString(written.ETag))
	requireVersion(t, r, aws.ToString(written.VersionId))
	last := sequencer(t, r)
	if last <= deleted {
		t.Fatalf("the write's sequencer %s does not sort after the delete's %s", last, deleted)
	}
	return last
}

// requireSuspendedMarkerWrite writes paused/gone.txt, whose null version
// is the delete marker v0.12.0 added while Suspended: the write replaces
// the marker as the null version and sends one ObjectCreated:Put of
// version null, and the version below it is kept.
func requireSuspendedMarkerWrite(t *testing.T, s *notificationStack, fx *versionedRootFixture) {
	t.Helper()
	const queue = "paused-events"
	u := s.createQueue(t, queue, s3QueuePolicy(queueARN(queue), "paused"))
	requireNoErr(t, putNotification(t, s.s3, "paused", &types.NotificationConfiguration{
		QueueConfigurations: []types.QueueConfiguration{{
			Id: aws.String("paused"), QueueArn: aws.String(queueARN(queue)),
			Events: createdAndRemoved,
		}},
	}, false), "configure paused")

	out := put(t, s.s3, "paused", "gone.txt", []byte(goneNull))
	requireEqual(t, aws.ToString(out.VersionId), "null", "the Suspended write's version")
	recs := s.awaitEvents(t, u, 1)
	req, host := sdkRequestIDs(t, out.ResultMetadata)
	requireCommon(t, &recs[0], "paused", "paused", req, host)
	requireCreated(t, &recs[0], "ObjectCreated:Put", "gone.txt", int64(len(goneNull)),
		aws.ToString(out.ETag))
	requireVersion(t, &recs[0], "null")

	requireListed(t, s, fx, "paused", []listed{
		{entry: entry{Key: "gone.txt", VersionID: "null", Latest: true}},
		fx.entry(t, "gone-v1", false),
		fx.entry(t, "note-null", true), fx.entry(t, "note-v1", false),
	})
	requireVersionBody(t, s.s3, "paused", "gone.txt", "", goneNull, "null")
	requireVersionBody(t, s.s3, "paused", "gone.txt", "null", goneNull, "null")
	requireFixtureBody(t, s.s3, fx.Versions["gone-v1"])
}

// archiveExpiry is when the rule expire-archive expires archive-v1, 4
// days after it became noncurrent: counted from the time v0.12.0
// recorded, and, as a misread would count, from its successor's
// LastModified and from its own.
func archiveExpiry(fx *versionedRootFixture) (recorded, successor, own time.Time) {
	const noncurrent = 4 * 24 * time.Hour
	return midnightAfter(fx.NoncurrentSince["archive-v1"].Add(noncurrent)),
		midnightAfter(fx.Versions["archive-v3"].LastModified.Add(noncurrent)),
		midnightAfter(fx.Versions["archive-v1"].LastModified.Add(noncurrent))
}

// requireFixtureExpiration moves the storage clock to when the fixture's
// rule expire-reports expires reports/q3.txt, and checks its
// LifecycleExpiration event: a delete marker over the report, sorting
// after the write before it, while the Disabled rule expires nothing and
// archive-v1 is kept. Then it moves the clock to when expire-archive
// expires archive-v1, counted from when v0.12.0 recorded it became
// noncurrent, and checks its LifecycleExpiration:Delete.
func requireFixtureExpiration(
	t *testing.T, s *notificationStack, fx *versionedRootFixture, clk *fakeClock,
	q, written string,
) {
	t.Helper()
	report := reportExpiry(fx)
	recorded, successor, own := archiveExpiry(fx)
	// The sweep ticks every minute of the clock, so each move ticks it.
	if !own.Before(report) || !report.Before(recorded) || !recorded.Before(successor) {
		t.Fatalf("the fixture's expiries do not tell the times apart: own %v, report %v, "+
			"recorded %v, successor %v", own, report, recorded, successor)
	}
	advance(t, clk, report)
	recs := s.awaitEvents(t, q, 1)
	versions := versionsOf(t, s.s3, "history", "reports/q3.txt")
	if len(versions) != 2 || !versions[1].Marker || !versions[1].Latest ||
		versions[0].VersionID != fx.id(t, "report-v1") {
		t.Fatalf("versions of reports/q3.txt %+v, want a marker over the fixture's", versions)
	}
	requireLifecycleEvent(t, &recs[0], "LifecycleExpiration:DeleteMarkerCreated", "history",
		"reports/q3.txt", versions[1].VersionID)
	if seq := sequencer(t, &recs[0]); seq <= written {
		t.Fatalf("the expiration's sequencer %s does not sort after %s", seq, written)
	}
	requireEqual(t, len(versionsOf(t, s.s3, "history", "doc.txt")), 4, "versions of doc.txt")
	requireEqual(t, len(versionsOf(t, s.s3, "history", "untouched.txt")), 1,
		"versions of untouched.txt")
	requireEqual(t, len(versionsOf(t, s.s3, "history", "archive/log.txt")), 2,
		"versions of archive/log.txt past the expiry its own LastModified would give")
	requireFixtureUpload(t, s.s3, fx)
	requireArchiveExpiry(t, s, fx, clk, q, recorded)
}

// requireArchiveExpiry moves the storage clock to recorded, when
// expire-archive expires archive-v1, and checks its
// LifecycleExpiration:Delete and that archive-v3 is left alone.
func requireArchiveExpiry(
	t *testing.T, s *notificationStack, fx *versionedRootFixture, clk *fakeClock,
	q string, recorded time.Time,
) {
	t.Helper()
	advance(t, clk, recorded)
	recs := s.awaitEvents(t, q, 1)
	requireLifecycleEvent(t, &recs[0], "LifecycleExpiration:Delete", "history",
		"archive/log.txt", fx.id(t, "archive-v1"))
	versions := versionsOf(t, s.s3, "history", "archive/log.txt")
	if len(versions) != 1 || versions[0].VersionID != fx.id(t, "archive-v3") ||
		!versions[0].Latest {
		t.Fatalf("versions of archive/log.txt %+v, want archive-v3 alone", versions)
	}
}

// requireResumedSequence restarts object-store and writes over doc.txt's
// delete marker: its event's sequencer is above the lease the restart
// resumed from, and the fixture's versions are still there beneath it.
func requireResumedSequence(
	t *testing.T, s *notificationStack, fx *versionedRootFixture, q string,
) {
	t.Helper()
	s.restartObjectStore(t)
	lease := seqLease(t, s.srv.Root)
	out := put(t, s.s3, "history", "doc.txt", []byte(docV4))
	recs := s.awaitEvents(t, q, 1)
	req, host := sdkRequestIDs(t, out.ResultMetadata)
	requireCommon(t, &recs[0], "history", "fixture", req, host)
	requireCreated(t, &recs[0], "ObjectCreated:Put", "doc.txt", int64(len(docV4)),
		aws.ToString(out.ETag))
	requireVersion(t, &recs[0], aws.ToString(out.VersionId))
	if seq := sequencer(t, &recs[0]); seq <= lease {
		t.Fatalf("the sequencer %s after a restart is not above the lease %s", seq, lease)
	}
	for _, label := range []string{"doc-v1", "doc-v2", "doc-v3"} {
		requireFixtureBody(t, s.s3, fx.Versions[label])
	}
}
