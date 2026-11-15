package dirimpl

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // S3 ETags are MD5 digests, not a security control
	"encoding/hex"
	"encoding/json"
	"errors"
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
	got, err := s.GetObjectTagging(ctx, oref("bucket", key))
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("%s: tags %v, %v; want %v", what, got, err, want)
	}
	obj, err := s.GetObject(ctx, oref("bucket", key))
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

	_, err := s.GetObjectTagging(ctx, oref("bucket", "k"))
	requireIs(t, "no object", err, storage.ErrObjectNotFound)
	requireIs(
		t,
		"put on no object",
		errOf(s.PutObjectTagging(ctx, oref("bucket", "k"), testTags, nil)),
		storage.ErrObjectNotFound,
	)

	put("k", testTags)
	requireTags(t, s, ctx, "k", testTags, "written with tags")
	// Read without a pin, under the bucket's lock, as authorization reads
	// them.
	if got, gErr := s.GetObjectTagging(context.Background(), oref("bucket", "k")); gErr != nil ||
		!slices.Equal(got, testTags) {
		t.Fatalf("unpinned: %v %v", got, gErr)
	}
	put("k", nil)
	requireTags(t, s, ctx, "k", nil, "written over without tags")
	requireNoTagFile(t, s, "k", "written over without tags")

	one := []contracts.Tag{{Key: "a", Value: ""}}
	if err = errOf(s.PutObjectTagging(ctx, oref("bucket", "k"), one, nil)); err != nil {
		t.Fatal(err)
	}
	requireTags(t, s, ctx, "k", one, "PutObjectTagging")
	if err = errOf(s.PutObjectTagging(ctx, oref("bucket", "k"), nil, nil)); err != nil {
		t.Fatal(err)
	}
	requireTags(t, s, ctx, "k", nil, "tags removed")

	// Tags written with the object are in its trailer, not a tag file, and
	// a removal of them is stored, so they do not apply again.
	put("k", testTags)
	requireNoTagFile(t, s, "k", "written with tags")
	if err = errOf(s.PutObjectTagging(ctx, oref("bucket", "k"), nil, nil)); err != nil {
		t.Fatal(err)
	}
	requireTags(t, s, ctx, "k", nil, "tags written with the object removed")
	if err = errOf(s.PutObjectTagging(ctx, oref("bucket", "k"), one, nil)); err != nil {
		t.Fatal(err)
	}
	requireTags(t, s, ctx, "k", one, "tags written with the object replaced")
	put("k", nil)
	requireTags(t, s, ctx, "k", nil, "replaced tags written over")
	requireNoTagFile(t, s, "k", "replaced tags written over")

	put("deleted", testTags)
	if err = errOf(s.DeleteObject(ctx, oref("bucket", "deleted"))); err != nil {
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

// An object is visible with the tags it was written with from the moment
// its write commits: they are read from the object itself, before the
// write removes the tag file of the object it replaced, which holds other
// tags. The object is read between the two steps of PutObject: the commit
// (putObject) and the removal (dropTagFile).
func TestAWrittenObjectIsVisibleWithItsTags(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	putTagged(t, s, ctx, "k", nil)
	if _, err := s.PutObjectTagging(ctx, oref("bucket", "k"),
		[]contracts.Tag{{Key: "old", Value: "1"}}, nil); err != nil {
		t.Fatal(err)
	}
	unlock := s.locks.lock("bucket", "k")
	er := newETagReader(strings.NewReader("y"), nil)
	if _, err := s.putObject(ctx, "bucket", "k", er,
		&trailer{Meta: er.meta, Tags: testTags}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.root.Stat(tagFile("bucket", "k")); err != nil {
		t.Fatalf("the earlier object's tag file is not left to read past: %v", err)
	}
	requireTags(t, s, ctx, "k", testTags, "just after the write commits")
	unlock()
}

// An object written before tags were stored with it has the tags its tag
// file holds for it, as v0.10.0 stored them.
func TestTagFilesOfEarlierWritesStillApply(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	er := newETagReader(strings.NewReader("x"), nil)
	info, err := s.putObject(ctx, "bucket", "k", er, &trailer{Meta: er.meta})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(objectTags{
		ETag: info.ETag, LastModified: info.LastModified, Tags: testTags,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.writeFile(tagFile("bucket", "k"), raw); err != nil {
		t.Fatal(err)
	}
	requireTags(t, s, ctx, "k", testTags, "an object stored with a v0.10.0 tag file")
	got, err := s.StatObjectTagging(context.Background(), oref("bucket", "k"))
	if err != nil || got.ETag != info.ETag || !slices.Equal(got.Tags, testTags) {
		t.Fatalf("StatObjectTagging: %+v, %v", got, err)
	}
}

// errStop is what a test's check fails with.
var errStop = errors.New("stop")

// checkRecorder is an operation's check that records the object it is
// given, and fails with errStop unless pass is set.
type checkRecorder struct {
	seen *contracts.ObjectInfo
	pass bool
}

func (c *checkRecorder) check(info *contracts.ObjectInfo) error {
	c.seen = info
	if c.pass {
		return nil
	}
	return errStop
}

// requireSeen fails unless the check was given "src", written with
// testTags, with its tags, and resets it.
func (c *checkRecorder) requireSeen(t *testing.T, what string) {
	t.Helper()
	if c.seen == nil || c.seen.Key != "src" || !slices.Equal(c.seen.Tags, testTags) ||
		c.seen.ETag == "" {
		t.Fatalf("%s: the check saw %+v", what, c.seen)
	}
	c.seen = nil
}

// A copy's check is given the source it opens, with its tags, and an
// error it returns fails the copy before it writes anything.
func TestACopysCheckSeesTheSourceOpened(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	putTagged(t, s, ctx, "src", testTags)
	rec := &checkRecorder{}
	in := contracts.CopyObjectInput{
		SrcBucket: "bucket", SrcKey: "src", DstBucket: "bucket", DstKey: "dst",
		CopyTags: true, SourceCheck: rec.check,
	}
	_, err := s.CopyObject(ctx, in)
	requireIs(t, "a stopped copy", err, errStop)
	rec.requireSeen(t, "CopyObject")
	_, err = s.StatObject(ctx, oref("bucket", "dst"))
	requireIs(t, "the stopped copy's destination", err, storage.ErrObjectNotFound)
	rec.pass = true
	if _, err = s.CopyObject(ctx, in); err != nil {
		t.Fatal(err)
	}
	rec.requireSeen(t, "a passed CopyObject")
	requireTags(t, s, ctx, "dst", testTags, "a passed copy")

	_, _, part := copySource(t, s, ctx)
	putTagged(t, s, ctx, "src", testTags)
	rec.pass, part.SourceCheck = false, rec.check
	_, err = s.UploadPartCopy(ctx, part)
	requireIs(t, "a stopped part copy", err, errStop)
	rec.requireSeen(t, "UploadPartCopy")
	parts, err := s.ListParts(ctx, "bucket", "dst", part.UploadID)
	if err != nil || len(parts) != 0 {
		t.Fatalf("the stopped part copy stored %v, %v", parts, err)
	}
}

// The checks of PutObjectTagging, PutObjectACL and GetObjectACL are given
// the object they act on, with its tags, and an error one returns fails
// the operation before it writes anything.
func TestTagAndACLChecksSeeTheObjectOpened(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	putTagged(t, s, ctx, "src", testTags)
	rec := &checkRecorder{}
	requireIs(t, "a stopped PutObjectTagging",
		errOf(s.PutObjectTagging(ctx, oref("bucket", "src"), nil, rec.check)), errStop)
	rec.requireSeen(t, "PutObjectTagging")
	requireTags(t, s, ctx, "src", testTags, "after a stopped PutObjectTagging")
	requireIs(t, "a stopped PutObjectACL",
		errOf(s.PutObjectACL(ctx, oref("bucket", "src"), []byte("<a/>"), rec.check)), errStop)
	rec.requireSeen(t, "PutObjectACL")
	_, _, err := s.GetObjectACL(ctx, oref("bucket", "src"), rec.check)
	requireIs(t, "a stopped GetObjectACL", err, errStop)
	rec.requireSeen(t, "GetObjectACL")
	_, _, err = s.GetObjectACL(ctx, oref("bucket", "src"), nil)
	requireIs(t, "the ACL after a stopped PutObjectACL", err, contracts.ErrNoSuchBucketConfig)
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
	_, err := s.UploadPartCopy(ctx, in)
	requireIs(t, "a range past the end", err, contracts.ErrInvalidCopyRange)
	in.Range, in.SourceCondition = nil, contracts.SourceCondition{IfNoneMatch: srcInfo.ETag}
	_, err = s.UploadPartCopy(ctx, in)
	requireIs(t, "a failed condition", err, contracts.ErrPreconditionFailed)
	in.SourceCondition, in.SrcKey = contracts.SourceCondition{}, "missing"
	_, err = s.UploadPartCopy(ctx, in)
	requireIs(t, "a missing source", err, storage.ErrObjectNotFound)
	in.SrcKey, in.UploadID = "src", "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	_, err = s.UploadPartCopy(ctx, in)
	requireIs(t, "a missing upload", err, contracts.ErrNoSuchUpload)
}

// UploadPartCopy streams the source, or its range, into the part, which a
// completion assembles as an uploaded one.
func TestUploadPartCopy(t *testing.T) {
	s := newTestService(t)
	ctx, _ := pin(t, s)
	src, srcInfo, in := copySource(t, s, ctx)
	out, err := s.UploadPartCopy(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	whole, headers := out.Part, out.Headers
	if whole.ETag != srcInfo.ETag || whole.Size != int64(len(src)) ||
		headers["X-Amz-Server-Side-Encryption"] != "AES256" || whole.LastModified.IsZero() {
		t.Fatalf("whole copy: %+v %v", whole, headers)
	}

	in.PartNumber, in.Range = 2, &contracts.CopyRange{First: 3, Last: 12}
	out, err = s.UploadPartCopy(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	ranged := out.Part
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
	obj, err := s.GetObject(ctx, oref("bucket", key))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(obj.Body)
	if cErr := obj.Body.Close(); err != nil || cErr != nil {
		t.Fatal(err, cErr)
	}
	return got
}

// requireVersionTags fails unless version id of "k" has tags want.
func requireVersionTags(t *testing.T, f *fixture, id string, want []contracts.Tag) {
	t.Helper()
	got, err := f.s.GetObjectTagging(f.ctx, vref("k", id))
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("tags of %s: %v %v, want %v", id, got, err, want)
	}
}

// A legacy object keeps its tags and ACL, in its keyHash files, as the
// null version its key's first versioned write makes it.
func TestALegacyNullKeepsItsTagsAndACL(t *testing.T) {
	f := newFixture(t)
	f.putIn("k", "legacy", contracts.PutObjectInput{ACL: []byte("<legacy/>")})
	one := []contracts.Tag{{Key: "a", Value: "1"}}
	if err := errOf(f.s.PutObjectTagging(f.ctx, oref("bucket", "k"), one, nil)); err != nil {
		t.Fatal(err)
	}
	f.state(contracts.VersioningEnabled)
	v1 := f.putIn("k", "v1", contracts.PutObjectInput{Tags: testTags, ACL: []byte("<v1/>")})

	requireVersionTags(t, f, contracts.VersionNull, one)
	requireVersionTags(t, f, v1.VersionID, testTags)
	if doc, id, err := f.s.GetObjectACL(f.ctx, vref("k", contracts.VersionNull), nil); err != nil ||
		string(doc) != "<legacy/>" || id != contracts.VersionNull {
		t.Fatalf("null ACL: %q %q %v", doc, id, err)
	}
	if !f.exists(tagFile("bucket", "k")) || !f.exists(aclFile("bucket", "k")) {
		t.Fatal("the legacy null version's side files moved")
	}
	e := f.versions("k")[0]
	if !f.exists(sideFile(aclDir, "bucket", "k", &e)) {
		t.Fatal("the new version's ACL is not in its token's file")
	}
}

// Tags are stored per version: PutObjectTagging of one changes none of
// the others, and is stored in that version's file, and a delete marker
// has none.
func TestTagsArePerVersion(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	v1 := f.putIn("k", "v1", contracts.PutObjectInput{Tags: testTags})
	v2 := f.put("k", "v2")
	one := []contracts.Tag{{Key: "a", Value: "1"}}
	id, err := f.s.PutObjectTagging(f.ctx, vref("k", v1.VersionID), one, nil)
	if err != nil || id != v1.VersionID {
		t.Fatalf("tag v1: %q %v", id, err)
	}
	requireVersionTags(t, f, v1.VersionID, one)
	requireVersionTags(t, f, v2.VersionID, nil)
	if id, err = f.s.PutObjectTagging(f.ctx, oref("bucket", "k"), testTags, nil); err != nil ||
		id != v2.VersionID {
		t.Fatalf("tag the current version: %q %v", id, err)
	}
	requireVersionTags(t, f, v1.VersionID, one)
	e := f.versions("k")[1]
	if !f.exists(sideFile(tagDir, "bucket", "k", &e)) || f.exists(tagFile("bucket", "k")) {
		t.Fatal("v1's tags are not in its token's file")
	}

	marker := f.del(oref("bucket", "k"))
	_, err = f.s.PutObjectTagging(f.ctx, vref("k", marker.VersionID), one, nil)
	requireIs(t, "tag a marker", err, contracts.ErrMethodNotAllowed)
	_, err = f.s.GetObjectTagging(f.ctx, vref("k", marker.VersionID))
	requireIs(t, "tags of a marker", err, contracts.ErrMethodNotAllowed)
	_, err = f.s.PutObjectTagging(f.ctx, oref("bucket", "k"), one, nil)
	requireIs(t, "tag a current marker", err, storage.ErrObjectNotFound)
	_, err = f.s.PutObjectTagging(f.ctx, vref("k", newVersionID(1)), one, nil)
	requireIs(t, "tag a missing version", err, storage.ErrVersionNotFound)
}

// A suspended write of the null version does not inherit the tags or ACL
// of the one it replaces, whose files go with it.
func TestASuspendedOverwriteInheritsNothing(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	f.put("k", "v1")
	f.state(contracts.VersioningSuspended)
	f.putIn("k", "null", contracts.PutObjectInput{ACL: []byte("<old/>")})
	if err := errOf(f.s.PutObjectTagging(f.ctx, oref("bucket", "k"), testTags, nil)); err != nil {
		t.Fatal(err)
	}
	old := f.versions("k")[0]
	f.put("k", "new")

	requireVersionTags(t, f, contracts.VersionNull, nil)
	_, _, err := f.s.GetObjectACL(f.ctx, vref("k", contracts.VersionNull), nil)
	requireIs(t, "the new null version's ACL", err, contracts.ErrNoSuchBucketConfig)
	if f.exists(sideFile(tagDir, "bucket", "k", &old)) ||
		f.exists(sideFile(aclDir, "bucket", "k", &old)) || f.hasData(&old) {
		t.Fatal("the replaced null version's files are left")
	}
}
