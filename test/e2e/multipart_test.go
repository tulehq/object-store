//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // S3 ETags are MD5 digests
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const mib = 1 << 20

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	requireNoErr(t, err, "random bytes")
	return b
}

// multipartETag computes the S3 ETag of an object uploaded as parts of
// partSize bytes.
func multipartETag(body []byte, partSize int) string {
	var digests []byte
	parts := 0
	for off := 0; off < len(body); off += partSize {
		sum := md5.Sum(body[off:min(off+partSize, len(body))]) //nolint:gosec // see import
		digests = append(digests, sum[:]...)
		parts++
	}
	sum := md5.Sum(digests) //nolint:gosec // see import
	return fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(sum[:]), parts)
}

func TestTransferManager(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "tm-bucket")
	bucket, key := aws.String("tm-bucket"), aws.String("big/file.bin")

	tm := transfermanager.New(c, func(o *transfermanager.Options) {
		o.PartSizeBytes = 5 * mib
		o.MultipartUploadThreshold = 5 * mib
	})

	body := randomBytes(t, 13*mib)
	_, err := tm.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: bucket, Key: key, Body: bytes.NewReader(body),
		ContentType: aws.String("application/x-test"),
	})
	requireNoErr(t, err, "multipart upload")

	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "head")
	requireEqual(t, aws.ToString(head.ETag), multipartETag(body, 5*mib), "multipart etag")
	requireEqual(t, aws.ToInt64(head.ContentLength), int64(len(body)), "size")
	requireEqual(t, aws.ToString(head.ContentType), "application/x-test", "content type")

	out, err := tm.GetObject(ctx, &transfermanager.GetObjectInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "transfer manager get")
	got, err := io.ReadAll(out.Body)
	requireNoErr(t, err, "read transfer manager get")
	requireEqual(t, bytes.Equal(got, body), true, "multipart body round-trips")

	part, partBody := get(
		t,
		c,
		&s3.GetObjectInput{Bucket: bucket, Key: key, PartNumber: aws.Int32(2)},
	)
	requireEqual(t, aws.ToInt32(part.PartsCount), 3, "parts count")
	requireEqual(t, bytes.Equal(partBody, body[5*mib:10*mib]), true, "part 2 bytes")

	headPart, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: bucket, Key: key, PartNumber: aws.Int32(3),
	})
	requireNoErr(t, err, "head part")
	requireEqual(t, aws.ToInt64(headPart.ContentLength), int64(3*mib), "part 3 length")

	_, err = c.GetObject(
		ctx,
		&s3.GetObjectInput{Bucket: bucket, Key: key, PartNumber: aws.Int32(4)},
	)
	requireCode(t, err, "InvalidPartNumber")

	// A plain object downloads through the part-based path as one part.
	put(t, c, "tm-bucket", "small.txt", []byte("small"))
	out, err = tm.GetObject(
		ctx,
		&transfermanager.GetObjectInput{Bucket: bucket, Key: aws.String("small.txt")},
	)
	requireNoErr(t, err, "transfer manager get of plain object")
	got, err = io.ReadAll(out.Body)
	requireNoErr(t, err, "read plain object")
	requireEqual(t, string(got), "small", "plain object body")
}

// multipart drives the low-level multipart API for one upload.
type multipart struct {
	t      *testing.T
	c      *s3.Client
	bucket *string
	key    *string
	id     *string
}

func startUpload(t *testing.T, c *s3.Client, bucket, key string) *multipart {
	t.Helper()
	out, err := c.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		ContentType: aws.String("text/csv"),
		Metadata:    map[string]string{"batch": "7"},
	})
	requireNoErr(t, err, "create multipart upload")
	return &multipart{
		t:      t,
		c:      c,
		bucket: aws.String(bucket),
		key:    aws.String(key),
		id:     out.UploadId,
	}
}

func (m *multipart) part(n int32, body []byte) types.CompletedPart {
	m.t.Helper()
	out, err := m.c.UploadPart(context.Background(), &s3.UploadPartInput{
		Bucket: m.bucket, Key: m.key, UploadId: m.id,
		PartNumber: aws.Int32(n), Body: bytes.NewReader(body),
	})
	requireNoErr(m.t, err, fmt.Sprintf("upload part %d", n))
	requireEqual(m.t, aws.ToString(out.ETag), md5Hex(body), "part etag")
	return types.CompletedPart{PartNumber: aws.Int32(n), ETag: out.ETag}
}

func (m *multipart) complete(
	parts ...types.CompletedPart,
) (*s3.CompleteMultipartUploadOutput, error) {
	out, err := m.c.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
		Bucket: m.bucket, Key: m.key, UploadId: m.id,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return nil, fmt.Errorf("complete: %w", err)
	}
	return out, nil
}

func (m *multipart) abort() error {
	_, err := m.c.AbortMultipartUpload(context.Background(), &s3.AbortMultipartUploadInput{
		Bucket: m.bucket, Key: m.key, UploadId: m.id,
	})
	if err != nil {
		return fmt.Errorf("abort: %w", err)
	}
	return nil
}

func TestMultipartValidation(t *testing.T) {
	c := newClient(t)
	createBucket(t, c, "mp-bucket")

	up := startUpload(t, c, "mp-bucket", "data.csv")
	p1 := up.part(1, []byte("a,b\n"))
	p2 := up.part(2, []byte("1,2\n"))

	_, err := up.complete(p1, p2)
	requireCode(t, err, "EntityTooSmall")

	_, err = up.complete(p2, p1)
	requireCode(t, err, "InvalidPartOrder")

	_, err = up.complete(types.CompletedPart{PartNumber: aws.Int32(1), ETag: p2.ETag})
	requireCode(t, err, "InvalidPart")

	_, err = up.complete(types.CompletedPart{PartNumber: aws.Int32(3), ETag: p1.ETag})
	requireCode(t, err, "InvalidPart")

	// A single part may be any size, since it is the last one.
	out, err := up.complete(p1)
	requireNoErr(t, err, "complete")
	requireEqual(t, aws.ToString(out.ETag), multipartETag([]byte("a,b\n"), 4), "completed etag")

	got, body := get(t, c, &s3.GetObjectInput{Bucket: up.bucket, Key: up.key})
	requireEqual(t, string(body), "a,b\n", "completed body")
	requireEqual(t, aws.ToString(got.ContentType), "text/csv", "content type from create")
	requireEqual(t, got.Metadata["batch"], "7", "metadata from create")

	_, err = c.UploadPart(context.Background(), &s3.UploadPartInput{
		Bucket: up.bucket, Key: up.key, UploadId: up.id,
		PartNumber: aws.Int32(1), Body: bytes.NewReader([]byte("x")),
	})
	requireCode(t, err, "NoSuchUpload")
	requireCode(t, up.abort(), "NoSuchUpload")
}

func TestMultipartAbort(t *testing.T) {
	c := newClient(t)
	createBucket(t, c, "mp-bucket")

	up := startUpload(t, c, "mp-bucket", "gone.csv")
	p1 := up.part(1, []byte("x"))

	other := &multipart{t: t, c: c, bucket: up.bucket, key: aws.String("other.csv"), id: up.id}
	_, err := other.complete(p1)
	requireCode(t, err, "NoSuchUpload")

	requireNoErr(t, up.abort(), "abort")
	_, err = up.complete(p1)
	requireCode(t, err, "NoSuchUpload")

	_, err = c.HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: up.bucket, Key: up.key})
	requireAs[*types.NotFound](t, err)

	_, err = c.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String("no-such-bucket"), Key: aws.String("k"),
	})
	requireCode(t, err, "NoSuchBucket")

	// The staging area never shows up as a bucket.
	list, err := c.ListBuckets(context.Background(), &s3.ListBucketsInput{})
	requireNoErr(t, err, "list buckets")
	requireEqual(t, len(list.Buckets), 1, "bucket count")
}
