package dirimpl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/config"
	"github.com/gasmod/gas/config/configtest"
	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// TestPutRecordsETag guards the assumption etagReader relies on: the trailer
// holding the metadata map is encoded once the body is read to EOF.
func TestPutRecordsETag(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)

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
	ctx, release := pin(t, s)

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

	release()
	if err = s.DeleteBucket(context.Background(), ref(t, s, "bucket")); err != nil {
		t.Fatalf("delete bucket: %v", err)
	}
}

// TestManyPartSizes checks that the part sizes of an upload of the most
// parts S3 allows are stored, which the backend's 2 KB metadata could not
// hold.
func TestManyPartSizes(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)

	sizes := make([]int64, contracts.MaxPartNumber)
	for i := range sizes {
		sizes[i] = contracts.MinPartSize + int64(i)
	}
	meta := map[string]string{etagMetaKey: "0123456789abcdef0123456789abcdef-10000"}
	joinParts(meta, sizes)

	if _, err := s.putObject(ctx, "bucket", "k", strings.NewReader("x"), "", meta); err != nil {
		t.Fatalf("put: %v", err)
	}
	info, err := s.StatObject(ctx, "bucket", "k")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if len(info.PartSizes) != len(sizes) || info.PartSizes[len(sizes)-1] != sizes[len(sizes)-1] {
		t.Fatalf("part sizes: got %d, want %d", len(info.PartSizes), len(sizes))
	}
	if info.ETag != meta[etagMetaKey] || info.Size != 1 {
		t.Fatalf("info: got etag %q size %d", info.ETag, info.Size)
	}
}

// newTestService returns an initialized Service on a fresh root, with a
// bucket named "bucket".
func newTestService(t *testing.T) *Service {
	t.Helper()

	cfg := config.New(config.WithProvider(configtest.NewStaticProvider(nil)))
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

// TestDamagedDataFile checks that an object whose data file was truncated or
// overwritten is an error, never a missing key or different contents.
func TestDamagedDataFile(t *testing.T) {
	for name, damage := range map[string]func(*os.File, int64) error{
		"truncated": func(f *os.File, size int64) error { return f.Truncate(size - 1) },
		"magic overwritten": func(f *os.File, size int64) error {
			if _, err := f.WriteAt([]byte{0}, size-1); err != nil {
				return fmt.Errorf("overwrite: %w", err)
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestService(t)
			ctx, _ := pin(t, s)
			if _, err := s.PutObject(ctx, "bucket", "k", strings.NewReader("hello"),
				contracts.PutObjectInput{}, contracts.Precondition{}); err != nil {
				t.Fatalf("put: %v", err)
			}

			damageDataFile(t, s.root, damage)

			_, err := s.StatObject(ctx, "bucket", "k")
			if err == nil || errors.Is(err, storage.ErrObjectNotFound) {
				t.Fatalf("stat: got %v, want an error other than not found", err)
			}
			_, err = s.GetObject(ctx, "bucket", "k")
			if err == nil || errors.Is(err, storage.ErrObjectNotFound) {
				t.Fatalf("get: got %v, want an error other than not found", err)
			}
		})
	}
}

// damageDataFile applies damage to the one backend data file in root.
func damageDataFile(t *testing.T, root *os.Root, damage func(*os.File, int64) error) {
	t.Helper()

	var files []string
	err := fs.WalkDir(root.FS(), ".gas/objects", func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			files = append(files, p)
		}
		return err
	})
	if err != nil || len(files) != 1 {
		t.Fatalf("data files: found %d, err %v; want 1", len(files), err)
	}

	f, err := root.OpenFile(files[0], os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open data file: %v", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat data file: %v", err)
	}
	if err = damage(f, info.Size()); err != nil {
		t.Fatalf("damage data file: %v", err)
	}
}

// ref is bucket as of its current generation.
func ref(t *testing.T, s *Service, bucket string) contracts.BucketRef {
	t.Helper()
	gen, _, err := s.GetBucketPolicy(context.Background(), bucket)
	if err != nil && !errors.Is(err, contracts.ErrNoSuchBucketPolicy) {
		t.Fatalf("read bucket %s: %v", bucket, err)
	}
	return contracts.BucketRef{Name: bucket, Generation: gen}
}

// pin pins the bucket "bucket" in its current generation until release,
// or the end of the test.
func pin(t *testing.T, s *Service) (context.Context, func()) {
	t.Helper()
	ctx, release, err := s.Pin(context.Background(), ref(t, s, "bucket"))
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	var once sync.Once
	release2 := func() { once.Do(release) }
	t.Cleanup(release2)
	return ctx, release2
}
