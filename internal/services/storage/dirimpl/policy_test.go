package dirimpl

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

func requireIs(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: got %v, want %v", what, err, want)
	}
}

func TestBucketPolicies(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)

	_, _, err := s.GetBucketPolicy(ctx, "bucket")
	requireIs(t, "no policy", err, contracts.ErrNoSuchBucketPolicy)
	for _, doc := range []string{`{"a":1}`, `{"b":2}`} {
		if err = s.PutBucketPolicy(ctx, "bucket", []byte(doc)); err != nil {
			t.Fatal(err)
		}
		_, got, gErr := s.GetBucketPolicy(ctx, "bucket")
		if gErr != nil || string(got) != doc {
			t.Fatalf("get: %q %v, want %q", got, gErr, doc)
		}
	}
	if err = s.DeleteBucketPolicy(ctx, "bucket"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteBucketPolicy(ctx, "bucket"); err != nil {
		t.Fatalf("delete again: %v", err)
	}
	_, _, err = s.GetBucketPolicy(ctx, "bucket")
	requireIs(t, "after delete", err, contracts.ErrNoSuchBucketPolicy)
}

func TestPoliciesOfAMissingBucket(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	_, _, err := s.GetBucketPolicy(ctx, "missing")
	requireIs(t, "get", err, storage.ErrBucketNotFound)
	_, _, err = s.Pin(ctx, contracts.BucketRef{Name: "missing"})
	requireIs(t, "pin", err, storage.ErrBucketNotFound)
}

// Every operation on a bucket needs a pin.
func TestOperationsNeedAPin(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	for name, err := range map[string]error{
		"put policy": s.PutBucketPolicy(ctx, "bucket", []byte("{}")),
		"delete":     s.DeleteObject(ctx, "bucket", "k"),
	} {
		if err == nil || errors.Is(err, storage.ErrBucketNotFound) {
			t.Errorf("%s without a pin: %v", name, err)
		}
	}
	if _, err := s.PutObject(ctx, "bucket", "k", strings.NewReader("x"),
		contracts.PutObjectInput{}, contracts.Precondition{}); err == nil {
		t.Error("put without a pin succeeded")
	}
}

// Deleting a bucket removes its policy, and a policy an interrupted delete
// left behind is not inherited by a bucket created under the same name.
func TestABucketsPolicyGoesWithIt(t *testing.T) {
	s := newTestService(t)
	ctx, release := pin(t, s)
	if err := s.PutBucketPolicy(ctx, "bucket", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	release()
	if err := s.DeleteBucket(context.Background(), ref(t, s, "bucket")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.root.Stat(policyFile("bucket")); err == nil {
		t.Fatal("the policy outlived its bucket")
	}

	if err := s.root.WriteFile(policyFile("left"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBucket(context.Background(), "left"); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.GetBucketPolicy(context.Background(), "left")
	requireIs(t, "a new bucket's policy", err, contracts.ErrNoSuchBucketPolicy)
}

// A bucket deleted and created again has a new generation: what was read
// of the old one does not pin, delete or complete an upload into the new.
func TestARecreatedBucketIsANewGeneration(t *testing.T) {
	s := newTestService(t)
	bg := context.Background()
	old := ref(t, s, "bucket")
	ctx, release := pin(t, s)
	id, err := s.CreateMultipartUpload(ctx, "bucket", "k", contracts.PutObjectInput{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UploadPart(ctx, "bucket", "k", id, 1, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	release()

	if err = s.DeleteBucket(bg, old); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateBucket(bg, "bucket"); err != nil {
		t.Fatal(err)
	}
	if now := ref(t, s, "bucket"); now.Generation == old.Generation {
		t.Fatalf("the recreated bucket kept generation %q", old.Generation)
	}
	_, _, err = s.Pin(bg, old)
	requireIs(t, "pin of the old generation", err, storage.ErrBucketNotFound)
	requireIs(t, "delete of the old generation", s.DeleteBucket(bg, old), storage.ErrBucketNotFound)

	ctx, _ = pin(t, s)
	_, err = s.CompleteMultipartUpload(ctx, "bucket", "k", id,
		[]contracts.CompletedPart{{PartNumber: 1, ETag: "9dd4e461268c8034f5c8564e155c67a6"}},
		contracts.Precondition{})
	requireIs(t, "complete an old upload", err, contracts.ErrNoSuchUpload)
	uploads, err := s.ListMultipartUploads(ctx, "bucket")
	if err != nil || len(uploads) != 0 {
		t.Fatalf("uploads of the new bucket: %v %v", uploads, err)
	}
}

// A bucket registered before generations were recorded has the legacy
// generation, and pins as such.
func TestALegacyBucketPins(t *testing.T) {
	s := newTestService(t)
	if err := s.root.Mkdir(bucketsDir+"/legacy", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := s.root.MkdirAll(indexDir+"/legacy", 0o750); err != nil {
		t.Fatal(err)
	}
	r := ref(t, s, "legacy")
	if r.Generation != legacyGeneration {
		t.Fatalf("generation %q", r.Generation)
	}
	if _, release, err := s.Pin(context.Background(), r); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
}
