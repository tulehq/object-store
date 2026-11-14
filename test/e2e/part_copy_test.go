//go:build e2e

package e2e

import (
	"bytes"
	"crypto/md5" //nolint:gosec // S3 ETags are MD5 digests
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The tests here run the live server and check UploadPartCopy, and the
// x-amz-copy-source-if-* conditions it shares with CopyObject.

// copyEnv is a live server's client with the bucket "pcopy" holding "src",
// 6 MiB, and an upload of "dst" started.
type copyEnv struct {
	c  *s3.Client
	up *multipart
	// info is src's HeadObject.
	info *s3.HeadObjectOutput
	src  []byte
}

func startCopyEnv(t *testing.T) *copyEnv {
	t.Helper()
	l := startLive(t, nil)
	c := l.testClient()
	createBucket(t, c, "pcopy")
	src := randomBytes(t, 6*mib)
	put(t, c, "pcopy", "src", src)
	info, err := c.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String("pcopy"), Key: aws.String("src"),
	})
	requireNoErr(t, err, "head src")
	return &copyEnv{c: c, up: startUpload(t, c, "pcopy", "dst"), src: src, info: info}
}

// copyPart copies source, or rng of it, into part n of the upload, with
// edit applied to the input.
func (e *copyEnv) copyPart(
	t *testing.T,
	n int32,
	rng string,
	edit func(*s3.UploadPartCopyInput),
) (*s3.UploadPartCopyOutput, error) {
	t.Helper()
	in := &s3.UploadPartCopyInput{
		Bucket: e.up.bucket, Key: e.up.key, UploadId: e.up.id, PartNumber: aws.Int32(n),
		CopySource: copySource("pcopy", "src"),
	}
	if rng != "" {
		in.CopySourceRange = aws.String(rng)
	}
	if edit != nil {
		edit(in)
	}
	out, err := e.c.UploadPartCopy(t.Context(), in)
	return out, err //nolint:wrapcheck // the test inspects the SDK error
}

// A whole copy and ranged copies make parts a completion assembles, with
// S3's multipart ETag; each part's ETag is the MD5 of its bytes.
func TestUploadPartCopy(t *testing.T) {
	e := startCopyEnv(t)
	whole, err := e.copyPart(t, 1, "", nil)
	requireNoErr(t, err, "copy the whole source")
	requireEqual(t, aws.ToString(whole.CopyPartResult.ETag), md5Hex(e.src), "whole part ETag")
	requireEqual(t, whole.ServerSideEncryption, types.ServerSideEncryptionAes256, "encryption")
	if whole.CopyPartResult.LastModified == nil {
		t.Fatal("no LastModified")
	}

	first, err := e.copyPart(t, 2, "bytes=0-5242879", nil)
	requireNoErr(t, err, "copy the first 5 MiB")
	requireEqual(t, aws.ToString(first.CopyPartResult.ETag), md5Hex(e.src[:5*mib]), "range ETag")
	last, err := e.copyPart(t, 3, "bytes=6291455-6291455", nil)
	requireNoErr(t, err, "copy the last byte")

	out, err := e.up.complete(
		types.CompletedPart{PartNumber: aws.Int32(1), ETag: whole.CopyPartResult.ETag},
		types.CompletedPart{PartNumber: aws.Int32(2), ETag: first.CopyPartResult.ETag},
		types.CompletedPart{PartNumber: aws.Int32(3), ETag: last.CopyPartResult.ETag},
	)
	requireNoErr(t, err, "complete from copied parts")
	want := append(append(bytes.Clone(e.src), e.src[:5*mib]...), e.src[6*mib-1:]...)
	digests := make([]byte, 0, 3*md5.Size)
	for _, p := range [][]byte{e.src, e.src[:5*mib], e.src[6*mib-1:]} {
		digests = append(digests, md5Sum(p)...)
	}
	requireEqual(t, aws.ToString(out.ETag), multipartETagOf(digests, 3), "multipart ETag")
	_, body := get(t, e.c, &s3.GetObjectInput{Bucket: aws.String("pcopy"), Key: aws.String("dst")})
	requireEqual(t, bytes.Equal(body, want), true, "the completed object's bytes")
}

// A copied part smaller than 5 MiB may only be the last, as an uploaded
// one, and the source takes every x-amz-copy-source spelling CopyObject
// takes.
func TestUploadPartCopyPartSizes(t *testing.T) {
	e := startCopyEnv(t)
	small, err := e.copyPart(t, 1, "bytes=0-99", func(in *s3.UploadPartCopyInput) {
		in.CopySource = aws.String("/pcopy/src")
	})
	requireNoErr(t, err, "copy a slash-led source")
	whole, err := e.copyPart(t, 2, "", func(in *s3.UploadPartCopyInput) {
		in.CopySource = aws.String("pcopy/%73rc")
	})
	requireNoErr(t, err, "copy a percent-encoded source")
	_, err = e.up.complete(
		types.CompletedPart{PartNumber: aws.Int32(1), ETag: small.CopyPartResult.ETag},
		types.CompletedPart{PartNumber: aws.Int32(2), ETag: whole.CopyPartResult.ETag},
	)
	requireCode(t, err, "EntityTooSmall")
}

// Each x-amz-copy-source-if-* condition, holding and not, on UploadPartCopy
// and CopyObject: a failed one is 412 PreconditionFailed.
func TestCopySourceConditions(t *testing.T) {
	e := startCopyEnv(t)
	etag := aws.ToString(e.info.ETag)
	before := e.info.LastModified.Add(-time.Hour)
	after := e.info.LastModified.Add(time.Hour)
	type cond func(match, noneMatch **string, modified, unmodified **time.Time)
	for _, tt := range []struct {
		set  cond
		name string
		ok   bool
	}{
		{name: "if-match", ok: true, set: func(m, _ **string, _, _ **time.Time) { *m = &etag }},
		{name: "if-match other", set: func(m, _ **string, _, _ **time.Time) {
			*m = aws.String(`"x"`)
		}},
		{name: "if-none-match", set: func(_, n **string, _, _ **time.Time) { *n = &etag }},
		{name: "if-none-match other", ok: true, set: func(_, n **string, _, _ **time.Time) {
			*n = aws.String(`"x"`)
		}},
		{name: "if-modified-since before", ok: true, set: func(_, _ **string, m, _ **time.Time) {
			*m = &before
		}},
		{name: "if-modified-since after", set: func(_, _ **string, m, _ **time.Time) {
			*m = &after
		}},
		{name: "if-unmodified-since after", ok: true, set: func(_, _ **string, _, u **time.Time) {
			*u = &after
		}},
		{name: "if-unmodified-since before", set: func(_, _ **string, _, u **time.Time) {
			*u = &before
		}},
		{
			name: "if-match true, if-unmodified-since false", ok: true,
			set: func(m, _ **string, _, u **time.Time) { *m, *u = &etag, &before },
		},
		{
			name: "if-none-match false, if-modified-since true",
			set:  func(_, n **string, m, _ **time.Time) { *n, *m = &etag, &before },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.copyPart(t, 1, "", func(in *s3.UploadPartCopyInput) {
				tt.set(&in.CopySourceIfMatch, &in.CopySourceIfNoneMatch,
					&in.CopySourceIfModifiedSince, &in.CopySourceIfUnmodifiedSince)
			})
			requireCondition(t, err, tt.ok, "UploadPartCopy")
			in := &s3.CopyObjectInput{
				Bucket: aws.String(
					"pcopy",
				),
				Key:        aws.String("copy"),
				CopySource: copySource("pcopy", "src"),
			}
			tt.set(&in.CopySourceIfMatch, &in.CopySourceIfNoneMatch,
				&in.CopySourceIfModifiedSince, &in.CopySourceIfUnmodifiedSince)
			_, err = e.c.CopyObject(t.Context(), in)
			requireCondition(t, err, tt.ok, "CopyObject")
		})
	}
}

// requireCondition fails unless err is nil when ok, and 412
// PreconditionFailed otherwise.
func requireCondition(t *testing.T, err error, ok bool, what string) {
	t.Helper()
	if ok {
		requireNoErr(t, err, what)
		return
	}
	requireCode(t, err, "PreconditionFailed")
	requireResponse(t, err, http.StatusPreconditionFailed)
}

// Ranges that are malformed, end past the source, or select from a source
// of at most 5 MB are refused, as are missing sources and uploads.
func TestUploadPartCopyErrors(t *testing.T) {
	e := startCopyEnv(t)
	put(t, e.c, "pcopy", "small", bytes.Repeat([]byte("s"), 5*mib))
	for _, rng := range []string{"0-9", "bytes=9-0", "bytes=0-", "bytes=-9", "bytes=a-b"} {
		_, err := e.copyPart(t, 1, rng, nil)
		requireCode(t, err, "InvalidArgument")
	}
	_, err := e.copyPart(t, 1, "bytes=0-6291456", nil)
	requireCode(t, err, "InvalidArgument")
	_, err = e.copyPart(t, 1, "bytes=0-9", func(in *s3.UploadPartCopyInput) {
		in.CopySource = copySource("pcopy", "small")
	})
	requireCode(t, err, "InvalidRequest")
	_, err = e.copyPart(t, 1, "", func(in *s3.UploadPartCopyInput) {
		in.CopySource = copySource("pcopy", "small")
	})
	requireNoErr(t, err, "a whole copy of a small source")

	_, err = e.copyPart(t, 1, "", func(in *s3.UploadPartCopyInput) {
		in.CopySource = copySource("pcopy", "missing")
	})
	requireCode(t, err, "NoSuchKey")
	_, err = e.copyPart(t, 1, "", func(in *s3.UploadPartCopyInput) {
		in.UploadId = aws.String("AAAAAAAAAAAAAAAAAAAAAAAAAA")
	})
	requireCode(t, err, "NoSuchUpload")
	_, err = e.copyPart(t, 1, "", func(in *s3.UploadPartCopyInput) {
		in.CopySource = aws.String("pcopy")
	})
	requireCode(t, err, "InvalidArgument")
}

func md5Sum(b []byte) []byte {
	sum := md5.Sum(b) //nolint:gosec // see import
	return sum[:]
}

// multipartETagOf is S3's ETag of a multipart object whose parts have
// the binary MD5s digests: the hex MD5 of their concatenation, "-", and
// the part count, quoted.
func multipartETagOf(digests []byte, parts int) string {
	sum := md5.Sum(digests) //nolint:gosec // see import
	return `"` + hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(parts) + `"`
}

// UploadPartCopy needs s3:GetObject on its source: without it, and from
// another bucket the caller cannot read, the copy is refused with
// AccessDenied naming the source.
func TestUploadPartCopyAuthorization(t *testing.T) {
	identity := startSibling(t, "identity", identityDirEnv)
	l := startLiveWithIdentity(t, identity.URL)
	ic := iamClient(identity.URL)
	root := l.client(rootCreds())
	for _, b := range []string{"pcopy", "other"} {
		createBucket(t, root, b)
		put(t, root, b, "src", []byte("src"))
	}
	dev := l.client(createUserKey(t, ic, "dev"))
	putUserPolicy(t, ic, "dev", "p0", allowDoc(`["s3:PutObject"]`, "arn:aws:s3:::pcopy/*"))
	up := startUpload(t, dev, "pcopy", "dst")
	copyFrom := func(bucket string) error {
		_, err := dev.UploadPartCopy(t.Context(), &s3.UploadPartCopyInput{
			Bucket: up.bucket, Key: up.key, UploadId: up.id, PartNumber: aws.Int32(1),
			CopySource: copySource(bucket, "src"),
		})
		return err //nolint:wrapcheck // the test inspects the SDK error
	}

	requireDenied(t, copyFrom("pcopy"),
		notAllowed(devARN, "s3:GetObject", "arn:aws:s3:::pcopy/src"))

	putUserPolicy(t, ic, "dev", "p1", allowDoc(`["s3:GetObject"]`, "arn:aws:s3:::pcopy/*"))
	requireNoErr(t, copyFrom("pcopy"), "copy from a source dev may read")
	requireDenied(t, copyFrom("other"),
		notAllowed(devARN, "s3:GetObject", "arn:aws:s3:::other/src"))
}
