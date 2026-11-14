//go:build e2e

package e2e

import (
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The tests here run the live server and check GetObjectAttributes.

var allAttributes = []types.ObjectAttributes{
	types.ObjectAttributesEtag, types.ObjectAttributesChecksum,
	types.ObjectAttributesObjectParts, types.ObjectAttributesStorageClass,
	types.ObjectAttributesObjectSize,
}

func attributes(
	t *testing.T,
	c *s3.Client,
	key string,
	edit func(*s3.GetObjectAttributesInput),
) (*s3.GetObjectAttributesOutput, error) {
	t.Helper()
	in := &s3.GetObjectAttributesInput{
		Bucket: aws.String("attrs"), Key: aws.String(key), ObjectAttributes: allAttributes,
	}
	if edit != nil {
		edit(in)
	}
	out, err := c.GetObjectAttributes(t.Context(), in)
	return out, err //nolint:wrapcheck // the test inspects the SDK error
}

// A plain object answers what is asked for, and only that.
func TestGetObjectAttributes(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	createBucket(t, c, "attrs")
	put(t, c, "attrs", "k", []byte("hello"))
	head, err := c.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String("attrs"), Key: aws.String("k"),
	})
	requireNoErr(t, err, "head")

	out, err := attributes(t, c, "k", nil)
	requireNoErr(t, err, "all attributes")
	requireEqual(t, `"`+aws.ToString(out.ETag)+`"`, aws.ToString(head.ETag), "ETag, unquoted")
	requireEqual(t, aws.ToInt64(out.ObjectSize), 5, "ObjectSize")
	requireEqual(t, out.StorageClass, types.StorageClassStandard, "StorageClass")
	requireEqual(t, out.ObjectParts == nil, true, "no ObjectParts for a plain object")
	requireEqual(t, out.Checksum == nil, true, "no Checksum")
	requireEqual(t, out.LastModified.Equal(aws.ToTime(head.LastModified)), true, "LastModified")

	out, err = attributes(t, c, "k", func(in *s3.GetObjectAttributesInput) {
		in.ObjectAttributes = []types.ObjectAttributes{types.ObjectAttributesObjectSize}
	})
	requireNoErr(t, err, "the size only")
	requireEqual(t, aws.ToInt64(out.ObjectSize), 5, "ObjectSize")
	requireEqual(t, aws.ToString(out.ETag), "", "no ETag")
	requireEqual(t, out.StorageClass, types.StorageClass(""), "no StorageClass")

	_, err = attributes(t, c, "missing", nil)
	requireCode(t, err, "NoSuchKey")
	_, err = attributes(t, c, "k", func(in *s3.GetObjectAttributesInput) {
		in.ObjectAttributes = []types.ObjectAttributes{"Owner"}
	})
	requireCode(t, err, "InvalidArgument")
}

// An object completed from a multipart upload lists its parts, paged by
// x-amz-max-parts and x-amz-part-number-marker.
func TestGetObjectAttributesParts(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	createBucket(t, c, "attrs")
	up := startUpload(t, c, "attrs", "mpu")
	sizes := []int{5 * mib, 5 * mib, 3}
	parts := make([]types.CompletedPart, len(sizes))
	for i, n := range sizes {
		parts[i] = up.part(int32(i+1), randomBytes(t, n)) //nolint:gosec // three parts
	}
	_, err := up.complete(parts...)
	requireNoErr(t, err, "complete")

	out, err := attributes(t, c, "mpu", nil)
	requireNoErr(t, err, "every part")
	requireParts(t, out.ObjectParts, "1:5242880,2:5242880,3:3", false, "")
	requireEqual(t, aws.ToInt32(out.ObjectParts.TotalPartsCount), 3, "TotalPartsCount")
	requireEqual(t, aws.ToInt32(out.ObjectParts.MaxParts), 1000, "MaxParts")
	requireEqual(t, strings.HasSuffix(aws.ToString(out.ETag), "-3"), true, "multipart ETag")

	out, err = attributes(t, c, "mpu", func(in *s3.GetObjectAttributesInput) {
		in.MaxParts = aws.Int32(2)
	})
	requireNoErr(t, err, "the first page")
	requireParts(t, out.ObjectParts, "1:5242880,2:5242880", true, "2")
	out, err = attributes(t, c, "mpu", func(in *s3.GetObjectAttributesInput) {
		in.MaxParts, in.PartNumberMarker = aws.Int32(2), out.ObjectParts.NextPartNumberMarker
	})
	requireNoErr(t, err, "the second page")
	requireParts(t, out.ObjectParts, "3:3", false, "")
	requireEqual(t, aws.ToString(out.ObjectParts.PartNumberMarker), "2", "PartNumberMarker")
}

// requireParts fails unless parts lists want, "number:size" each, and is
// truncated, with next as NextPartNumberMarker, as said.
func requireParts(
	t *testing.T,
	parts *types.GetObjectAttributesParts,
	want string,
	truncated bool,
	next string,
) {
	t.Helper()
	if parts == nil {
		t.Fatal("no ObjectParts")
	}
	got := make([]string, len(parts.Parts))
	for i, p := range parts.Parts {
		got[i] = strconv.Itoa(int(aws.ToInt32(p.PartNumber))) + ":" +
			strconv.FormatInt(aws.ToInt64(p.Size), 10)
	}
	requireEqual(t, strings.Join(got, ","), want, "parts")
	requireEqual(t, aws.ToBool(parts.IsTruncated), truncated, "IsTruncated")
	requireEqual(t, aws.ToString(parts.NextPartNumberMarker), next, "NextPartNumberMarker")
}

// GetObjectAttributes needs s3:GetObject and s3:GetObjectAttributes, and
// a missing key is NoSuchKey only to a caller allowed s3:ListBucket.
func TestGetObjectAttributesAuthorization(t *testing.T) {
	identity := startSibling(t, "identity", identityDirEnv)
	l := startLiveWithIdentity(t, identity.URL)
	ic := iamClient(identity.URL)
	root := l.client(rootCreds())
	createBucket(t, root, "attrs")
	put(t, root, "attrs", "k", []byte("k"))
	dev := l.client(createUserKey(t, ic, "dev"))

	putUserPolicy(t, ic, "dev", "p0", allowDoc(`["s3:GetObject"]`, "*"))
	_, err := attributes(t, dev, "k", nil)
	requireDenied(t, err, notAllowed(devARN, "s3:GetObjectAttributes", "arn:aws:s3:::attrs/k"))
	putUserPolicy(t, ic, "dev", "p0", allowDoc(`["s3:GetObjectAttributes"]`, "*"))
	_, err = attributes(t, dev, "k", nil)
	requireDenied(t, err, notAllowed(devARN, "s3:GetObject", "arn:aws:s3:::attrs/k"))

	putUserPolicy(t, ic, "dev", "p0", allowDoc(`["s3:GetObject","s3:GetObjectAttributes"]`, "*"))
	_, err = attributes(t, dev, "k", nil)
	requireNoErr(t, err, "both actions")
	_, err = attributes(t, dev, "missing", nil)
	requireDenied(t, err, notAllowed(devARN, "s3:ListBucket", "arn:aws:s3:::attrs"))
	putUserPolicy(t, ic, "dev", "p0",
		allowDoc(`["s3:GetObject","s3:GetObjectAttributes","s3:ListBucket"]`, "*"))
	_, err = attributes(t, dev, "missing", nil)
	requireCode(t, err, "NoSuchKey")
}

// Parts are reported by position: an upload completed from parts 1 and 3
// lists them as 1 and 2 (see "Parts are numbered by position" in the
// README).
func TestGetObjectAttributesNumbersPartsByPosition(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	createBucket(t, c, "attrs")
	up := startUpload(t, c, "attrs", "gap")
	first := up.part(1, randomBytes(t, 5*mib))
	third := up.part(3, randomBytes(t, 7))
	_, err := up.complete(first, third)
	requireNoErr(t, err, "complete from parts 1 and 3")
	out, err := attributes(t, c, "gap", nil)
	requireNoErr(t, err, "attributes")
	requireParts(t, out.ObjectParts, "1:5242880,2:7", false, "")
}
