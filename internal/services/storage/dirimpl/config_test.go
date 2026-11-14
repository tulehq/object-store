package dirimpl

import (
	"context"
	"strings"
	"testing"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

func TestBucketConfigs(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)

	for _, kind := range contracts.BucketConfigs {
		requireConfigLifecycle(t, s, ctx, kind)
	}
	// Read without a pin, under the bucket's lock.
	if err := s.PutBucketConfig(ctx, "bucket", contracts.ConfigCORS, []byte("<c/>")); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBucketConfig(context.Background(), "bucket", contracts.ConfigCORS)
	if err != nil || string(got) != "<c/>" {
		t.Fatalf("unpinned: %q %v", got, err)
	}

	_, err = s.GetBucketConfig(context.Background(), "missing", contracts.ConfigCORS)
	requireIs(t, "a missing bucket", err, storage.ErrBucketNotFound)
	for name, err := range map[string]error{
		"put":    s.PutBucketConfig(context.Background(), "bucket", contracts.ConfigCORS, nil),
		"delete": s.DeleteBucketConfig(context.Background(), "bucket", contracts.ConfigCORS),
		"acl":    s.PutObjectACL(context.Background(), "bucket", "k", nil),
	} {
		if err == nil {
			t.Errorf("%s without a pin succeeded", name)
		}
	}
}

// requireConfigLifecycle puts, reads and deletes the configuration kind
// of "bucket", ctx being pinned to it.
func requireConfigLifecycle(
	t *testing.T,
	s *Service,
	ctx context.Context, //nolint:revive // the test's pinned context, after the service
	kind contracts.BucketConfig,
) {
	t.Helper()
	_, err := s.GetBucketConfig(ctx, "bucket", kind)
	requireIs(t, string(kind)+" before", err, contracts.ErrNoSuchBucketConfig)
	for _, doc := range []string{"<a/>", "<b/>"} {
		if err = s.PutBucketConfig(ctx, "bucket", kind, []byte(doc)); err != nil {
			t.Fatal(err)
		}
		got, gErr := s.GetBucketConfig(ctx, "bucket", kind)
		if gErr != nil || string(got) != doc {
			t.Fatalf("%s: %q %v, want %q", kind, got, gErr, doc)
		}
	}
	if err = s.DeleteBucketConfig(ctx, "bucket", kind); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteBucketConfig(ctx, "bucket", kind); err != nil {
		t.Fatalf("%s delete again: %v", kind, err)
	}
	_, err = s.GetBucketConfig(ctx, "bucket", kind)
	requireIs(t, string(kind)+" after delete", err, contracts.ErrNoSuchBucketConfig)
}

// A bucket is created with the configuration it is given, and only that:
// what an interrupted delete left behind is removed.
func TestABucketsConfigGoesWithIt(t *testing.T) {
	s := newTestService(t)
	bg := context.Background()
	if err := s.writeFile(configFile(contracts.ConfigTagging, "left"), []byte("<t/>")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBucket(bg, "left", map[contracts.BucketConfig][]byte{
		contracts.ConfigOwnership: []byte("<o/>"),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetBucketConfig(bg, "left", contracts.ConfigTagging)
	requireIs(t, "a left-behind configuration", err, contracts.ErrNoSuchBucketConfig)
	got, err := s.GetBucketConfig(bg, "left", contracts.ConfigOwnership)
	if err != nil || string(got) != "<o/>" {
		t.Fatalf("the configuration created with: %q %v", got, err)
	}
}

// Deleting a bucket removes its configuration and its objects' ACLs.
func TestABucketsConfigIsDeletedWithIt(t *testing.T) {
	s := newTestService(t)
	bg := context.Background()
	if _, err := s.CreateBucket(bg, "left", map[contracts.BucketConfig][]byte{
		contracts.ConfigOwnership: []byte("<o/>"),
	}); err != nil {
		t.Fatal(err)
	}
	ctx, release, err := s.Pin(bg, ref(t, s, "left"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutObject(ctx, "left", "k", strings.NewReader("x"), contracts.PutObjectInput{
		ACL: []byte("<acl/>"),
	}, contracts.Precondition{}); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteObject(ctx, "left", "k"); err != nil {
		t.Fatal(err)
	}
	release()
	if err = s.DeleteBucket(bg, ref(t, s, "left")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		configFile(contracts.ConfigOwnership, "left"),
		aclFile("left", "k"),
	} {
		if _, sErr := s.root.Stat(name); sErr == nil {
			t.Errorf("%s outlived its bucket", name)
		}
	}
}

// An object's ACL is the one stored with it as it now is: one written
// over, or deleted and written again, has none until it is given one.
func TestObjectACLs(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	write := func(acl []byte) {
		t.Helper()
		if _, err := s.PutObject(ctx, "bucket", "k", strings.NewReader("x"),
			contracts.PutObjectInput{ACL: acl}, contracts.Precondition{}); err != nil {
			t.Fatal(err)
		}
	}

	_, err := s.GetObjectACL(ctx, "bucket", "k")
	requireIs(t, "no object", err, storage.ErrObjectNotFound)
	requireIs(t, "put on no object", s.PutObjectACL(ctx, "bucket", "k", []byte("<a/>")),
		storage.ErrObjectNotFound)

	write([]byte("<written/>"))
	if got, gErr := s.GetObjectACL(ctx, "bucket", "k"); gErr != nil || string(got) != "<written/>" {
		t.Fatalf("the ACL written with the object: %q %v", got, gErr)
	}
	if err = s.PutObjectACL(ctx, "bucket", "k", []byte("<put/>")); err != nil {
		t.Fatal(err)
	}
	if got, gErr := s.GetObjectACL(ctx, "bucket", "k"); gErr != nil || string(got) != "<put/>" {
		t.Fatalf("the ACL put: %q %v", got, gErr)
	}

	write(nil)
	_, err = s.GetObjectACL(ctx, "bucket", "k")
	requireIs(t, "written over without an ACL", err, contracts.ErrNoSuchBucketConfig)
	requireNoACLFile(t, s, "k", "written over without an ACL")
}

// requireNoACLFile fails if the ACL file of key in "bucket" exists.
func requireNoACLFile(t *testing.T, s *Service, key, what string) {
	t.Helper()
	if _, err := s.root.Stat(aclFile("bucket", key)); err == nil {
		t.Errorf("%s: the ACL file of %q is left", what, key)
	}
}

// Every path that ends or replaces an object removes or replaces its ACL
// file: a delete, a copy over it without an ACL, and a multipart upload
// completed over it without one.
func TestObjectACLFilesGoWithTheirObjects(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	withACL := func(key string) {
		t.Helper()
		if _, err := s.PutObject(ctx, "bucket", key, strings.NewReader("x"),
			contracts.PutObjectInput{ACL: []byte("<a/>")}, contracts.Precondition{}); err != nil {
			t.Fatal(err)
		}
	}

	withACL("deleted")
	if err := s.DeleteObject(ctx, "bucket", "deleted"); err != nil {
		t.Fatal(err)
	}
	requireNoACLFile(t, s, "deleted", "a delete")

	withACL("copied")
	if _, err := s.CopyObject(ctx, contracts.CopyObjectInput{
		SrcBucket: "bucket", SrcKey: "copied", DstBucket: "bucket", DstKey: "copied",
		Replace: &contracts.PutObjectInput{},
	}); err != nil {
		t.Fatal(err)
	}
	requireNoACLFile(t, s, "copied", "a copy without an ACL")

	withACL("uploaded")
	id, err := s.CreateMultipartUpload(ctx, "bucket", "uploaded", contracts.PutObjectInput{})
	if err != nil {
		t.Fatal(err)
	}
	etag, _, err := s.UploadPart(ctx, "bucket", "uploaded", id, 1, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteMultipartUpload(
		ctx,
		"bucket",
		"uploaded",
		id,
		[]contracts.CompletedPart{
			{PartNumber: 1, ETag: etag},
		},
		contracts.Precondition{},
	); err != nil {
		t.Fatal(err)
	}
	requireNoACLFile(t, s, "uploaded", "an upload completed without an ACL")
}

// UploadPart reports the headers the upload's object will be stored with.
func TestUploadPartReportsTheUploadsHeaders(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	id, err := s.CreateMultipartUpload(ctx, "bucket", "k", contracts.PutObjectInput{
		Headers: map[string]string{"X-Amz-Server-Side-Encryption": "aws:kms"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, headers, err := s.UploadPart(ctx, "bucket", "k", id, 1, strings.NewReader("x"))
	if err != nil || headers["X-Amz-Server-Side-Encryption"] != "aws:kms" {
		t.Fatalf("headers %v, %v", headers, err)
	}
}

// A copy gets the ACL and headers its request gives, not its source's ACL;
// an empty header removes it.
func TestACopysACL(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	if _, err := s.PutObject(ctx, "bucket", "k", strings.NewReader("x"), contracts.PutObjectInput{
		ACL: []byte("<source/>"), Headers: map[string]string{"Cache-Control": "no-cache"},
	}, contracts.Precondition{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.CopyObject(ctx, contracts.CopyObjectInput{
		SrcBucket: "bucket", SrcKey: "k", DstBucket: "bucket", DstKey: "copy",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.GetObjectACL(ctx, "bucket", "copy")
	requireIs(t, "a copy without an ACL", err, contracts.ErrNoSuchBucketConfig)
	if _, err = s.CopyObject(ctx, contracts.CopyObjectInput{
		SrcBucket: "bucket", SrcKey: "k", DstBucket: "bucket", DstKey: "copy",
		ACL: []byte("<copy/>"), Headers: map[string]string{"X-A": "1", "Cache-Control": ""},
	}); err != nil {
		t.Fatal(err)
	}
	if got, gErr := s.GetObjectACL(ctx, "bucket", "copy"); gErr != nil || string(got) != "<copy/>" {
		t.Fatalf("a copy's ACL: %q %v", got, gErr)
	}
	info, err := s.StatObject(ctx, "bucket", "copy")
	if err != nil || info.Headers["X-A"] != "1" {
		t.Fatalf("a copy's headers: %v %v", info, err)
	}
	if _, ok := info.Headers["Cache-Control"]; ok {
		t.Error("an empty header was stored")
	}
}
