//go:build e2e

package e2e

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The test here loads a storage root v0.10.1 wrote, the one
// testdata/genroot made (see testdata/genroot/README.md), and versions it.

// rootFixture is the fixture.json genroot writes beside the root.
type rootFixture struct {
	Objects map[string]rootObject `json:"objects"` // by "bucket/key"
	Upload  struct {
		Bucket     string `json:"bucket"`
		Key        string `json:"key"`
		ID         string `json:"id"`
		PartETag   string `json:"partETag"`
		PartSHA256 string `json:"partSHA256"`
		PartSize   int64  `json:"partSize"`
	} `json:"upload"`
}

// rootObject is an object genroot wrote.
type rootObject struct {
	SHA256 string  `json:"sha256"`
	ETag   string  `json:"etag"`
	Parts  []int64 `json:"parts"`
	Size   int64   `json:"size"`
}

// legacyPolicy is the bucket policy genroot gave "legacy".
const legacyPolicy = `{"Version":"2012-10-17","Statement":[{"Sid":"ReadLegacy",` +
	`"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000000:root"},` +
	`"Action":"s3:GetObject","Resource":"arn:aws:s3:::legacy/*"}]}`

// The bodies the test writes over the legacy objects.
const (
	retaggedV1   = "retagged.txt, its first version"
	retaggedNull = "retagged.txt, written over while Suspended"
	publicV1     = "public.txt, its first version"
)

// extractRoot extracts testdata/v0.10.1-root.tar.gz to a temporary
// directory and returns its storage root and fixture.
func extractRoot(t *testing.T) (string, *rootFixture) {
	t.Helper()
	var fx rootFixture
	dir := extractTarball(t, "testdata/v0.10.1-root.tar.gz", &fx)
	if fx.Upload.ID == "" {
		t.Fatal("the fixture has no fixture.json")
	}
	return dir, &fx
}

// extractTarball extracts the root tarball name, as genroot writes it, to
// a temporary directory, decodes its fixture.json into fx and returns the
// storage root.
func extractTarball(t *testing.T, name string, fx any) string {
	t.Helper()
	f, err := os.Open(name) //nolint:gosec // G304: a tarball of this suite's testdata
	requireNoErr(t, err, "open the fixture")
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	requireNoErr(t, err, "read the fixture")
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	requireNoErr(t, err, "open the extraction directory")
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, nErr := tr.Next()
		if errors.Is(nErr, io.EOF) {
			break
		}
		requireNoErr(t, nErr, "read the fixture")
		if hdr.Name == "fixture.json" {
			requireNoErr(t, json.NewDecoder(tr).Decode(fx), "decode fixture.json")
			continue
		}
		extractEntry(t, root, hdr, tr)
	}
	return filepath.Join(dir, "root")
}

// extractEntry writes the directory or file hdr names in root, which
// refuses a name outside it.
func extractEntry(t *testing.T, root *os.Root, hdr *tar.Header, r io.Reader) {
	t.Helper()
	mode := hdr.FileInfo().Mode().Perm()
	switch hdr.Typeflag {
	case tar.TypeDir:
		requireNoErr(t, root.MkdirAll(hdr.Name, mode), "extract "+hdr.Name)
	case tar.TypeReg:
		out, err := root.OpenFile(hdr.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		requireNoErr(t, err, "extract "+hdr.Name)
		_, err = io.Copy(out, r)
		requireNoErr(t, errors.Join(err, out.Close()), "extract "+hdr.Name)
	default:
		t.Fatalf("the fixture holds %q, of type %c", hdr.Name, hdr.Typeflag)
	}
}

// TestDataDirFromV0_10_1 starts the server on a root v0.10.1 wrote: its
// objects read as they were written, as their keys' null versions once
// the bucket is versioned, and the root stays readable after a restart.
func TestDataDirFromV0_10_1(t *testing.T) {
	dir, fx := extractRoot(t)
	env := map[string]string{"TULE_Storage_RootDir": dir}
	var ids versionedIDs
	if !t.Run("loaded and versioned", func(t *testing.T) {
		c := clientFor(startLive(t, env))
		requireLegacyRoot(t, c, fx)
		ids = versionRoot(t, c, fx)
		requireVersionedRoot(t, c, fx, ids)
	}) {
		return
	}
	t.Run("after a restart", func(t *testing.T) {
		requireVersionedRoot(t, clientFor(startLive(t, env)), fx, ids)
	})
}

// requireLegacyRoot checks the root as v0.10.1 left it: in buckets never
// versioned, whose responses name no version.
func requireLegacyRoot(t *testing.T, c *s3.Client, fx *rootFixture) {
	t.Helper()
	ctx := t.Context()
	out, err := c.ListBuckets(ctx, &s3.ListBucketsInput{})
	requireNoErr(t, err, "list buckets")
	names := make([]string, 0, len(out.Buckets))
	for _, b := range out.Buckets {
		names = append(names, aws.ToString(b.Name))
	}
	if !slices.Equal(names, []string{"legacy", "legacy-writer"}) {
		t.Fatalf("buckets %v: the server does not serve the extracted root", names)
	}
	for _, bucket := range names {
		requireVersioningStatus(t, c, bucket, "")
	}
	requireUntouched(t, c, fx, "")
	requireLegacyObject(t, c, fx, "legacy", "tagged.txt", "")
	requireTags(t, c, "tagged.txt", "", map[string]string{"env": "dev", "team": "storage"})
	requireLegacyObject(t, c, fx, "legacy", "retagged.txt", "")
	requireTags(t, c, "retagged.txt", "", map[string]string{"stage": "new", "reviewed": "yes"})
	requireLegacyObject(t, c, fx, "legacy-writer", "public.txt", "")
	requirePublicRead(t, c, "")
	requireVersions(t, c, "legacy", []entry{
		{Key: "headers.txt", VersionID: "null", Latest: true},
		{Key: "multipart.bin", VersionID: "null", Latest: true},
		{Key: "plain.txt", VersionID: "null", Latest: true},
		{Key: "retagged.txt", VersionID: "null", Latest: true},
		{Key: "tagged.txt", VersionID: "null", Latest: true},
	})
	requireVersions(t, c, "legacy-writer", []entry{
		{Key: "public.txt", VersionID: "null", Latest: true},
	})
	requirePendingUpload(t, c, fx)
}

// versionedIDs are the IDs of the versions versionRoot writes.
type versionedIDs struct {
	retagged, public, pending string
}

// versionRoot enables versioning on both buckets, writes versions over
// the legacy objects, completes the legacy upload, suspends "legacy",
// writes over a legacy null version and deletes another.
func versionRoot(t *testing.T, c *s3.Client, fx *rootFixture) versionedIDs {
	t.Helper()
	setVersioning(t, c, "legacy", types.BucketVersioningStatusEnabled)
	setVersioning(t, c, "legacy-writer", types.BucketVersioningStatusEnabled)
	requireVersioningStatus(t, c, "legacy", types.BucketVersioningStatusEnabled)

	// Before any key is written again, its legacy object is its current
	// version, null, with its tags and ACL.
	requireUntouched(t, c, fx, "null")
	requireLegacyObject(t, c, fx, "legacy", "retagged.txt", "null")
	requireTags(t, c, "retagged.txt", "", map[string]string{"stage": "new", "reviewed": "yes"})
	requireLegacyObject(t, c, fx, "legacy-writer", "public.txt", "null")
	requirePublicRead(t, c, "")

	var ids versionedIDs
	ids.retagged = requireNewVersion(t, c, "legacy", "retagged.txt", retaggedV1)
	requireTags(t, c, "retagged.txt", "", map[string]string{})
	ids.public = requireNewVersion(t, c, "legacy-writer", "public.txt", publicV1)
	acl, err := c.GetObjectAcl(t.Context(), &s3.GetObjectAclInput{
		Bucket: aws.String("legacy-writer"), Key: aws.String("public.txt"),
	})
	requireNoErr(t, err, "get the ACL of the new version")
	requireEqual(t, len(acl.Grants), 1, "grants of a version written without an ACL")
	requireLegacyObject(t, c, fx, "legacy", "retagged.txt", "null")
	requireLegacyObject(t, c, fx, "legacy-writer", "public.txt", "null")
	t.Run("the legacy null version's tags and ACL by versionId", func(t *testing.T) {
		requireTags(t, c, "retagged.txt", "null",
			map[string]string{"stage": "new", "reviewed": "yes"})
		requirePublicRead(t, c, "null")
	})

	ids.pending = completePending(t, c, fx)

	setVersioning(t, c, "legacy", types.BucketVersioningStatusSuspended)
	requireEqual(t, putVersion(t, c, "legacy", "retagged.txt", retaggedNull), "null",
		"a Suspended write's version")
	requireTags(t, c, "retagged.txt", "", map[string]string{})

	del, err := c.DeleteObject(t.Context(), &s3.DeleteObjectInput{
		Bucket: aws.String("legacy"), Key: aws.String("tagged.txt"), VersionId: aws.String("null"),
	})
	requireNoErr(t, err, "delete the null version of tagged.txt")
	requireEqual(t, aws.ToString(del.VersionId), "null", "deleted version")
	requireEqual(t, aws.ToBool(del.DeleteMarker), false, "a delete marker deleted")
	return ids
}

// requireVersionedRoot checks the root as versionRoot left it.
func requireVersionedRoot(t *testing.T, c *s3.Client, fx *rootFixture, ids versionedIDs) {
	t.Helper()
	requireVersioningStatus(t, c, "legacy", types.BucketVersioningStatusSuspended)
	requireVersioningStatus(t, c, "legacy-writer", types.BucketVersioningStatusEnabled)
	requireUntouched(t, c, fx, "null")
	requireVersions(t, c, "legacy", []entry{
		{Key: "headers.txt", VersionID: "null", Latest: true},
		{Key: "multipart.bin", VersionID: "null", Latest: true},
		{Key: "pending.bin", VersionID: ids.pending, Latest: true},
		{Key: "plain.txt", VersionID: "null", Latest: true},
		{Key: "retagged.txt", VersionID: "null", Latest: true},
		{Key: "retagged.txt", VersionID: ids.retagged},
	})
	requireVersions(t, c, "legacy-writer", []entry{
		{Key: "public.txt", VersionID: ids.public, Latest: true},
		{Key: "public.txt", VersionID: "null"},
	})

	// The Suspended write replaced the legacy null version, and took
	// none of its tags.
	requireVersionBody(t, c, "legacy", "retagged.txt", "", retaggedNull, "null")
	requireVersionBody(t, c, "legacy", "retagged.txt", "null", retaggedNull, "null")
	requireVersionBody(t, c, "legacy", "retagged.txt", ids.retagged, retaggedV1, ids.retagged)
	requireTags(t, c, "retagged.txt", "", map[string]string{})

	_, err := c.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("legacy"), Key: aws.String("tagged.txt"),
	})
	requireCode(t, err, "NoSuchKey")

	requireVersionBody(t, c, "legacy-writer", "public.txt", "", publicV1, ids.public)
	requireLegacyObject(t, c, fx, "legacy-writer", "public.txt", "null")
	requireCompleted(t, c, fx, ids.pending)
}

// requireUntouched checks the legacy objects the test never writes over,
// and the bucket policy, answered as version, "" for none.
func requireUntouched(t *testing.T, c *s3.Client, fx *rootFixture, version string) {
	t.Helper()
	requireLegacyObject(t, c, fx, "legacy", "plain.txt", version)
	requireTags(t, c, "plain.txt", "", map[string]string{})
	requireLegacyObject(t, c, fx, "legacy", "headers.txt", version)
	requireHeaders(t, c)
	requireLegacyObject(t, c, fx, "legacy", "multipart.bin", version)
	requireLegacyParts(t, c, fx.Objects["legacy/multipart.bin"].Parts, version)

	out, err := c.GetBucketPolicy(
		t.Context(),
		&s3.GetBucketPolicyInput{Bucket: aws.String("legacy")},
	)
	requireNoErr(t, err, "get bucket policy")
	requireEqual(t, aws.ToString(out.Policy), legacyPolicy, "bucket policy")
}

// requireLegacyObject checks that GET and HEAD of bucket/key, by
// version, "null" or the current one for "", answer the object genroot
// wrote, named version, "" for no x-amz-version-id.
func requireLegacyObject(t *testing.T, c *s3.Client, fx *rootFixture, bucket, key, version string) {
	t.Helper()
	want, ok := fx.Objects[bucket+"/"+key]
	if !ok {
		t.Fatalf("the fixture has no %s/%s", bucket, key)
	}
	in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if version != "" {
		in.VersionId = aws.String(version)
	}
	out, body := get(t, c, in)
	requireEqual(t, sha256Hex(body), want.SHA256, key+" body")
	requireEqual(t, aws.ToString(out.ETag), want.ETag, key+" ETag")
	requireEqual(t, aws.ToString(out.VersionId), version, key+" GET x-amz-version-id")

	head, err := c.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: in.Bucket, Key: in.Key, VersionId: in.VersionId,
	})
	requireNoErr(t, err, "head "+key)
	requireEqual(t, aws.ToInt64(head.ContentLength), want.Size, key+" size")
	requireEqual(t, aws.ToString(head.ETag), want.ETag, key+" HEAD ETag")
	requireEqual(t, aws.ToString(head.VersionId), version, key+" HEAD x-amz-version-id")
}

// requireHeaders checks the metadata and system headers of
// legacy/headers.txt.
func requireHeaders(t *testing.T, c *s3.Client) {
	t.Helper()
	out, _ := get(
		t,
		c,
		&s3.GetObjectInput{Bucket: aws.String("legacy"), Key: aws.String("headers.txt")},
	)
	for _, h := range []struct{ name, got, want string }{
		{"Content-Type", aws.ToString(out.ContentType), "text/plain; charset=utf-8"},
		{"Cache-Control", aws.ToString(out.CacheControl), "max-age=60"},
		{
			"Content-Disposition", aws.ToString(out.ContentDisposition),
			`attachment; filename="headers.txt"`,
		},
		{"Content-Encoding", aws.ToString(out.ContentEncoding), "identity"},
		{"Content-Language", aws.ToString(out.ContentLanguage), "en-US"},
		{"Expires", aws.ToString(out.ExpiresString), "Wed, 02 Jan 2030 03:04:05 GMT"},
	} {
		requireEqual(t, h.got, h.want, "headers.txt "+h.name)
	}
	want := map[string]string{"color": "blue", "owner-team": "storage"}
	if !maps.Equal(out.Metadata, want) {
		t.Fatalf("headers.txt metadata %v, want %v", out.Metadata, want)
	}
}

// requireLegacyParts checks that legacy/multipart.bin has parts of sizes.
func requireLegacyParts(t *testing.T, c *s3.Client, sizes []int64, version string) {
	t.Helper()
	for i, size := range sizes {
		in := &s3.HeadObjectInput{
			Bucket: aws.String("legacy"), Key: aws.String("multipart.bin"),
			PartNumber: aws.Int32(int32(i + 1)),
		}
		if version != "" {
			in.VersionId = aws.String(version)
		}
		out, err := c.HeadObject(t.Context(), in)
		requireNoErr(t, err, "head a part of multipart.bin")
		requireEqual(t, int(aws.ToInt32(out.PartsCount)), len(sizes), "parts count")
		requireEqual(t, aws.ToInt64(out.ContentLength), size, "part size")
	}
}

// requireTags checks the tags of legacy/key, of version or the current
// one for "".
func requireTags(t *testing.T, c *s3.Client, key, version string, want map[string]string) {
	t.Helper()
	in := &s3.GetObjectTaggingInput{Bucket: aws.String("legacy"), Key: aws.String(key)}
	if version != "" {
		in.VersionId = aws.String(version)
	}
	out, err := c.GetObjectTagging(t.Context(), in)
	requireNoErr(t, err, "get tags of "+key)
	got := map[string]string{}
	for _, tag := range out.TagSet {
		got[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("tags of %s %q: %v, want %v", key, version, got, want)
	}
}

// requirePublicRead checks that legacy-writer/public.txt, of version or
// the current one for "", has the public-read ACL it was written with.
func requirePublicRead(t *testing.T, c *s3.Client, version string) {
	t.Helper()
	in := &s3.GetObjectAclInput{Bucket: aws.String("legacy-writer"), Key: aws.String("public.txt")}
	if version != "" {
		in.VersionId = aws.String(version)
	}
	acl, err := c.GetObjectAcl(t.Context(), in)
	requireNoErr(t, err, "get object acl")
	requireEqual(t, len(acl.Grants), 2, "public-read grants")
	requireEqual(t, aws.ToString(acl.Grants[1].Grantee.URI),
		"http://acs.amazonaws.com/groups/global/AllUsers", "AllUsers grantee")
	requireEqual(t, acl.Grants[1].Permission, types.PermissionRead, "AllUsers permission")
}

// requireNewVersion writes body at bucket/key and checks that it is a new
// version, not null, and the current one, and returns its ID.
func requireNewVersion(t *testing.T, c *s3.Client, bucket, key, body string) string {
	t.Helper()
	id := putVersion(t, c, bucket, key, body)
	if id == "" || id == "null" {
		t.Fatalf("a write of %s in an Enabled bucket answered version %q", key, id)
	}
	requireVersionBody(t, c, bucket, key, "", body, id)
	return id
}

// requireVersionBody checks that bucket/key, of version or the current one for
// "", reads body, answering wantVersion.
func requireVersionBody(
	t *testing.T,
	c *s3.Client,
	bucket, key, version, body, wantVersion string,
) {
	t.Helper()
	got, id := getVersion(t, c, bucket, key, version)
	requireEqual(t, got, body, key+" body of version "+version)
	requireEqual(t, id, wantVersion, key+" version answered")
}

// requireVersioningStatus checks bucket's versioning status.
func requireVersioningStatus(
	t *testing.T,
	c *s3.Client,
	bucket string,
	want types.BucketVersioningStatus,
) {
	t.Helper()
	out, err := c.GetBucketVersioning(
		t.Context(),
		&s3.GetBucketVersioningInput{Bucket: aws.String(bucket)},
	)
	requireNoErr(t, err, "get bucket versioning")
	requireEqual(t, out.Status, want, bucket+" versioning status")
	requireEqual(t, out.MFADelete, "", bucket+" MFA delete")
}

// requireVersions checks that bucket lists exactly the versions want, in
// order, and no delete markers.
func requireVersions(t *testing.T, c *s3.Client, bucket string, want []entry) {
	t.Helper()
	out := listVersions(t, c, s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	if len(out.DeleteMarkers) != 0 {
		t.Fatalf("%s lists %d delete markers", bucket, len(out.DeleteMarkers))
	}
	got := make([]entry, 0, len(out.Versions))
	for _, v := range out.Versions {
		got = append(got, entry{
			Key: aws.ToString(
				v.Key,
			),
			VersionID: aws.ToString(v.VersionId),
			Latest:    aws.ToBool(v.IsLatest),
		})
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s versions:\n got %v\nwant %v", bucket, got, want)
	}
}

// requirePendingUpload checks that the upload v0.10.1 left in progress is
// listed, with its part.
func requirePendingUpload(t *testing.T, c *s3.Client, fx *rootFixture) {
	t.Helper()
	up := fx.Upload
	ups, err := c.ListMultipartUploads(
		t.Context(),
		&s3.ListMultipartUploadsInput{Bucket: aws.String(up.Bucket)},
	)
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

// completePending completes the upload v0.10.1 left in progress, in an
// Enabled bucket, and returns the version it created.
func completePending(t *testing.T, c *s3.Client, fx *rootFixture) string {
	t.Helper()
	up := fx.Upload
	requirePendingUpload(t, c, fx)
	done, err := c.CompleteMultipartUpload(t.Context(), &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(up.Bucket), Key: aws.String(up.Key), UploadId: aws.String(up.ID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: aws.String(up.PartETag)},
		}},
	})
	requireNoErr(t, err, "complete the legacy upload")
	id := aws.ToString(done.VersionId)
	if id == "" || id == "null" {
		t.Fatalf("completing the legacy upload answered version %q", id)
	}
	requireCompleted(t, c, fx, id)
	return id
}

// requireCompleted checks the object completing the legacy upload wrote,
// as version id, and that the upload is gone.
func requireCompleted(t *testing.T, c *s3.Client, fx *rootFixture, id string) {
	t.Helper()
	up := fx.Upload
	out, body := get(
		t,
		c,
		&s3.GetObjectInput{Bucket: aws.String(up.Bucket), Key: aws.String(up.Key)},
	)
	requireEqual(t, sha256Hex(body), up.PartSHA256, "completed body")
	requireEqual(t, aws.ToString(out.VersionId), id, "completed version")
	requireEqual(t, out.Metadata["origin"], "v0.10.1", "completed metadata")
	ups, err := c.ListMultipartUploads(
		t.Context(),
		&s3.ListMultipartUploadsInput{Bucket: aws.String(up.Bucket)},
	)
	requireNoErr(t, err, "list multipart uploads")
	requireEqual(t, len(ups.Uploads), 0, "uploads in progress after completion")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
