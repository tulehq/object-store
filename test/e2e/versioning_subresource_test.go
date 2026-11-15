//go:build e2e

package e2e

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// The tests here check the operations on one object version other than
// its read and delete: its tags, ACL and attributes by ?versionId, and
// copies from a version named in x-amz-copy-source, as an unmodified SDK
// client sees them.

// versionHeader returns the x-amz-version-id of the response md, an SDK
// output's ResultMetadata, was read from, for an output that does not
// model it.
func versionHeader(t *testing.T, md middleware.Metadata) string {
	t.Helper()
	raw, ok := awsmiddleware.GetRawResponse(md).(*smithyhttp.Response)
	if !ok {
		t.Fatal("no raw response in the result metadata")
	}
	return raw.Header.Get("X-Amz-Version-Id")
}

// requireMarkerVersion405 checks that err is the 405 MethodNotAllowed of
// a delete marker named by version, id, for an operation other than
// GetObject and HeadObject: x-amz-delete-marker and x-amz-version-id, and
// no Last-Modified.
func requireMarkerVersion405(t *testing.T, err error, id, what string) {
	t.Helper()
	h := requireResponse(t, err, http.StatusMethodNotAllowed)
	requireEqual(t, h.Get("X-Amz-Delete-Marker"), "true", what+": x-amz-delete-marker")
	requireEqual(t, h.Get("X-Amz-Version-Id"), id, what+": x-amz-version-id")
	requireEqual(t, h.Get("Last-Modified"), "", what+": Last-Modified")
}

// Tags set on one version, by ?versionId, are that version's alone, and
// each tagging response names the version it read or changed.
func TestVersionTags(t *testing.T) {
	c := startLive(t, nil).testClient()
	ctx := t.Context()
	versionedBucket(t, c, "vtags")
	bucket, key := aws.String("vtags"), aws.String("k")
	v1 := putVersion(t, c, "vtags", "k", "one")
	v2 := putVersion(t, c, "vtags", "k", "two")

	put1, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: bucket, Key: key, VersionId: aws.String(v1), Tagging: tags("v", "one"),
	})
	requireNoErr(t, err, "tag v1")
	requireEqual(t, aws.ToString(put1.VersionId), v1, "PutObjectTagging's version")

	got1, err := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: bucket, Key: key, VersionId: aws.String(v1),
	})
	requireNoErr(t, err, "get v1's tags")
	requireEqual(t, aws.ToString(got1.VersionId), v1, "GetObjectTagging's version")
	requireEqual(t, len(got1.TagSet) == 1 && aws.ToString(got1.TagSet[0].Value) == "one",
		true, "v1's tags")
	cur, err := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "get the current version's tags")
	requireEqual(t, aws.ToString(cur.VersionId), v2, "the current version answered")
	requireEqual(t, len(cur.TagSet), 0, "v2 is untagged")

	h1, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: bucket, Key: key, VersionId: aws.String(v1),
	})
	requireNoErr(t, err, "head v1")
	requireEqual(t, aws.ToInt32(h1.TagCount), int32(1), "v1's tag count")
	h2, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "head v2")
	requireEqual(t, h2.TagCount == nil, true, "v2 has no tag count")

	// Tagging creates no version.
	requireEqual(t, len(listVersions(t, c, s3.ListObjectVersionsInput{Bucket: bucket}).Versions), 2,
		"versions after tagging")

	del, err := c.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{
		Bucket: bucket, Key: key, VersionId: aws.String(v1),
	})
	requireNoErr(t, err, "delete v1's tags")
	requireEqual(t, aws.ToString(del.VersionId), v1, "DeleteObjectTagging's version")
	got1, err = c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: bucket, Key: key, VersionId: aws.String(v1),
	})
	requireNoErr(t, err, "get v1's tags after the delete")
	requireEqual(t, len(got1.TagSet), 0, "v1's tags after the delete")

	_, err = c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		// A well-formed ID the server never issued.
		Bucket: bucket, Key: key, VersionId: aws.String("0000000000000AAAAAAAAAAAAA"),
	})
	requireCode(t, err, "NoSuchVersion")
}

// An ACL set on one version, by ?versionId, is that version's alone, and
// both ACL operations answer the version's x-amz-version-id.
func TestVersionACLs(t *testing.T) {
	c := startLive(t, nil).testClient()
	ctx := t.Context()
	bucket, key := aws.String("vacl"), aws.String("k")
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: bucket, ObjectOwnership: types.ObjectOwnershipObjectWriter,
	})
	requireNoErr(t, err, "create an ObjectWriter bucket")
	_, err = c.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{Bucket: bucket})
	requireNoErr(t, err, "delete public access block")
	setVersioning(t, c, "vacl", types.BucketVersioningStatusEnabled)
	v1 := putVersion(t, c, "vacl", "k", "one")
	v2 := putVersion(t, c, "vacl", "k", "two")

	put1, err := c.PutObjectAcl(ctx, &s3.PutObjectAclInput{
		Bucket: bucket, Key: key, VersionId: aws.String(v1), ACL: types.ObjectCannedACLPublicRead,
	})
	requireNoErr(t, err, "make v1 public-read")
	requireEqual(t, versionHeader(t, put1.ResultMetadata), v1, "PutObjectAcl's version")

	acl1, err := c.GetObjectAcl(ctx, &s3.GetObjectAclInput{
		Bucket: bucket, Key: key, VersionId: aws.String(v1),
	})
	requireNoErr(t, err, "get v1's ACL")
	requireEqual(t, len(acl1.Grants), 2, "v1's public-read grants")
	requireEqual(t, versionHeader(t, acl1.ResultMetadata), v1, "GetObjectAcl's version")

	acl2, err := c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "get the current version's ACL")
	requireEqual(t, len(acl2.Grants), 1, "v2 keeps the owner's grant alone")
	requireEqual(t, versionHeader(t, acl2.ResultMetadata), v2, "the current version answered")
}

// A delete marker has no tags and no ACL: the tagging and ACL operations
// of a marker named by version are 405 MethodNotAllowed with
// x-amz-delete-marker, and without one, while the current version is a
// marker, 404 NoSuchKey. An older version is still served.
func TestVersionSubresourcesOfADeleteMarker(t *testing.T) {
	c := startLive(t, nil).testClient()
	ctx := t.Context()
	versionedBucket(t, c, "vmark")
	bucket, key := aws.String("vmark"), aws.String("k")
	v1 := putVersion(t, c, "vmark", "k", "one")
	del, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "mark k")
	m := aws.ToString(del.VersionId)
	marker := aws.String(m)

	_, err = c.GetObjectTagging(ctx,
		&s3.GetObjectTaggingInput{Bucket: bucket, Key: key, VersionId: marker})
	requireMarkerVersion405(t, err, m, "GetObjectTagging")
	_, err = c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: bucket, Key: key, VersionId: marker, Tagging: tags("a", "b"),
	})
	requireMarkerVersion405(t, err, m, "PutObjectTagging")
	_, err = c.DeleteObjectTagging(ctx,
		&s3.DeleteObjectTaggingInput{Bucket: bucket, Key: key, VersionId: marker})
	requireMarkerVersion405(t, err, m, "DeleteObjectTagging")
	_, err = c.GetObjectAcl(ctx,
		&s3.GetObjectAclInput{Bucket: bucket, Key: key, VersionId: marker})
	requireMarkerVersion405(t, err, m, "GetObjectAcl")
	_, err = c.PutObjectAcl(ctx, &s3.PutObjectAclInput{
		Bucket: bucket, Key: key, VersionId: marker, ACL: types.ObjectCannedACLPrivate,
	})
	requireMarkerVersion405(t, err, m, "PutObjectAcl")

	_, err = c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: bucket, Key: key})
	requireCode(t, err, "NoSuchKey")
	h := requireResponse(t, err, http.StatusNotFound)
	requireEqual(t, h.Get("X-Amz-Delete-Marker"), "", "a tagging 404's x-amz-delete-marker")
	_, err = c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: bucket, Key: key})
	requireCode(t, err, "NoSuchKey")

	_, err = c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: bucket, Key: key, VersionId: aws.String(v1), Tagging: tags("a", "b"),
	})
	requireNoErr(t, err, "tag the version under the marker")
}

// GetObjectAttributes by ?versionId answers that version's attributes and
// version ID; a marker named by version is 405 and a current marker 404,
// both with x-amz-delete-marker and the marker's x-amz-version-id.
func TestVersionAttributes(t *testing.T) {
	c := startLive(t, nil).testClient()
	ctx := t.Context()
	versionedBucket(t, c, "vattr")
	bucket, key := aws.String("vattr"), aws.String("k")
	v1 := putVersion(t, c, "vattr", "k", "one")
	v2 := putVersion(t, c, "vattr", "k", "second")
	attrs := []types.ObjectAttributes{types.ObjectAttributesEtag, types.ObjectAttributesObjectSize}

	a1, err := c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{
		Bucket: bucket, Key: key, VersionId: aws.String(v1), ObjectAttributes: attrs,
	})
	requireNoErr(t, err, "v1's attributes")
	requireEqual(t, aws.ToString(a1.VersionId), v1, "v1's x-amz-version-id")
	requireEqual(t, aws.ToInt64(a1.ObjectSize), int64(3), "v1's size")
	requireEqual(t, `"`+aws.ToString(a1.ETag)+`"`, md5Hex([]byte("one")), "v1's ETag")
	a2, err := c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{
		Bucket: bucket, Key: key, ObjectAttributes: attrs,
	})
	requireNoErr(t, err, "the current version's attributes")
	requireEqual(t, aws.ToString(a2.VersionId), v2, "the current version answered")
	requireEqual(t, aws.ToInt64(a2.ObjectSize), int64(6), "v2's size")

	del, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "mark k")
	m := aws.ToString(del.VersionId)
	_, err = c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{
		Bucket: bucket, Key: key, VersionId: aws.String(m), ObjectAttributes: attrs,
	})
	requireMarkerVersion405(t, err, m, "GetObjectAttributes of the marker")
	_, err = c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{
		Bucket: bucket, Key: key, ObjectAttributes: attrs,
	})
	requireCode(t, err, "NoSuchKey")
	h := requireResponse(t, err, http.StatusNotFound)
	requireEqual(t, h.Get("X-Amz-Delete-Marker"), "true", "a current marker's header")
	requireEqual(t, h.Get("X-Amz-Version-Id"), m, "a current marker's version")
}

// CopyObject and UploadPartCopy copy the version x-amz-copy-source names
// with ?versionId and answer it as x-amz-copy-source-version-id.
func TestCopyFromVersion(t *testing.T) {
	c := startLive(t, nil).testClient()
	ctx := t.Context()
	versionedBucket(t, c, "vcopy")
	bucket := aws.String("vcopy")
	v1 := putVersion(t, c, "vcopy", "k", "one")
	putVersion(t, c, "vcopy", "k", "two")
	fromV1 := aws.String("vcopy/k?versionId=" + v1)

	out, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: aws.String("c"), CopySource: fromV1,
	})
	requireNoErr(t, err, "copy v1")
	requireEqual(t, aws.ToString(out.CopySourceVersionId), v1, "CopyObject's source version")
	if aws.ToString(out.VersionId) == "" {
		t.Fatal("the copy has no version")
	}
	body, _ := getVersion(t, c, "vcopy", "c", "")
	requireEqual(t, body, "one", "the copy of v1")

	up := startUpload(t, c, "vcopy", "mp")
	part, err := c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
		Bucket: up.bucket, Key: up.key, UploadId: up.id, PartNumber: aws.Int32(1),
		CopySource: fromV1,
	})
	requireNoErr(t, err, "copy v1 into a part")
	requireEqual(t, aws.ToString(part.CopySourceVersionId), v1,
		"UploadPartCopy's source version")
	_, err = up.complete(types.CompletedPart{
		PartNumber: aws.Int32(1), ETag: part.CopyPartResult.ETag,
	})
	requireNoErr(t, err, "complete the upload")
	body, _ = getVersion(t, c, "vcopy", "mp", "")
	requireEqual(t, body, "one", "the part copy of v1")

	// A source in an Unversioned bucket has no version to answer.
	createBucket(t, c, "plain")
	put(t, c, "plain", "k", []byte("p"))
	plain, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("plain"), Key: aws.String("c"), CopySource: aws.String("plain/k"),
	})
	requireNoErr(t, err, "copy from an Unversioned bucket")
	requireEqual(t, plain.CopySourceVersionId == nil, true,
		"no source version from an Unversioned bucket")
}

// Copying a previous version onto its own key restores it: the copy is a
// new current version and every version is kept (S3 User Guide,
// "Restoring previous versions"). A copy onto itself without a version,
// or in an Unversioned bucket, is still refused.
func TestRestoreByCopy(t *testing.T) {
	c := startLive(t, nil).testClient()
	ctx := t.Context()
	versionedBucket(t, c, "vrestore")
	bucket, key := aws.String("vrestore"), aws.String("k")
	v1 := putVersion(t, c, "vrestore", "k", "one")
	v2 := putVersion(t, c, "vrestore", "k", "two")

	out, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: key, CopySource: aws.String("vrestore/k?versionId=" + v1),
	})
	requireNoErr(t, err, "restore v1")
	v3 := aws.ToString(out.VersionId)
	if v3 == "" || v3 == v1 || v3 == v2 {
		t.Fatalf("the restored copy's version is %q", v3)
	}
	requireEqual(t, aws.ToString(out.CopySourceVersionId), v1, "the restored version")
	body, id := getVersion(t, c, "vrestore", "k", "")
	requireEqual(t, body, "one", "the current version after the restore")
	requireEqual(t, id, v3, "the current version's ID")
	requireEqual(t, len(listVersions(t, c, s3.ListObjectVersionsInput{Bucket: bucket}).Versions), 3,
		"every version kept")

	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: key, CopySource: aws.String("vrestore/k"),
	})
	requireCode(t, err, "InvalidRequest")

	createBucket(t, c, "plain")
	put(t, c, "plain", "k", []byte("p"))
	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("plain"), Key: key, CopySource: aws.String("plain/k?versionId=null"),
	})
	requireCode(t, err, "InvalidRequest")
	requireResponse(t, err, http.StatusBadRequest)
}

// A copy source naming a delete marker by version is 400 InvalidRequest;
// one whose current version is a marker, named without a version, is 404
// NoSuchKey.
func TestCopyFromADeleteMarker(t *testing.T) {
	c := startLive(t, nil).testClient()
	ctx := t.Context()
	versionedBucket(t, c, "vdel")
	bucket := aws.String("vdel")
	putVersion(t, c, "vdel", "k", "one")
	del, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: aws.String("k")})
	requireNoErr(t, err, "mark k")
	fromMarker := aws.String("vdel/k?versionId=" + aws.ToString(del.VersionId))

	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: aws.String("c"), CopySource: fromMarker,
	})
	requireAPIError(t, err, "InvalidRequest",
		"The source of a copy request may not specifically refer to a delete marker by version id.")
	requireResponse(t, err, http.StatusBadRequest)
	up := startUpload(t, c, "vdel", "mp")
	_, err = c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
		Bucket: up.bucket, Key: up.key, UploadId: up.id, PartNumber: aws.Int32(1),
		CopySource: fromMarker,
	})
	requireCode(t, err, "InvalidRequest")
	requireResponse(t, err, http.StatusBadRequest)

	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: aws.String("c"), CopySource: aws.String("vdel/k"),
	})
	requireCode(t, err, "NoSuchKey")
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String("c")})
	requireResponse(t, err, http.StatusNotFound)
}

// In a Suspended bucket a copy writes the null version, so a restore by
// copy replaces the key's null version and keeps every other: copying an
// older version onto its key makes it the new null version, and copying
// the null version onto itself, which replaces its own source while it is
// read, leaves the null version with its source's bytes and tags.
func TestRestoreByCopySuspended(t *testing.T) {
	c := startLive(t, nil).testClient()
	ctx := t.Context()
	versionedBucket(t, c, "vsusp")
	bucket, key := aws.String("vsusp"), aws.String("k")
	v1 := putVersion(t, c, "vsusp", "k", "one")
	setVersioning(t, c, "vsusp", types.BucketVersioningStatusSuspended)
	requireEqual(t, putVersion(t, c, "vsusp", "k", "suspended"), "null", "the null version")

	out, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: key, CopySource: aws.String("vsusp/k?versionId=" + v1),
	})
	requireNoErr(t, err, "restore v1 in a Suspended bucket")
	requireEqual(t, aws.ToString(out.VersionId), "null", "the restored copy's version")
	requireEqual(t, aws.ToString(out.CopySourceVersionId), v1, "the version restored")
	body, id := getVersion(t, c, "vsusp", "k", "")
	requireEqual(t, body, "one", "the current version after the restore")
	requireEqual(t, id, "null", "the current version's ID")
	body, _ = getVersion(t, c, "vsusp", "k", v1)
	requireEqual(t, body, "one", "v1 kept")
	listed := listVersions(t, c, s3.ListObjectVersionsInput{Bucket: bucket})
	requireEqual(t, len(listed.Versions), 2, "v1 and one null version")

	// The null version copied onto itself: larger than one read buffer, and
	// tagged, so a copy that read its source after replacing it, or lost
	// its tags with it, shows.
	self := randomBytes(t, 6*mib)
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: key, Body: bytes.NewReader(self), Tagging: aws.String("a=b"),
	})
	requireNoErr(t, err, "write the null version to copy")
	out, err = c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: key, CopySource: aws.String("vsusp/k?versionId=null"),
	})
	requireNoErr(t, err, "copy the null version onto itself")
	requireEqual(t, aws.ToString(out.VersionId), "null", "the self-copy's version")
	requireEqual(t, aws.ToString(out.CopySourceVersionId), "null", "the version copied")
	requireEqual(t, aws.ToString(out.CopyObjectResult.ETag), md5Hex(self), "the copy's ETag")
	got, _ := get(t, c, &s3.GetObjectInput{Bucket: bucket, Key: key, VersionId: aws.String("null")})
	requireEqual(t, aws.ToString(got.VersionId), "null", "the null version read")
	_, data := get(t, c, &s3.GetObjectInput{Bucket: bucket, Key: key})
	requireEqual(t, bytes.Equal(data, self), true, "the self-copy's bytes equal its source's")
	tagsOut, err := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: bucket, Key: key, VersionId: aws.String("null"),
	})
	requireNoErr(t, err, "the self-copy's tags")
	requireEqual(t, len(tagsOut.TagSet) == 1 && aws.ToString(tagsOut.TagSet[0].Key) == "a",
		true, "the self-copy keeps its source's tags")
	body, _ = getVersion(t, c, "vsusp", "k", v1)
	requireEqual(t, body, "one", "v1 kept after the self-copy")
	listed = listVersions(t, c, s3.ListObjectVersionsInput{Bucket: bucket})
	requireEqual(t, len(listed.Versions), 2, "still v1 and one null version")
	requireEqual(t, len(listed.DeleteMarkers), 0, "no delete marker")
}
