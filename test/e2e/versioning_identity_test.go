//go:build e2e

package e2e

import (
	"net/http"
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
