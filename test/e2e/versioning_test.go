//go:build e2e

package e2e

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The tests here check bucket versioning as an unmodified SDK client sees
// it: the versioning states, the version IDs writes answer, reads and
// deletes by version, delete markers, listing versions, and the Suspended
// state's null version.

// setVersioning sets bucket's versioning status.
func setVersioning(t *testing.T, c *s3.Client, bucket string, status types.BucketVersioningStatus) {
	t.Helper()
	_, err := c.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: status},
	})
	requireNoErr(t, err, "put bucket versioning "+string(status))
}

// versionedBucket creates bucket with versioning Enabled.
func versionedBucket(t *testing.T, c *s3.Client, bucket string) {
	t.Helper()
	createBucket(t, c, bucket)
	setVersioning(t, c, bucket, types.BucketVersioningStatusEnabled)
}

// putVersion puts body at bucket/key and returns the version ID answered,
// "" for none.
func putVersion(t *testing.T, c *s3.Client, bucket, key, body string) string {
	t.Helper()
	return aws.ToString(put(t, c, bucket, key, []byte(body)).VersionId)
}

// getVersion reads version id of bucket/key, the current one for "", and
// returns its body and the version ID answered.
func getVersion(t *testing.T, c *s3.Client, bucket, key, id string) (string, string) {
	t.Helper()
	in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if id != "" {
		in.VersionId = aws.String(id)
	}
	out, body := get(t, c, in)
	return string(body), aws.ToString(out.VersionId)
}

// entry is one version or delete marker of a listing.
type entry struct {
	Key, VersionID string
	Marker, Latest bool
}

// listAll lists every version of bucket under in's prefix and delimiter
// with the SDK, in one page.
func listVersions(
	t *testing.T,
	c *s3.Client,
	in s3.ListObjectVersionsInput,
) *s3.ListObjectVersionsOutput {
	t.Helper()
	out, err := c.ListObjectVersions(t.Context(), &in)
	requireNoErr(t, err, "list object versions")
	requireEqual(t, aws.ToBool(out.IsTruncated), false, "one page truncated")
	return out
}

// rawVersions lists bucket's versions over raw HTTP and returns its
// Version and DeleteMarker elements in the order the response has them,
// which the SDK, splitting them in two lists, does not keep.
func rawVersions(t *testing.T, srv *liveServer, bucket, query string) []entry {
	t.Helper()
	req := signedRequest(t, srv, http.MethodGet, "/"+bucket+"?versions"+query, nil, emptyHash,
		"s3", "us-east-1", time.Now())
	res := sendLive(t, req)
	requireEqual(t, res.StatusCode, http.StatusOK, "list versions status: "+res.Body)
	dec := xml.NewDecoder(strings.NewReader(res.Body))
	var entries []entry
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Version" && se.Name.Local != "DeleteMarker" {
			continue
		}
		var e struct {
			Key       string `xml:"Key"`
			VersionID string `xml:"VersionId"`
			IsLatest  bool   `xml:"IsLatest"`
		}
		requireNoErr(t, dec.DecodeElement(&e, &se), "decode "+se.Name.Local)
		entries = append(entries, entry{
			Key: e.Key, VersionID: e.VersionID, Marker: se.Name.Local == "DeleteMarker",
			Latest: e.IsLatest,
		})
	}
	return entries
}

// pageEntries is a page's versions and markers, in the order of full, the
// whole listing, which the SDK's two lists do not keep.
func pageEntries(t *testing.T, out *s3.ListObjectVersionsOutput, full []entry) []entry {
	t.Helper()
	page := make([]entry, 0, len(out.Versions)+len(out.DeleteMarkers))
	for _, v := range out.Versions {
		page = append(page, entry{
			Key: aws.ToString(v.Key), VersionID: aws.ToString(v.VersionId),
			Latest: aws.ToBool(v.IsLatest),
		})
	}
	for _, m := range out.DeleteMarkers {
		page = append(page, entry{
			Key: aws.ToString(m.Key), VersionID: aws.ToString(m.VersionId),
			Marker: true, Latest: aws.ToBool(m.IsLatest),
		})
	}
	slices.SortFunc(page, func(a, b entry) int {
		return slices.Index(full, a) - slices.Index(full, b)
	})
	return page
}

// requireMarkerResponse checks that err is a response of status carrying
// x-amz-delete-marker: true and x-amz-version-id id, and, for a 405,
// Last-Modified.
func requireMarkerResponse(t *testing.T, err error, status int, id string) {
	t.Helper()
	h := requireResponse(t, err, status)
	requireEqual(t, h.Get("X-Amz-Delete-Marker"), "true", "x-amz-delete-marker")
	requireEqual(t, h.Get("X-Amz-Version-Id"), id, "x-amz-version-id")
	if status == http.StatusMethodNotAllowed && h.Get("Last-Modified") == "" {
		t.Fatal("a 405 for a delete marker has no Last-Modified")
	}
}

func TestBucketVersioningStates(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	createBucket(t, c, "states")
	bucket := aws.String("states")
	status := func() (types.BucketVersioningStatus, types.MFADeleteStatus) {
		t.Helper()
		out, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: bucket})
		requireNoErr(t, err, "get bucket versioning")
		return out.Status, out.MFADelete
	}

	s, mfa := status()
	requireEqual(t, s, "", "never set")
	requireEqual(t, mfa, "", "MFA delete never set")
	for _, want := range []types.BucketVersioningStatus{
		types.BucketVersioningStatusEnabled, types.BucketVersioningStatusSuspended,
		types.BucketVersioningStatusEnabled,
	} {
		setVersioning(t, c, "states", want)
		s, mfa = status()
		requireEqual(t, s, want, "status")
		requireEqual(t, mfa, "", "MFA delete not set")
	}
	_, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: bucket, VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatusSuspended, MFADelete: types.MFADeleteDisabled,
		},
	})
	requireNoErr(t, err, "suspend with MFA delete disabled")
	s, mfa = status()
	requireEqual(t, s, types.BucketVersioningStatusSuspended, "status")
	requireEqual(t, mfa, types.MFADeleteStatusDisabled, "MFA delete echoed")

	// A bucket never versioned can be suspended.
	createBucket(t, c, "suspended")
	setVersioning(t, c, "suspended", types.BucketVersioningStatusSuspended)
	out, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{
		Bucket: aws.String("suspended"),
	})
	requireNoErr(t, err, "get suspended")
	requireEqual(t, out.Status, types.BucketVersioningStatusSuspended, "suspended from unversioned")

	// A bucket deleted and created again is unversioned.
	_, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: bucket})
	requireNoErr(t, err, "delete bucket")
	createBucket(t, c, "states")
	s, _ = status()
	requireEqual(t, s, "", "a new bucket is unversioned")
}

// A bucket never returns to unversioned, MFA delete is not served, and a
// status S3 does not know is malformed; none of them stores anything.
func TestBucketVersioningRefusals(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	createBucket(t, c, "refused")
	bucket := aws.String("refused")
	setVersioning(t, c, "refused", types.BucketVersioningStatusSuspended)

	_, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: bucket, VersioningConfiguration: &types.VersioningConfiguration{},
	})
	requireCode(t, err, "IllegalVersioningConfigurationException")
	requireResponse(t, err, http.StatusBadRequest)
	_, err = c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: bucket, VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatusEnabled, MFADelete: types.MFADeleteEnabled,
		},
	})
	requireCode(t, err, "NotImplemented")
	_, err = c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: bucket, VersioningConfiguration: &types.VersioningConfiguration{
			Status: "On",
		},
	})
	requireCode(t, err, "MalformedXML")
	out, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: bucket})
	requireNoErr(t, err, "get bucket versioning")
	requireEqual(
		t,
		out.Status,
		types.BucketVersioningStatusSuspended,
		"refused configurations stored nothing",
	)
}

// completeOnePart uploads bucket/key as a multipart upload of one part and
// returns the version ID its completion answered.
func completeOnePart(t *testing.T, c *s3.Client, bucket, key string) string {
	t.Helper()
	ctx := t.Context()
	up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	requireNoErr(t, err, "create upload")
	part, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: up.UploadId,
		PartNumber: aws.Int32(1), Body: strings.NewReader("part"),
	})
	requireNoErr(t, err, "upload part")
	done, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: part.ETag},
		}},
	})
	requireNoErr(t, err, "complete upload")
	return aws.ToString(done.VersionId)
}

func TestVersionedWrites(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	createBucket(t, c, "plain")
	requireEqual(t, putVersion(t, c, "plain", "k", "x"), "", "unversioned PUT version")
	versionedBucket(t, c, "ver")

	ids := map[string]bool{}
	for _, body := range []string{"one", "two", "three"} {
		id := putVersion(t, c, "ver", "k", body)
		if id == "" || id == "null" || ids[id] {
			t.Fatalf("PUT answered version %q after %v", id, ids)
		}
		ids[id] = true
	}

	cp, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("ver"), Key: aws.String("copy"), CopySource: copySource("ver", "k"),
	})
	requireNoErr(t, err, "copy")
	copyID := aws.ToString(cp.VersionId)
	if copyID == "" || ids[copyID] {
		t.Fatalf("copy answered version %q", copyID)
	}
	body, id := getVersion(t, c, "ver", "copy", "")
	requireEqual(t, body, "three", "copy body")
	requireEqual(t, id, copyID, "copy is the current version")

	done := completeOnePart(t, c, "ver", "mp")
	_, id = getVersion(t, c, "ver", "mp", "")
	requireEqual(t, done, id, "completed version")
}

// Concurrent writes of one key all become versions.
func TestConcurrentVersionedWrites(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	versionedBucket(t, c, "ver")
	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			_, pErr := c.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String("ver"), Key: aws.String("race"),
				Body: strings.NewReader(strconv.Itoa(i)),
			})
			errs <- pErr
		})
	}
	wg.Wait()
	close(errs)
	for pErr := range errs {
		requireNoErr(t, pErr, "concurrent put")
	}
	out := listVersions(t, c, s3.ListObjectVersionsInput{
		Bucket: aws.String("ver"),
		Prefix: aws.String("race"),
	})
	requireEqual(t, len(out.Versions), n, "versions of concurrent puts")
}

func TestVersionedReads(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	createBucket(t, c, "reads")
	putVersion(t, c, "reads", "old", "before")

	// Unversioned, the object is its null version, and nothing names it.
	body, id := getVersion(t, c, "reads", "old", "null")
	requireEqual(t, body, "before", "unversioned null read")
	requireEqual(t, id, "", "unversioned read version")

	setVersioning(t, c, "reads", types.BucketVersioningStatusEnabled)
	body, id = getVersion(t, c, "reads", "old", "")
	requireEqual(t, body, "before", "pre-existing object")
	requireEqual(t, id, "null", "pre-existing object is the null version")
	v2 := putVersion(t, c, "reads", "old", "after")

	for _, tc := range []struct{ id, body string }{
		{"", "after"}, {v2, "after"}, {"null", "before"},
	} {
		body, id = getVersion(t, c, "reads", "old", tc.id)
		requireEqual(t, body, tc.body, "body of "+tc.id)
		want := tc.id
		if want == "" {
			want = v2
		}
		requireEqual(t, id, want, "version of "+tc.id)
	}
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("reads"), Key: aws.String("old"), VersionId: aws.String("null"),
	})
	requireNoErr(t, err, "head null")
	requireEqual(t, aws.ToString(head.VersionId), "null", "head version")
	requireEqual(t, aws.ToInt64(head.ContentLength), int64(len("before")), "head length")

	ranged, rBody := get(t, c, &s3.GetObjectInput{
		Bucket: aws.String("reads"), Key: aws.String("old"), VersionId: aws.String("null"),
		Range: aws.String("bytes=1-3"),
	})
	requireEqual(t, string(rBody), "efo", "range of the null version")
	requireEqual(t, aws.ToString(ranged.VersionId), "null", "range version")
	_, err = c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("reads"), Key: aws.String("old"), VersionId: aws.String("null"),
		IfMatch: aws.String(md5Hex([]byte("after"))),
	})
	requireResponse(t, err, http.StatusPreconditionFailed)
}

// A version the key does not have, well-formed (another key's), is
// NoSuchVersion; a malformed or empty one is 400 InvalidArgument.
func TestVersionNotFound(t *testing.T) {
	srv := startLive(t, nil)
	c := clientFor(srv)
	ctx := t.Context()
	versionedBucket(t, c, "reads")
	putVersion(t, c, "reads", "old", "x")
	other := putVersion(t, c, "reads", "other", "x")
	_, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("reads"), Key: aws.String("old"), VersionId: aws.String(other),
	})
	requireCode(t, err, "NoSuchVersion")
	requireResponse(t, err, http.StatusNotFound)
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("reads"), Key: aws.String("old"), VersionId: aws.String(other),
	})
	requireResponse(t, err, http.StatusNotFound)
	_, err = c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("reads"), Key: aws.String("old"), VersionId: aws.String("bogus"),
	})
	requireAPIError(t, err, "InvalidArgument", "Invalid version id specified")
	// The SDK sends no empty versionId.
	status, e := send(t, srv.Client(), signedRequest(t, srv, http.MethodGet,
		"/reads/old?versionId=", nil, emptyHash, "s3", "us-east-1", time.Now()))
	requireS3Error(t, status, e, http.StatusBadRequest, "InvalidArgument",
		"Version id cannot be the empty string")
}

// A delete marker hides the object: a GET or HEAD of the key is 404 with
// the marker's headers, and of the marker by version 405 with its
// Last-Modified; deleting the marker brings the object back.
func TestDeleteMarkers(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	versionedBucket(t, c, "dmk")
	v1 := putVersion(t, c, "dmk", "k", "data")

	del, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("dmk"), Key: aws.String("k"),
	})
	requireNoErr(t, err, "simple delete")
	requireEqual(t, aws.ToBool(del.DeleteMarker), true, "delete marker created")
	marker := aws.ToString(del.VersionId)
	if marker == "" || marker == v1 {
		t.Fatalf("marker version %q", marker)
	}

	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("dmk"), Key: aws.String("k")})
	requireCode(t, err, "NoSuchKey")
	requireMarkerResponse(t, err, http.StatusNotFound, marker)
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("dmk"), Key: aws.String("k")})
	requireMarkerResponse(t, err, http.StatusNotFound, marker)
	_, err = c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("dmk"), Key: aws.String("k"), VersionId: aws.String(marker),
	})
	requireCode(t, err, "MethodNotAllowed")
	requireMarkerResponse(t, err, http.StatusMethodNotAllowed, marker)
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("dmk"), Key: aws.String("k"), VersionId: aws.String(marker),
	})
	requireMarkerResponse(t, err, http.StatusMethodNotAllowed, marker)
	body, _ := getVersion(t, c, "dmk", "k", v1)
	requireEqual(t, body, "data", "the version under the marker")
	requireMarkedKeyIsMissing(t, c)

	// A marker on a marker, and on a key that never existed.
	again, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("dmk"), Key: aws.String("k"),
	})
	requireNoErr(t, err, "delete again")
	requireEqual(t, aws.ToBool(again.DeleteMarker), true, "second marker")
	never, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("dmk"), Key: aws.String("never"),
	})
	requireNoErr(t, err, "delete a key never written")
	requireEqual(t, aws.ToBool(never.DeleteMarker), true, "marker of a key never written")

	// Deleting the markers by version restores the object.
	for _, id := range []string{aws.ToString(again.VersionId), marker} {
		out, dErr := c.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String("dmk"), Key: aws.String("k"), VersionId: aws.String(id),
		})
		requireNoErr(t, dErr, "delete marker "+id)
		requireEqual(t, aws.ToBool(out.DeleteMarker), true, "removed a marker")
		requireEqual(t, aws.ToString(out.VersionId), id, "removed version")
	}
	body, id := getVersion(t, c, "dmk", "k", "")
	requireEqual(t, body, "data", "restored")
	requireEqual(t, id, v1, "restored version")
}

// requireMarkedKeyIsMissing checks that the operations other than reads
// that act on dmk/k, whose current version is a delete marker, answer
// 404 NoSuchKey: a copy from it and its tagging.
func requireMarkedKeyIsMissing(t *testing.T, c *s3.Client) {
	t.Helper()
	_, err := c.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket: aws.String("dmk"), Key: aws.String("copy"), CopySource: copySource("dmk", "k"),
	})
	requireCode(t, err, "NoSuchKey")
	_, err = c.GetObjectTagging(t.Context(), &s3.GetObjectTaggingInput{
		Bucket: aws.String("dmk"), Key: aws.String("k"),
	})
	requireCode(t, err, "NoSuchKey")
}

func TestVersionedDeletes(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	versionedBucket(t, c, "del")
	v1 := putVersion(t, c, "del", "k", "one")
	v2 := putVersion(t, c, "del", "k", "two")

	out, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("del"), Key: aws.String("k"), VersionId: aws.String(v2),
	})
	requireNoErr(t, err, "permanent delete")
	requireEqual(t, aws.ToString(out.VersionId), v2, "deleted version")
	requireEqual(t, aws.ToBool(out.DeleteMarker), false, "not a marker")
	body, id := getVersion(t, c, "del", "k", "")
	requireEqual(t, body, "one", "the older version is current")
	requireEqual(t, id, v1, "current version")
	_, err = c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("del"), Key: aws.String("k"), VersionId: aws.String(v2),
	})
	requireCode(t, err, "NoSuchVersion")

	// A version that does not exist is deleted already.
	out, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("del"), Key: aws.String("k"), VersionId: aws.String(v2),
	})
	requireNoErr(t, err, "delete a version again")
	requireEqual(t, aws.ToString(out.VersionId), v2, "echoed version")
}

// DeleteObjects deletes keys and versions, a simple delete creating a
// marker, and answers each in the shape the DeleteObjects API reference
// shows for it.
func TestDeleteObjectsVersions(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	versionedBucket(t, c, "del")
	v1 := putVersion(t, c, "del", "k", "one")
	a1 := putVersion(t, c, "del", "a", "a")
	b1 := putVersion(t, c, "del", "b", "b")
	m, err := c.DeleteObject(
		ctx,
		&s3.DeleteObjectInput{Bucket: aws.String("del"), Key: aws.String("b")},
	)
	requireNoErr(t, err, "mark b")
	bMarker := aws.ToString(m.VersionId)
	res, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("del"), Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("a")},
			{Key: aws.String("k"), VersionId: aws.String(v1)},
			{Key: aws.String("b"), VersionId: aws.String(bMarker)},
			{Key: aws.String("b"), VersionId: aws.String("bogus")},
		}},
	})
	requireNoErr(t, err, "delete objects")
	requireEqual(t, len(res.Deleted), 3, "deleted entries")
	got := map[string]types.DeletedObject{}
	for _, d := range res.Deleted {
		got[aws.ToString(d.Key)] = d
	}
	a := got["a"]
	requireEqual(t, aws.ToBool(a.DeleteMarker), true, "a: marker created")
	if aws.ToString(a.DeleteMarkerVersionId) == "" || aws.ToString(a.VersionId) != "" {
		t.Fatalf("a: %+v", a)
	}
	k := got["k"]
	requireEqual(t, aws.ToString(k.VersionId), v1, "k: version")
	requireEqual(t, aws.ToBool(k.DeleteMarker), false, "k: no marker")
	b := got["b"]
	requireEqual(t, aws.ToString(b.VersionId), bMarker, "b: version")
	requireEqual(t, aws.ToBool(b.DeleteMarker), true, "b: was a marker")
	requireEqual(t, aws.ToString(b.DeleteMarkerVersionId), bMarker, "b: marker version")
	requireEqual(t, len(res.Errors), 1, "error entries")
	requireEqual(t, aws.ToString(res.Errors[0].Code), "InvalidArgument", "bogus version")
	requireEqual(t, aws.ToString(res.Errors[0].VersionId), "bogus", "error version")

	body, id := getVersion(t, c, "del", "b", "")
	requireEqual(t, body, "b", "b restored")
	requireEqual(t, id, b1, "b version")
	body, _ = getVersion(t, c, "del", "a", a1)
	requireEqual(t, body, "a", "a's version under its marker")
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("del"), Key: aws.String("k")})
	requireCode(t, err, "NoSuchKey")
}

// A quiet DeleteObjects of a version answers nothing for it.
func TestQuietDeleteObjectsOfAVersion(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	versionedBucket(t, c, "del")
	b1 := putVersion(t, c, "del", "b", "b")
	quiet, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("del"), Delete: &types.Delete{
			Quiet:   aws.Bool(true),
			Objects: []types.ObjectIdentifier{{Key: aws.String("b"), VersionId: aws.String(b1)}},
		},
	})
	requireNoErr(t, err, "quiet delete")
	requireEqual(t, len(quiet.Deleted)+len(quiet.Errors), 0, "quiet entries")
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("del"), Key: aws.String("b")})
	requireCode(t, err, "NoSuchKey")
}

// Unversioned, versionId=null deletes the object, naming no version.
func TestUnversionedDeleteOfNull(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	createBucket(t, c, "plain")
	putVersion(t, c, "plain", "k", "x")
	out, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("plain"), Key: aws.String("k"), VersionId: aws.String("null"),
	})
	requireNoErr(t, err, "unversioned delete of null")
	requireEqual(t, aws.ToString(out.VersionId), "", "unversioned delete version")
	requireEqual(t, aws.ToBool(out.DeleteMarker), false, "unversioned delete marker")
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("plain"), Key: aws.String("k")})
	requireCode(t, err, "NoSuchKey")
}

// listingBucket makes bucket "lvs" with keys a (four versions, a delete
// marker between the newest and the others), b/x and b/y, and c (a marker
// over one version), and returns its whole listing in order.
func listingBucket(t *testing.T, srv *liveServer, c *s3.Client) []entry {
	t.Helper()
	versionedBucket(t, c, "lvs")
	for _, k := range []string{"a", "a", "b/x", "a", "b/y", "c"} {
		putVersion(t, c, "lvs", k, k)
	}
	for _, k := range []string{"a", "c"} {
		_, err := c.DeleteObject(t.Context(), &s3.DeleteObjectInput{
			Bucket: aws.String("lvs"), Key: aws.String(k),
		})
		requireNoErr(t, err, "mark "+k)
	}
	// A version put over a's marker, so a marker sits between versions.
	putVersion(t, c, "lvs", "a", "a")
	return rawVersions(t, srv, "lvs", "")
}

func TestListObjectVersionsOrder(t *testing.T) {
	srv := startLive(t, nil)
	c := clientFor(srv)
	full := listingBucket(t, srv, c)

	// Keys ascending; within a key newest first, markers interleaved; the
	// first of each key latest.
	shape := make([]string, 0, len(full))
	for i, e := range full {
		kind := "V"
		if e.Marker {
			kind = "M"
		}
		shape = append(shape, e.Key+":"+kind)
		first := i == 0 || full[i-1].Key != e.Key
		requireEqual(t, e.Latest, first, "IsLatest of "+e.Key+" "+e.VersionID)
	}
	requireEqual(t, strings.Join(shape, " "), "a:V a:M a:V a:V a:V b/x:V b/y:V c:M c:V",
		"listing order")
	out := listVersions(t, c, s3.ListObjectVersionsInput{Bucket: aws.String("lvs")})
	requireEqual(t, len(out.Versions), 7, "SDK versions")
	requireEqual(t, len(out.DeleteMarkers), 2, "SDK delete markers")
	if got := pageEntries(t, out, full); !slices.Equal(got, full) {
		t.Fatalf("the SDK's listing %v is not the raw one %v", got, full)
	}

	// An unversioned bucket lists each object as its null version.
	createBucket(t, c, "plain")
	putVersion(t, c, "plain", "k", "x")
	out = listVersions(t, c, s3.ListObjectVersionsInput{Bucket: aws.String("plain")})
	requireEqual(t, len(out.Versions), 1, "unversioned versions")
	requireEqual(t, aws.ToString(out.Versions[0].VersionId), "null", "unversioned version")
	requireEqual(t, aws.ToBool(out.Versions[0].IsLatest), true, "unversioned latest")
}

// pageAll lists in page by page, resuming each on the markers the one
// before answered, and returns the entries and common prefixes listed, in
// the order of full. A page of entries ends on the last entry it returned;
// one ended by a common prefix has no version-id marker.
func pageAll(
	t *testing.T,
	c *s3.Client,
	in s3.ListObjectVersionsInput,
	full []entry,
) ([]entry, []string) {
	t.Helper()
	var got []entry
	var prefixes []string
	for range 2 * len(full) {
		page, err := c.ListObjectVersions(t.Context(), &in)
		requireNoErr(t, err, "list page")
		entries := pageEntries(t, page, full)
		if n := len(entries) + len(page.CommonPrefixes); n > int(aws.ToInt32(in.MaxKeys)) {
			t.Fatalf("max-keys %d: a page of %d", aws.ToInt32(in.MaxKeys), n)
		}
		got = append(got, entries...)
		for _, p := range page.CommonPrefixes {
			prefixes = append(prefixes, aws.ToString(p.Prefix))
		}
		if !aws.ToBool(page.IsTruncated) {
			return got, prefixes
		}
		if len(page.CommonPrefixes) == 0 {
			last := entries[len(entries)-1]
			requireEqual(t, aws.ToString(page.NextKeyMarker), last.Key, "next key marker")
			requireEqual(t, aws.ToString(page.NextVersionIdMarker), last.VersionID,
				"next version-id marker")
		} else {
			requireEqual(t, aws.ToString(page.NextVersionIdMarker), "",
				"a prefix page's version-id marker")
		}
		in.KeyMarker, in.VersionIdMarker = page.NextKeyMarker, page.NextVersionIdMarker
	}
	t.Fatalf("max-keys %d: paging does not end", aws.ToInt32(in.MaxKeys))
	return nil, nil
}

func TestListObjectVersionsPaging(t *testing.T) {
	srv := startLive(t, nil)
	c := clientFor(srv)
	ctx := t.Context()
	full := listingBucket(t, srv, c)
	bucket := aws.String("lvs")

	// Paging on the key and version-id markers repeats and skips nothing.
	for _, maxKeys := range []int32{1, 2, 3} {
		got, _ := pageAll(t, c, s3.ListObjectVersionsInput{
			Bucket: bucket, MaxKeys: aws.Int32(maxKeys),
		}, full)
		if !slices.Equal(got, full) {
			t.Fatalf("max-keys %d: pages %v, want %v", maxKeys, got, full)
		}
	}
	// With a delimiter, a page ended by the common prefix resumes after it.
	got, prefixes := pageAll(t, c, s3.ListObjectVersionsInput{
		Bucket: bucket, Delimiter: aws.String("/"), MaxKeys: aws.Int32(1),
	}, full)
	requireEqual(t, strings.Join(prefixes, ","), "b/", "delimited prefixes")
	requireEqual(t, len(got), 7, "delimited entries")

	// A key marker alone starts after that key.
	out := listVersions(t, c, s3.ListObjectVersionsInput{
		Bucket: bucket, KeyMarker: aws.String("a"),
	})
	requireEqual(t, len(out.Versions)+len(out.DeleteMarkers), 4, "after key a")
	// A prefix, and a delimiter rolling b/ into one common prefix.
	out = listVersions(t, c, s3.ListObjectVersionsInput{Bucket: bucket, Prefix: aws.String("b/")})
	requireEqual(t, len(out.Versions), 2, "under b/")
	out = listVersions(t, c, s3.ListObjectVersionsInput{
		Bucket: bucket, Delimiter: aws.String("/"),
	})
	requireEqual(t, len(out.CommonPrefixes), 1, "common prefixes")
	requireEqual(t, aws.ToString(out.CommonPrefixes[0].Prefix), "b/", "common prefix")
	requireEqual(t, len(out.Versions)+len(out.DeleteMarkers), 7, "entries beside the prefix")

	// A version-id marker needs a key marker; max-keys 0 is an empty page.
	_, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
		Bucket: bucket, VersionIdMarker: aws.String(full[0].VersionID),
	})
	requireAPIError(t, err, "InvalidArgument",
		"A version-id marker cannot be specified without a key marker.")
	zero, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
		Bucket: bucket, MaxKeys: aws.Int32(0),
	})
	requireNoErr(t, err, "max-keys 0")
	requireEqual(t, len(zero.Versions)+len(zero.DeleteMarkers), 0, "max-keys 0 entries")
	requireEqual(t, aws.ToBool(zero.IsTruncated), false, "max-keys 0 truncated")
}

// ListObjects and ListObjectsV2 list no key whose current version is a
// delete marker, and no common prefix all of whose keys are marked.
func TestListObjectsHidesDeleteMarkers(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	versionedBucket(t, c, "hide")
	for _, k := range []string{"live", "dead", "dir/dead", "dir2/live"} {
		putVersion(t, c, "hide", k, k)
	}
	for _, k := range []string{"dead", "dir/dead"} {
		_, err := c.DeleteObject(
			ctx,
			&s3.DeleteObjectInput{Bucket: aws.String("hide"), Key: aws.String(k)},
		)
		requireNoErr(t, err, "mark "+k)
	}

	v2, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("hide")})
	requireNoErr(t, err, "list v2")
	var keys []string
	for _, o := range v2.Contents {
		keys = append(keys, aws.ToString(o.Key))
	}
	requireEqual(t, strings.Join(keys, ","), "dir2/live,live", "v2 keys")
	v2, err = c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String("hide"), Delimiter: aws.String("/"),
	})
	requireNoErr(t, err, "list v2 delimited")
	requireEqual(t, len(v2.Contents), 1, "v2 delimited keys")
	requireEqual(t, len(v2.CommonPrefixes), 1, "v2 prefixes")
	requireEqual(t, aws.ToString(v2.CommonPrefixes[0].Prefix), "dir2/", "v2 prefix")

	v1, err := c.ListObjects(ctx, &s3.ListObjectsInput{Bucket: aws.String("hide")})
	requireNoErr(t, err, "list v1")
	keys = keys[:0]
	for _, o := range v1.Contents {
		keys = append(keys, aws.ToString(o.Key))
	}
	requireEqual(t, strings.Join(keys, ","), "dir2/live,live", "v1 keys")
	v1, err = c.ListObjects(ctx, &s3.ListObjectsInput{
		Bucket: aws.String("hide"), Delimiter: aws.String("/"),
	})
	requireNoErr(t, err, "list v1 delimited")
	requireEqual(t, len(v1.CommonPrefixes), 1, "v1 prefixes")
	requireEqual(t, aws.ToString(v1.CommonPrefixes[0].Prefix), "dir2/", "v1 prefix")
}

// Suspended, writes and deletes replace the null version and keep every
// other.
func TestSuspendedSemantics(t *testing.T) {
	srv := startLive(t, nil)
	c := clientFor(srv)
	ctx := t.Context()
	versionedBucket(t, c, "sus")
	v1 := putVersion(t, c, "sus", "k", "enabled")
	only := putVersion(t, c, "sus", "only", "enabled")
	setVersioning(t, c, "sus", types.BucketVersioningStatusSuspended)
	// versions lists key's versions as id or "M:"+id for a marker, newest
	// first.
	versions := func(key string) string {
		t.Helper()
		var ids []string
		for _, e := range rawVersions(t, srv, "sus", "&prefix="+key) {
			if e.Marker {
				ids = append(ids, "M:"+e.VersionID)
			} else {
				ids = append(ids, e.VersionID)
			}
		}
		return strings.Join(ids, ",")
	}

	requireEqual(t, putVersion(t, c, "sus", "k", "null one"), "null", "suspended PUT version")
	requireEqual(t, versions("k"), "null,"+v1, "null version on top")
	requireEqual(t, putVersion(t, c, "sus", "k", "null two"), "null", "suspended overwrite")
	requireEqual(t, versions("k"), "null,"+v1, "the null version replaced, the other kept")
	body, id := getVersion(t, c, "sus", "k", "")
	requireEqual(t, body, "null two", "current body")
	requireEqual(t, id, "null", "current version")
	body, _ = getVersion(t, c, "sus", "k", v1)
	requireEqual(t, body, "enabled", "the enabled version kept")

	// A delete replaces the null version with a null marker.
	del, err := c.DeleteObject(
		ctx,
		&s3.DeleteObjectInput{Bucket: aws.String("sus"), Key: aws.String("k")},
	)
	requireNoErr(t, err, "suspended delete")
	requireEqual(t, aws.ToBool(del.DeleteMarker), true, "suspended delete marker")
	requireEqual(t, aws.ToString(del.VersionId), "null", "suspended marker version")
	requireEqual(t, versions("k"), "M:null,"+v1, "null marker over the enabled version")
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("sus"), Key: aws.String("k")})
	requireMarkerResponse(t, err, http.StatusNotFound, "null")
	// A write replaces the null marker.
	requireEqual(t, putVersion(t, c, "sus", "k", "back"), "null", "write over the null marker")
	requireEqual(t, versions("k"), "null,"+v1, "null marker replaced")

	// A key without a null version only gains a marker.
	del, err = c.DeleteObject(
		ctx,
		&s3.DeleteObjectInput{Bucket: aws.String("sus"), Key: aws.String("only")},
	)
	requireNoErr(t, err, "delete a key with no null version")
	requireEqual(t, aws.ToString(del.VersionId), "null", "its marker version")
	requireEqual(t, versions("only"), "M:null,"+only, "marker over the kept version")
}

// A GET of ?partNumber with ?versionId serves that part of that version,
// an older one completed from two parts.
func TestGetPartOfAVersion(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	versionedBucket(t, c, "parts")
	bucket, key := aws.String("parts"), aws.String("m")
	up, err := c.CreateMultipartUpload(
		ctx,
		&s3.CreateMultipartUploadInput{Bucket: bucket, Key: key},
	)
	requireNoErr(t, err, "create upload")
	parts := make([]types.CompletedPart, 0, 2)
	for i, body := range [][]byte{bytes.Repeat([]byte("a"), 5<<20), []byte("bb")} {
		n := aws.Int32(int32(i + 1)) //nolint:gosec // two parts
		p, pErr := c.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: bucket, Key: key, UploadId: up.UploadId, PartNumber: n,
			Body: bytes.NewReader(body),
		})
		requireNoErr(t, pErr, "upload part")
		parts = append(parts, types.CompletedPart{PartNumber: n, ETag: p.ETag})
	}
	done, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: key, UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	requireNoErr(t, err, "complete upload")
	putVersion(t, c, "parts", "m", "new")

	out, body := get(t, c, &s3.GetObjectInput{
		Bucket: bucket, Key: key, VersionId: done.VersionId, PartNumber: aws.Int32(2),
	})
	requireEqual(t, string(body), "bb", "part 2 of the older version")
	requireEqual(t, aws.ToInt32(out.PartsCount), 2, "parts count")
	requireEqual(t, aws.ToString(out.VersionId), aws.ToString(done.VersionId), "version served")
}

// A conditional write treats a current delete marker as no object: "if
// the current object version is a delete marker, the write operation
// succeeds" under If-None-Match, and If-Match "fails with a 404 Not Found
// error" ("How to prevent object overwrites with conditional writes", S3
// User Guide).
func TestConditionalWritesOverADeleteMarker(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	versionedBucket(t, c, "cond")
	v1 := putVersion(t, c, "cond", "k", "x")
	_, err := c.DeleteObject(
		ctx,
		&s3.DeleteObjectInput{Bucket: aws.String("cond"), Key: aws.String("k")},
	)
	requireNoErr(t, err, "mark k")

	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("cond"), Key: aws.String("k"), Body: strings.NewReader("y"),
		IfMatch: aws.String("*"),
	})
	requireCode(t, err, "NoSuchKey")
	requireResponse(t, err, http.StatusNotFound)

	out, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("cond"), Key: aws.String("k"), Body: strings.NewReader("y"),
		IfNoneMatch: aws.String("*"),
	})
	requireNoErr(t, err, "If-None-Match * over a marker")
	id := aws.ToString(out.VersionId)
	if id == "" || id == "null" || id == v1 {
		t.Fatalf("If-None-Match * write answered version %q", id)
	}
	body, current := getVersion(t, c, "cond", "k", "")
	requireEqual(t, body, "y", "the version written")
	requireEqual(t, current, id, "current version")
}

// encoding-type=url encodes a listing's keys, prefix and key marker, as
// ListObjects encodes them here (url.PathEscape, "/" included).
func TestListObjectVersionsURLEncoding(t *testing.T) {
	srv := startLive(t, nil)
	c := clientFor(srv)
	versionedBucket(t, c, "enc")
	putVersion(t, c, "enc", "a b/é", "z")
	putVersion(t, c, "enc", "a c", "z")

	res := sendLive(t, signedRequest(t, srv, http.MethodGet,
		"/enc?versions&encoding-type=url&prefix=a%20&max-keys=1", nil, emptyHash, "s3",
		"us-east-1", time.Now()))
	requireEqual(t, res.StatusCode, http.StatusOK, "status: "+res.Body)
	for _, want := range []string{
		"<EncodingType>url</EncodingType>", "<Prefix>a%20</Prefix>",
		"<Key>a%20b%2F%C3%A9</Key>", "<NextKeyMarker>a%20b%2F%C3%A9</NextKeyMarker>",
		"<IsTruncated>true</IsTruncated>",
	} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("%s does not contain %s", res.Body, want)
		}
	}
}
