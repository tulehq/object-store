package dirimpl

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // S3 ETags are MD5 digests, not a security control
	"encoding/hex"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

var testTags = []contracts.Tag{{Key: "env", Value: "prod"}, {Key: "team", Value: "storage"}}

// requireTags fails unless the object at key in "bucket" has tags, as
// GetObjectTagging and GetObject read them.
func requireTags(
	t *testing.T,
	s *Service,
	ctx context.Context, //nolint:revive // the test's pinned context, after the service
	key string,
	want []contracts.Tag,
	what string,
) {
	t.Helper()
	got, err := s.GetObjectTagging(ctx, "bucket", key)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("%s: tags %v, %v; want %v", what, got, err, want)
	}
	obj, err := s.GetObject(ctx, "bucket", key)
	if err != nil {
		t.Fatal(err)
	}
	if err = obj.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(obj.Tags, want) {
		t.Fatalf("%s: GetObject tags %v, want %v", what, obj.Tags, want)
	}
}

func requireNoTagFile(t *testing.T, s *Service, key, what string) {
	t.Helper()
	if _, err := s.root.Stat(tagFile("bucket", key)); err == nil {
		t.Errorf("%s: the tag file of %q is left", what, key)
	}
}

// putTagged writes key in "bucket" with tags.
func putTagged(
	t *testing.T,
	s *Service,
	ctx context.Context, //nolint:revive // the test's pinned context, after the service
	key string,
	tags []contracts.Tag,
) {
	t.Helper()
	if _, err := s.PutObject(ctx, "bucket", key, strings.NewReader("x"),
		contracts.PutObjectInput{Tags: tags}, contracts.Precondition{}); err != nil {
		t.Fatal(err)
	}
}

// An object's tags are the ones stored with it as it now is: one written
// over, or deleted, has none until it is given some.
func TestObjectTagsGoWithTheirObjects(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	put := func(key string, tags []contracts.Tag) { t.Helper(); putTagged(t, s, ctx, key, tags) }

	_, err := s.GetObjectTagging(ctx, "bucket", "k")
	requireIs(t, "no object", err, storage.ErrObjectNotFound)
	requireIs(t, "put on no object", s.PutObjectTagging(ctx, "bucket", "k", testTags),
		storage.ErrObjectNotFound)

	put("k", testTags)
	requireTags(t, s, ctx, "k", testTags, "written with tags")
	// Read without a pin, under the bucket's lock, as authorization reads
	// them.
	if got, gErr := s.GetObjectTagging(context.Background(), "bucket", "k"); gErr != nil ||
		!slices.Equal(got, testTags) {
		t.Fatalf("unpinned: %v %v", got, gErr)
	}
	put("k", nil)
	requireTags(t, s, ctx, "k", nil, "written over without tags")
	requireNoTagFile(t, s, "k", "written over without tags")

	one := []contracts.Tag{{Key: "a", Value: ""}}
	if err = s.PutObjectTagging(ctx, "bucket", "k", one); err != nil {
		t.Fatal(err)
	}
	requireTags(t, s, ctx, "k", one, "PutObjectTagging")
	if err = s.PutObjectTagging(ctx, "bucket", "k", nil); err != nil {
		t.Fatal(err)
	}
	requireNoTagFile(t, s, "k", "tags removed")

	put("deleted", testTags)
	if err = s.DeleteObject(ctx, "bucket", "deleted"); err != nil {
		t.Fatal(err)
	}
	requireNoTagFile(t, s, "deleted", "a delete")
}

// A copy takes its source's tags under COPY and its own otherwise, and a
// multipart upload's object the tags the upload was created with.
func TestTagsOfCopiesAndUploads(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	one := []contracts.Tag{{Key: "a", Value: ""}}
	putTagged(t, s, ctx, "src", testTags)
	for _, tt := range []struct {
		want []contracts.Tag
		in   contracts.CopyObjectInput
	}{
		{in: contracts.CopyObjectInput{CopyTags: true}, want: testTags},
		{in: contracts.CopyObjectInput{Tags: one}, want: one},
		{in: contracts.CopyObjectInput{}, want: nil},
	} {
		tt.in.SrcBucket, tt.in.SrcKey, tt.in.DstBucket, tt.in.DstKey = "bucket", "src",
			"bucket", "dst"
		if _, err := s.CopyObject(ctx, tt.in); err != nil {
			t.Fatal(err)
		}
		requireTags(t, s, ctx, "dst", tt.want, "a copy")
	}

	for _, tags := range [][]contracts.Tag{testTags, nil} {
		id, cErr := s.CreateMultipartUpload(ctx, "bucket", "src", contracts.PutObjectInput{
			Tags: tags,
		})
		if cErr != nil {
			t.Fatal(cErr)
		}
		etag, _, uErr := s.UploadPart(ctx, "bucket", "src", id, 1, strings.NewReader("x"))
		if uErr != nil {
			t.Fatal(uErr)
		}
		if _, err := s.CompleteMultipartUpload(ctx, "bucket", "src", id,
			[]contracts.CompletedPart{{PartNumber: 1, ETag: etag}},
			contracts.Precondition{}); err != nil {
			t.Fatal(err)
		}
		requireTags(t, s, ctx, "src", tags, "a completed upload")
	}
}

// Tags stored for an earlier object at the key, such as a crash between a
// write and its tags leaves, are not the object's.
func TestStaleTagsAreNotInherited(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	info, err := s.PutObject(ctx, "bucket", "k", strings.NewReader("x"),
		contracts.PutObjectInput{}, contracts.Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(objectTags{
		ETag: info.ETag, LastModified: info.LastModified.Add(-time.Second), Tags: testTags,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.writeFile(tagFile("bucket", "k"), raw); err != nil {
		t.Fatal(err)
	}
	requireTags(t, s, ctx, "k", nil, "a stale tag file")
}

// Deleting a bucket removes its objects' tags.
func TestABucketsTagsAreDeletedWithIt(t *testing.T) {
	s := newTestService(t)
	bg := context.Background()
	if _, err := s.CreateBucket(bg, "left", nil); err != nil {
		t.Fatal(err)
	}
	ctx, release, err := s.Pin(bg, ref(t, s, "left"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutObject(ctx, "left", "k", strings.NewReader("x"),
		contracts.PutObjectInput{Tags: testTags}, contracts.Precondition{}); err != nil {
		t.Fatal(err)
	}
	// The tag file is kept, as a delete interrupted before it could leave it.
	base, err := objectInput("left", "k")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.dir.DeleteObject(
		ctx,
		&storage.DeleteObjectInput{BaseObjectInput: base},
	); err != nil {
		t.Fatal(err)
	}
	if err = s.removeFromIndex("left", "k"); err != nil {
		t.Fatal(err)
	}
	release()
	if err = s.DeleteBucket(bg, ref(t, s, "left")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.root.Stat(tagFile("left", "k")); err == nil {
		t.Error("an object's tags outlived its bucket")
	}
}

// The copy source conditions follow RFC 7232's order, as S3 documents
// their pairs.
func TestCheckSourceCondition(t *testing.T) {
	modified := time.Date(2026, 1, 2, 3, 4, 5, 600, time.UTC)
	info := &contracts.ObjectInfo{ETag: "abc", LastModified: modified}
	before, at, after := modified.Add(-time.Hour), modified.Truncate(time.Second),
		modified.Add(time.Hour)
	for _, tt := range []struct {
		name string
		cond contracts.SourceCondition
		ok   bool
	}{
		{"none", contracts.SourceCondition{}, true},
		{"if-match", contracts.SourceCondition{IfMatch: "abc"}, true},
		{"if-match *", contracts.SourceCondition{IfMatch: "*"}, true},
		{"if-match other", contracts.SourceCondition{IfMatch: "def"}, false},
		{"if-unmodified-since after", contracts.SourceCondition{IfUnmodifiedSince: &after}, true},
		{"if-unmodified-since at", contracts.SourceCondition{IfUnmodifiedSince: &at}, true},
		{
			"if-unmodified-since before",
			contracts.SourceCondition{IfUnmodifiedSince: &before},
			false,
		},
		{
			"if-match true, if-unmodified-since false",
			contracts.SourceCondition{IfMatch: "abc", IfUnmodifiedSince: &before},
			true,
		},
		{"if-none-match other", contracts.SourceCondition{IfNoneMatch: "def"}, true},
		{"if-none-match", contracts.SourceCondition{IfNoneMatch: "abc"}, false},
		{"if-none-match *", contracts.SourceCondition{IfNoneMatch: "*"}, false},
		{"if-modified-since before", contracts.SourceCondition{IfModifiedSince: &before}, true},
		{"if-modified-since at", contracts.SourceCondition{IfModifiedSince: &at}, false},
		{
			"if-none-match false, if-modified-since true",
			contracts.SourceCondition{IfNoneMatch: "abc", IfModifiedSince: &before},
			false,
		},
		{
			"if-none-match true, if-modified-since false",
			contracts.SourceCondition{IfNoneMatch: "def", IfModifiedSince: &after},
			true,
		},
	} {
		err := checkSourceCondition(tt.cond, info)
		if (err == nil) != tt.ok {
			t.Errorf("%s: %v, want ok %t", tt.name, err, tt.ok)
		}
		if err != nil {
			requireIs(t, tt.name, err, contracts.ErrPreconditionFailed)
		}
	}
}

func TestCopySpan(t *testing.T) {
	const big = contracts.MinRangeCopySource + 1
	for _, tt := range []struct {
		rng          *contracts.CopyRange
		want         error
		name         string
		size         int64
		first, count int64
	}{
		{name: "whole", size: 10, count: 10},
		{name: "whole, empty", size: 0, count: 0},
		{
			name: "range", size: big, rng: &contracts.CopyRange{First: 5, Last: 9}, first: 5,
			count: 5,
		},
		{
			name: "last byte", size: big, rng: &contracts.CopyRange{First: big - 1, Last: big - 1},
			first: big - 1, count: 1,
		},
		{
			name: "past the end", size: big, rng: &contracts.CopyRange{First: 0, Last: big},
			want: contracts.ErrInvalidCopyRange,
		},
		{
			name: "small source", size: contracts.MinRangeCopySource,
			rng: &contracts.CopyRange{First: 0, Last: 0}, want: contracts.ErrRangeCopySourceTooSmall,
		},
		{
			name: "whole, too large", size: contracts.MaxCopyPartSize + 1,
			want: contracts.ErrEntityTooLarge,
		},
		{
			name: "range, too large", size: 6 << 30,
			rng:  &contracts.CopyRange{First: 0, Last: contracts.MaxCopyPartSize},
			want: contracts.ErrEntityTooLarge,
		},
		{
			name: "range, the largest", size: 6 << 30,
			rng:   &contracts.CopyRange{First: 1, Last: contracts.MaxCopyPartSize},
			first: 1, count: contracts.MaxCopyPartSize,
		},
	} {
		first, count, err := copySpan(tt.rng, tt.size)
		if tt.want != nil {
			requireIs(t, tt.name, err, tt.want)
			continue
		}
		if err != nil || first != tt.first || count != tt.count {
			t.Errorf("%s: %d %d %v, want %d %d", tt.name, first, count, err, tt.first, tt.count)
		}
	}
}

// copySource writes "src", larger than MinRangeCopySource, and starts an
// upload of "dst", returning src's bytes and info and the copy of the
// upload's part 1 from it.
func copySource(
	t *testing.T,
	s *Service,
	ctx context.Context, //nolint:revive // the test's pinned context, after the service
) ([]byte, *contracts.ObjectInfo, contracts.UploadPartCopyInput) {
	t.Helper()
	src := bytes.Repeat([]byte("0123456789"), contracts.MinRangeCopySource/10+1)
	srcInfo, err := s.PutObject(ctx, "bucket", "src", bytes.NewReader(src),
		contracts.PutObjectInput{}, contracts.Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateMultipartUpload(ctx, "bucket", "dst", contracts.PutObjectInput{
		Headers: map[string]string{"X-Amz-Server-Side-Encryption": "AES256"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return src, srcInfo, contracts.UploadPartCopyInput{
		Bucket: "bucket", Key: "dst", UploadID: id, SrcBucket: "bucket", SrcKey: "src",
		PartNumber: 1,
	}
}

// UploadPartCopy refuses a range past the source's end, a failed source
// condition, a missing source and a missing upload.
func TestUploadPartCopyErrors(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	src, srcInfo, in := copySource(t, s, ctx)
	in.Range = &contracts.CopyRange{First: 0, Last: int64(len(src))}
	_, _, err := s.UploadPartCopy(ctx, in)
	requireIs(t, "a range past the end", err, contracts.ErrInvalidCopyRange)
	in.Range, in.SourceCondition = nil, contracts.SourceCondition{IfNoneMatch: srcInfo.ETag}
	_, _, err = s.UploadPartCopy(ctx, in)
	requireIs(t, "a failed condition", err, contracts.ErrPreconditionFailed)
	in.SourceCondition, in.SrcKey = contracts.SourceCondition{}, "missing"
	_, _, err = s.UploadPartCopy(ctx, in)
	requireIs(t, "a missing source", err, storage.ErrObjectNotFound)
	in.SrcKey, in.UploadID = "src", "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	_, _, err = s.UploadPartCopy(ctx, in)
	requireIs(t, "a missing upload", err, contracts.ErrNoSuchUpload)
}

// UploadPartCopy streams the source, or its range, into the part, which a
// completion assembles as an uploaded one.
func TestUploadPartCopy(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	src, srcInfo, in := copySource(t, s, ctx)
	whole, headers, err := s.UploadPartCopy(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if whole.ETag != srcInfo.ETag || whole.Size != int64(len(src)) ||
		headers["X-Amz-Server-Side-Encryption"] != "AES256" || whole.LastModified.IsZero() {
		t.Fatalf("whole copy: %+v %v", whole, headers)
	}

	in.PartNumber, in.Range = 2, &contracts.CopyRange{First: 3, Last: 12}
	ranged, _, err := s.UploadPartCopy(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(src[3:13]) //nolint:gosec // see import
	if ranged.ETag != hex.EncodeToString(sum[:]) || ranged.Size != 10 {
		t.Fatalf("ranged copy: %+v", ranged)
	}

	requireCompleted(t, s, ctx, in.UploadID, []contracts.CompletedPart{
		{PartNumber: 1, ETag: whole.ETag}, {PartNumber: 2, ETag: ranged.ETag},
	}, append(slices.Clone(src), src[3:13]...))
}

// requireCompleted completes upload id of "dst" with parts and fails
// unless its object holds want, with a multipart ETag.
func requireCompleted(
	t *testing.T,
	s *Service,
	ctx context.Context, //nolint:revive // the test's pinned context, after the service
	id string,
	parts []contracts.CompletedPart,
	want []byte,
) {
	t.Helper()
	info, err := s.CompleteMultipartUpload(ctx, "bucket", "dst", id, parts,
		contracts.Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	got := readObject(t, s, ctx, "dst")
	if !bytes.Equal(got, want) || !strings.HasSuffix(info.ETag, "-"+strconv.Itoa(len(parts))) {
		t.Fatalf("completed object: %d bytes, ETag %s", len(got), info.ETag)
	}
}

// readObject returns the bytes of key in "bucket".
func readObject(
	t *testing.T,
	s *Service,
	ctx context.Context, //nolint:revive // the test's pinned context, after the service
	key string,
) []byte {
	t.Helper()
	obj, err := s.GetObject(ctx, "bucket", key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(obj.Body)
	if cErr := obj.Body.Close(); err != nil || cErr != nil {
		t.Fatal(err, cErr)
	}
	return got
}
