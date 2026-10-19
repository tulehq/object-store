package dirimpl

import (
	"context"
	"strings"
	"testing"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/config"

	"tule.dev/object-store/internal/contracts"
)

// TestPutRecordsETag guards the assumption etagReader relies on: the dir
// backend reads the body to EOF before it stores the metadata map.
func TestPutRecordsETag(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	meta := map[string]string{"owner": "me"}
	if _, err := s.PutObject(ctx, "bucket", "k", strings.NewReader("hello"),
		contracts.PutObjectInput{Metadata: meta}, contracts.Precondition{}); err != nil {
		t.Fatalf("put: %v", err)
	}

	info, err := s.StatObject(ctx, "bucket", "k")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if want := "5d41402abc4b2a76b9719d911017c592"; info.ETag != want {
		t.Fatalf("etag: got %q, want %q", info.ETag, want)
	}
	if len(info.Metadata) != 1 || info.Metadata["owner"] != "me" {
		t.Fatalf("metadata: got %v", info.Metadata)
	}
	if len(meta) != 1 {
		t.Fatalf("caller's metadata map was modified: %v", meta)
	}
}

// TestStaleIndexEntries checks that a key indexed without an object, as a
// crash during a write or delete can leave, is neither listed nor keeps its
// bucket from being deleted.
func TestStaleIndexEntries(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	if err := s.addToIndex("bucket", "ghost"); err != nil {
		t.Fatalf("index: %v", err)
	}

	out, err := s.ListObjects(ctx, "bucket", contracts.ListObjectsInput{MaxKeys: 1000})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out.Objects) != 0 {
		t.Fatalf("listed %d objects, want none", len(out.Objects))
	}

	if err = s.DeleteBucket(ctx, "bucket"); err != nil {
		t.Fatalf("delete bucket: %v", err)
	}
}

// newTestService returns an initialized Service on a fresh root, with a
// bucket named "bucket".
func newTestService(t *testing.T) *Service {
	t.Helper()

	cfg := config.New()
	if err := cfg.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}
	s := NewService(&Settings{RootDir: t.TempDir()})(cfg, &gas.NopLogger{})
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(ctx) })

	if _, err := s.CreateBucket(ctx, "bucket"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return s
}
