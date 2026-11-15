//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The tests here run the live server against the real identity service
// and check that a request naming a version is evaluated as its version
// action, as S3 evaluates it: "If you include a versionId in your request
// header, you must have the s3:GetObjectVersion permission" (GetObject API
// reference), and listing versions needs s3:ListBucketVersions.

// A policy granting only s3:GetObject reads the current version but not
// one named by versionId, which needs s3:GetObjectVersion, and only it.
func TestVersionReadNeedsGetObjectVersion(t *testing.T) {
	e := startTagEnv(t)
	ctx := t.Context()
	setVersioning(t, e.root, "bkt", types.BucketVersioningStatusEnabled)
	v1 := putVersion(t, e.root, "bkt", "k", "one")
	putVersion(t, e.root, "bkt", "k", "two")
	byVersion := &s3.GetObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(v1),
	}

	e.set(allowDoc(`"s3:GetObject"`, "*"))
	body, _ := getVersion(t, e.dev, "bkt", "k", "")
	requireEqual(t, body, "two", "current version with s3:GetObject")
	_, err := e.dev.GetObject(ctx, byVersion)
	requireDenied(t, err, notAllowed(devARN, "s3:GetObjectVersion", "arn:aws:s3:::bkt/k"))
	_, err = e.dev.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(v1),
	})
	requireResponse(t, err, http.StatusForbidden)

	e.set(allowDoc(`"s3:GetObjectVersion"`, "*"))
	body, _ = getVersion(t, e.dev, "bkt", "k", v1)
	requireEqual(t, body, "one", "a version with s3:GetObjectVersion")
	_, err = e.dev.GetObject(
		ctx,
		&s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")},
	)
	requireDenied(t, err, notAllowed(devARN, "s3:GetObject", "arn:aws:s3:::bkt/k"))

	// s3:versionid narrows the grant to one version.
	e.set(conditionDoc(`"s3:GetObjectVersion"`,
		`{"StringEquals":{"s3:versionid":"`+v1+`"}}`, `"s3:GetObject"`))
	body, _ = getVersion(t, e.dev, "bkt", "k", v1)
	requireEqual(t, body, "one", "the version the condition names")
	_, err = e.dev.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String("null"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:GetObjectVersion", "arn:aws:s3:::bkt/k"))
}

// A delete naming a version needs s3:DeleteObjectVersion; listing
// versions needs s3:ListBucketVersions, not s3:ListBucket; and setting the
// versioning state needs s3:PutBucketVersioning.
func TestVersionWriteAndListActions(t *testing.T) {
	e := startTagEnv(t)
	ctx := t.Context()
	e.set(allowDoc(`["s3:DeleteObject","s3:ListBucket"]`, "*"))
	_, err := e.dev.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String("bkt"),
		VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatusEnabled,
		},
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutBucketVersioning", "arn:aws:s3:::bkt"))
	setVersioning(t, e.root, "bkt", types.BucketVersioningStatusEnabled)
	v1 := putVersion(t, e.root, "bkt", "k", "one")

	_, err = e.dev.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(v1),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:DeleteObjectVersion", "arn:aws:s3:::bkt/k"))
	_, err = e.dev.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String("bkt")})
	requireDenied(t, err, notAllowed(devARN, "s3:ListBucketVersions", "arn:aws:s3:::bkt"))
	del, err := e.dev.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"),
	})
	requireNoErr(t, err, "simple delete with s3:DeleteObject")
	requireEqual(t, aws.ToBool(del.DeleteMarker), true, "marker created")

	e.set(
		allowDoc(
			`["s3:DeleteObjectVersion","s3:ListBucketVersions","s3:PutBucketVersioning"]`,
			"*",
		),
	)
	setVersioning(t, e.dev, "bkt", types.BucketVersioningStatusSuspended)
	out, err := e.dev.ListObjectVersions(
		ctx,
		&s3.ListObjectVersionsInput{Bucket: aws.String("bkt")},
	)
	requireNoErr(t, err, "list versions with s3:ListBucketVersions")
	requireEqual(t, len(out.Versions)+len(out.DeleteMarkers), 2, "versions listed")
	_, err = e.dev.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(v1),
	})
	requireNoErr(t, err, "delete a version with s3:DeleteObjectVersion")
}

// A missing version and a current delete marker are 404 only to a caller
// allowed s3:ListBucket; any other gets 403, without the marker's headers,
// so it learns nothing of the key.
func TestVersionNotFoundNeedsListBucket(t *testing.T) {
	e := startTagEnv(t)
	ctx := t.Context()
	setVersioning(t, e.root, "bkt", types.BucketVersioningStatusEnabled)
	putVersion(t, e.root, "bkt", "k", "one")
	other := putVersion(t, e.root, "bkt", "other", "x")
	_, err := e.root.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"),
	})
	requireNoErr(t, err, "mark k")
	missing := &s3.GetObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(other),
	}
	current := &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")}

	e.set(allowDoc(`["s3:GetObject","s3:GetObjectVersion"]`, "*"))
	for _, in := range []*s3.GetObjectInput{missing, current} {
		_, err = e.dev.GetObject(ctx, in)
		h := requireResponse(t, err, http.StatusForbidden)
		requireEqual(t, h.Get("X-Amz-Delete-Marker"), "", "a 403's x-amz-delete-marker")
		requireEqual(t, h.Get("X-Amz-Version-Id"), "", "a 403's x-amz-version-id")
	}

	e.set(allowDoc(`["s3:GetObject","s3:GetObjectVersion","s3:ListBucket"]`, "*"))
	_, err = e.dev.GetObject(ctx, missing)
	requireCode(t, err, "NoSuchVersion")
	_, err = e.dev.GetObject(ctx, current)
	requireCode(t, err, "NoSuchKey")
	h := requireResponse(t, err, http.StatusNotFound)
	requireEqual(t, h.Get("X-Amz-Delete-Marker"), "true", "a 404's x-amz-delete-marker")
}

// TestVersionActions checks that the tagging, ACL and attributes
// operations, and the source of a copy, naming a version are evaluated as
// their version actions, with s3:versionid and the version's own
// s3:ExistingObjectTag keys, and only them.
func TestVersionActions(t *testing.T) {
	e := startTagEnv(t)
	ctx := t.Context()
	setVersioning(t, e.root, "bkt", types.BucketVersioningStatusEnabled)
	tagged, err := e.root.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("one"),
		Tagging: aws.String("team=a"),
	})
	requireNoErr(t, err, "put a tagged v1")
	v1 := aws.ToString(tagged.VersionId)
	v2 := putVersion(t, e.root, "bkt", "k", "two")
	obj := "arn:aws:s3:::bkt/k"

	t.Run("tagging", func(t *testing.T) { versionTaggingActions(t, e, v1, v2, obj) })
	t.Run("acl", func(t *testing.T) { versionACLActions(t, e, v1, obj) })
	t.Run("attributes", func(t *testing.T) { versionAttributesActions(t, e, v1, v2, obj) })
	t.Run("copy", func(t *testing.T) { versionCopyActions(t, e, v1, v2, obj) })
	t.Run("delete marker", func(t *testing.T) { versionMarkerActions(t, e) })
}

// versionTaggingActions: Get, Put and DeleteObjectTagging of a version
// need s3:GetObjectVersionTagging, s3:PutObjectVersionTagging and
// s3:DeleteObjectVersionTagging, narrowed by s3:versionid and by the
// version's own tags; v1 is tagged team=a, v2 untagged.
func versionTaggingActions(t *testing.T, e *tagEnv, v1, v2, obj string) {
	ctx := t.Context()
	get := func(id string) error {
		_, err := e.dev.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
			Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(id),
		})
		return err //nolint:wrapcheck // the test inspects the SDK error
	}
	e.set(allowDoc(`["s3:GetObjectTagging","s3:PutObjectTagging","s3:DeleteObjectTagging"]`,
		"*"))
	requireDenied(t, get(v1), notAllowed(devARN, "s3:GetObjectVersionTagging", obj))
	_, err := e.dev.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(v2),
		Tagging: tags("x", "y"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutObjectVersionTagging", obj))
	_, err = e.dev.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(v2),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:DeleteObjectVersionTagging", obj))

	e.set(allowDoc(
		`["s3:GetObjectVersionTagging","s3:PutObjectVersionTagging",`+
			`"s3:DeleteObjectVersionTagging"]`, "*"))
	requireNoErr(t, get(v1), "get v1's tags with s3:GetObjectVersionTagging")
	_, err = e.dev.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(v2),
		Tagging: tags("x", "y"),
	})
	requireNoErr(t, err, "tag v2 with s3:PutObjectVersionTagging")
	_, err = e.dev.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(v2),
	})
	requireNoErr(t, err, "untag v2 with s3:DeleteObjectVersionTagging")

	e.set(conditionDoc(`"s3:GetObjectVersionTagging"`,
		`{"StringEquals":{"s3:versionid":"`+v2+`"}}`, `"s3:GetObject"`))
	requireNoErr(t, get(v2), "the version s3:versionid names")
	requireDenied(t, get(v1), notAllowed(devARN, "s3:GetObjectVersionTagging", obj))

	// The tags evaluated are the version's: v1's team=a, not the current
	// v2's.
	e.set(conditionDoc(`"s3:GetObjectVersionTagging"`,
		`{"StringEquals":{"s3:ExistingObjectTag/team":"a"}}`, `"s3:GetObject"`))
	requireNoErr(t, get(v1), "the version tagged team=a")
	requireDenied(t, get(v2), notAllowed(devARN, "s3:GetObjectVersionTagging", obj))
}

// versionACLActions: Get and PutObjectAcl of a version need
// s3:GetObjectVersionAcl and s3:PutObjectVersionAcl.
func versionACLActions(t *testing.T, e *tagEnv, v1, obj string) {
	ctx := t.Context()
	getACL := &s3.GetObjectAclInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(v1),
	}
	putACL := &s3.PutObjectAclInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(v1),
		ACL: types.ObjectCannedACLPrivate,
	}
	e.set(allowDoc(`["s3:GetObjectAcl","s3:PutObjectAcl"]`, "*"))
	_, err := e.dev.GetObjectAcl(ctx, getACL)
	requireDenied(t, err, notAllowed(devARN, "s3:GetObjectVersionAcl", obj))
	_, err = e.dev.PutObjectAcl(ctx, putACL)
	requireDenied(t, err, notAllowed(devARN, "s3:PutObjectVersionAcl", obj))

	e.set(conditionDoc(`["s3:GetObjectVersionAcl","s3:PutObjectVersionAcl"]`,
		`{"StringEquals":{"s3:versionid":"`+v1+`"}}`, `"s3:GetObject"`))
	_, err = e.dev.GetObjectAcl(ctx, getACL)
	requireNoErr(t, err, "get v1's ACL with s3:GetObjectVersionAcl")
	_, err = e.dev.PutObjectAcl(ctx, putACL)
	requireNoErr(t, err, "put v1's ACL with s3:PutObjectVersionAcl")
	_, err = e.dev.GetObjectAcl(ctx, &s3.GetObjectAclInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String("null"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:GetObjectVersionAcl", obj))
}

// versionAttributesActions: GetObjectAttributes of a version needs
// s3:GetObjectVersion and s3:GetObjectVersionAttributes, both with
// s3:versionid: "If you pass a version ID in your request, you need both"
// (its API reference).
func versionAttributesActions(t *testing.T, e *tagEnv, v1, v2, obj string) {
	ctx := t.Context()
	attrs := func(id string) error {
		_, err := e.dev.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{
			Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(id),
			ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesEtag},
		})
		return err //nolint:wrapcheck // the test inspects the SDK error
	}
	e.set(allowDoc(`["s3:GetObject","s3:GetObjectAttributes"]`, "*"))
	requireDenied(t, attrs(v1), notAllowed(devARN, "s3:GetObjectVersion", obj))
	e.set(allowDoc(`["s3:GetObjectVersion","s3:GetObjectAttributes"]`, "*"))
	requireDenied(t, attrs(v1), notAllowed(devARN, "s3:GetObjectVersionAttributes", obj))

	both := `["s3:GetObjectVersion","s3:GetObjectVersionAttributes"]`
	e.set(conditionDoc(both, `{"StringEquals":{"s3:versionid":"`+v1+`"}}`, `"s3:ListBucket"`))
	requireNoErr(t, attrs(v1), "v1's attributes with both version actions")
	requireDenied(t, attrs(v2), notAllowed(devARN, "s3:GetObjectVersion", obj))
	// The second check is bound to the version too: allowed s3:GetObjectVersion
	// on every version, the attributes of v2 are still refused.
	e.set(`{"Version":"2012-10-17","Statement":[` +
		`{"Effect":"Allow","Action":"s3:GetObjectVersion","Resource":"*"},` +
		`{"Effect":"Allow","Action":"s3:GetObjectVersionAttributes","Resource":"*",` +
		`"Condition":{"StringEquals":{"s3:versionid":"` + v1 + `"}}}]}`)
	requireNoErr(t, attrs(v1), "v1's attributes")
	requireDenied(t, attrs(v2), notAllowed(devARN, "s3:GetObjectVersionAttributes", obj))
}

// versionCopyActions: a copy's source named by version needs
// s3:GetObjectVersion, and, under the COPY tagging directive for a tagged
// source, s3:GetObjectVersionTagging, both with the source's
// s3:versionid; the destination's s3:PutObject has no s3:versionid, and
// its s3:x-amz-copy-source is <bucket>/<key> without the version.
func versionCopyActions(t *testing.T, e *tagEnv, v1, v2, obj string) {
	ctx := t.Context()
	copyOf := func(id, dst string) (*s3.CopyObjectOutput, error) {
		return e.dev.CopyObject(ctx, &s3.CopyObjectInput{ //nolint:wrapcheck // inspected
			Bucket: aws.String("bkt"), Key: aws.String(dst),
			CopySource: aws.String("bkt/k?versionId=" + id),
		})
	}
	up, err := e.root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("bkt"), Key: aws.String("mp"),
	})
	requireNoErr(t, err, "start an upload")
	partOf := func(id string) (*s3.UploadPartCopyOutput, error) {
		return e.dev.UploadPartCopy(ctx, &s3.UploadPartCopyInput{ //nolint:wrapcheck // inspected
			Bucket: aws.String("bkt"), Key: aws.String("mp"), UploadId: up.UploadId,
			PartNumber: aws.Int32(1), CopySource: aws.String("bkt/k?versionId=" + id),
		})
	}

	e.set(allowDoc(`["s3:GetObject","s3:GetObjectTagging","s3:PutObject",`+
		`"s3:PutObjectTagging"]`, "*"))
	_, err = copyOf(v1, "c")
	requireDenied(t, err, notAllowed(devARN, "s3:GetObjectVersion", obj))
	_, err = partOf(v2)
	requireDenied(t, err, notAllowed(devARN, "s3:GetObjectVersion", obj))

	// v1 is tagged, so a copy of it under COPY also reads its tags.
	e.set(allowDoc(`["s3:GetObjectVersion","s3:GetObjectTagging","s3:PutObject",`+
		`"s3:PutObjectTagging"]`, "*"))
	_, err = copyOf(v1, "c")
	requireDenied(t, err, notAllowed(devARN, "s3:GetObjectVersionTagging", obj))
	_, err = copyOf(v2, "c")
	requireNoErr(t, err, "copy the untagged v2 without its tag permission")

	// s3:versionid is on the source's checks alone, and
	// s3:x-amz-copy-source names no version.
	e.set(`{"Version":"2012-10-17","Statement":[` +
		`{"Effect":"Allow","Action":["s3:GetObjectVersion","s3:GetObjectVersionTagging"],` +
		`"Resource":"arn:aws:s3:::bkt/k","Condition":{"StringEquals":{"s3:versionid":"` + v1 +
		`"}}},` +
		`{"Effect":"Allow","Action":["s3:PutObject","s3:PutObjectTagging"],` +
		`"Resource":"arn:aws:s3:::bkt/*","Condition":{"Null":{"s3:versionid":"true"}}},` +
		`{"Effect":"Deny","Action":"s3:PutObject","Resource":"*","Condition":` +
		`{"StringNotEquals":{"s3:x-amz-copy-source":"bkt/k"}}}]}`)
	out, err := copyOf(v1, "c")
	requireNoErr(t, err, "copy v1 under conditions on each side")
	requireEqual(t, aws.ToString(out.CopySourceVersionId), v1, "the version copied")
	part, err := partOf(v1)
	requireNoErr(t, err, "copy v1 into a part under the same conditions")
	requireEqual(t, aws.ToString(part.CopySourceVersionId), v1, "the version part-copied")
	_, err = copyOf(v2, "c")
	requireDenied(t, err, notAllowed(devARN, "s3:GetObjectVersion", obj))
	_, err = partOf(v2)
	requireDenied(t, err, notAllowed(devARN, "s3:GetObjectVersion", obj))

	// A restore by copy is a copy like any other.
	restored, err := copyOf(v1, "k")
	requireNoErr(t, err, "restore v1 onto its own key")
	body, id := getVersion(t, e.root, "bkt", "k", "")
	requireEqual(t, body, "one", "the restored current version")
	requireEqual(t, id, aws.ToString(restored.VersionId), "the restored version's ID")
}

// versionMarkerActions: a caller allowed the version actions gets the
// delete marker answers, 405 for its tags and 400 as a copy source.
func versionMarkerActions(t *testing.T, e *tagEnv) {
	ctx := t.Context()
	putVersion(t, e.root, "bkt", "m", "x")
	del, err := e.root.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("m"),
	})
	requireNoErr(t, err, "mark m")
	marker := aws.ToString(del.VersionId)

	e.set(allowDoc(`["s3:GetObjectVersionTagging","s3:PutObjectVersionAcl",`+
		`"s3:GetObjectVersion","s3:PutObject"]`, "*"))
	_, err = e.dev.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: aws.String("bkt"), Key: aws.String("m"), VersionId: aws.String(marker),
	})
	h := requireResponse(t, err, http.StatusMethodNotAllowed)
	requireEqual(t, h.Get("X-Amz-Delete-Marker"), "true", "the marker's tagging 405")
	_, err = e.dev.PutObjectAcl(ctx, &s3.PutObjectAclInput{
		Bucket: aws.String("bkt"), Key: aws.String("m"), VersionId: aws.String(marker),
		ACL: types.ObjectCannedACLPrivate,
	})
	requireResponse(t, err, http.StatusMethodNotAllowed)
	_, err = e.dev.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("c"),
		CopySource: aws.String("bkt/m?versionId=" + marker),
	})
	requireCode(t, err, "InvalidRequest")
	requireResponse(t, err, http.StatusBadRequest)
}
