package dirimpl

import (
	"context"
	"strings"
	"testing"

	"github.com/gasmod/gas"
)

// TestUploadRecordsETag guards the assumption etagReader relies on: the dir
// backend reads the body to EOF before it copies the metadata map.
func TestUploadRecordsETag(t *testing.T) {
	s := NewService(&Settings{RootDir: t.TempDir()})(gas.NewNopLogger()())
	if err := s.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if _, err := s.CreateBucket(ctx, "bucket"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	meta := map[string]string{"owner": "me"}
	if err := s.Upload(ctx, "k", strings.NewReader("hello"),
		gas.InBucket("bucket"), gas.WithMetadata(meta)); err != nil {
		t.Fatalf("upload: %v", err)
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
