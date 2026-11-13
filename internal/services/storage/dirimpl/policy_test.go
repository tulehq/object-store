package dirimpl

import (
	"context"
	"errors"
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
	ctx := context.Background()

	_, err := s.GetBucketPolicy(ctx, "bucket")
	requireIs(t, "no policy", err, contracts.ErrNoSuchBucketPolicy)
	for _, doc := range []string{`{"a":1}`, `{"b":2}`} {
		if err = s.PutBucketPolicy(ctx, "bucket", []byte(doc)); err != nil {
			t.Fatal(err)
		}
		got, gErr := s.GetBucketPolicy(ctx, "bucket")
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
	_, err = s.GetBucketPolicy(ctx, "bucket")
	requireIs(t, "after delete", err, contracts.ErrNoSuchBucketPolicy)
}

func TestPoliciesOfAMissingBucket(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	_, err := s.GetBucketPolicy(ctx, "missing")
	requireIs(t, "get", err, storage.ErrBucketNotFound)
	requireIs(t, "put", s.PutBucketPolicy(ctx, "missing", []byte("{}")), storage.ErrBucketNotFound)
	requireIs(t, "delete", s.DeleteBucketPolicy(ctx, "missing"), storage.ErrBucketNotFound)
}

// Deleting a bucket removes its policy, and a policy an interrupted delete
// left behind is not inherited by a bucket created under the same name.
func TestABucketsPolicyGoesWithIt(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	if err := s.PutBucketPolicy(ctx, "bucket", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBucket(ctx, "bucket"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.root.Stat(policyFile("bucket")); err == nil {
		t.Fatal("the policy outlived its bucket")
	}

	if err := s.root.WriteFile(policyFile("left"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBucket(ctx, "left"); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetBucketPolicy(ctx, "left")
	requireIs(t, "a new bucket's policy", err, contracts.ErrNoSuchBucketPolicy)
}
