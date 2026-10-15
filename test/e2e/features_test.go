//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// copySource builds an x-amz-copy-source value, which the SDK expects the
// caller to URL-encode.
func copySource(bucket, key string) *string {
	segments := strings.Split(key, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return aws.String(bucket + "/" + strings.Join(segments, "/"))
}

func TestMetadata(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "meta")
	bucket, key := aws.String("meta"), aws.String("report.pdf")

	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket:             bucket,
		Key:                key,
		Body:               strings.NewReader("%PDF"),
		ContentType:        aws.String("application/pdf"),
		ContentDisposition: aws.String("attachment; filename=q3.pdf"),
		CacheControl:       aws.String("max-age=60"),
		Metadata:           map[string]string{"uploaded-by": "ahmed", "Mixed-Case": "v"},
	})
	requireNoErr(t, err, "put")

	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "head")
	requireEqual(t, aws.ToString(head.ContentType), "application/pdf", "content type")
	requireEqual(
		t,
		aws.ToString(head.ContentDisposition),
		"attachment; filename=q3.pdf",
		"disposition",
	)
	requireEqual(t, aws.ToString(head.CacheControl), "max-age=60", "cache control")
	requireEqual(t, head.Metadata["uploaded-by"], "ahmed", "user metadata")
	requireEqual(t, head.Metadata["mixed-case"], "v", "user metadata key is lowercased")
	requireEqual(t, len(head.Metadata), 2, "user metadata count")

	got, _ := get(t, c, &s3.GetObjectInput{
		Bucket:                     bucket,
		Key:                        key,
		ResponseContentType:        aws.String("text/plain"),
		ResponseContentDisposition: aws.String("inline"),
	})
	requireEqual(t, aws.ToString(got.ContentType), "text/plain", "overridden content type")
	requireEqual(t, aws.ToString(got.ContentDisposition), "inline", "overridden disposition")
	requireEqual(t, aws.ToString(got.CacheControl), "max-age=60", "get cache control")
	requireEqual(t, got.Metadata["uploaded-by"], "ahmed", "get user metadata")
}

func TestCopyObject(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "src")
	createBucket(t, c, "dst")

	srcKey := "dir/a b=c.txt"
	body := []byte("copy me")
	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String("src"),
		Key:         aws.String(srcKey),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("text/plain"),
		Metadata:    map[string]string{"origin": "src"},
	})
	requireNoErr(t, err, "put source")

	out, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String("dst"),
		Key:        aws.String("copied.txt"),
		CopySource: copySource("src", srcKey),
	})
	requireNoErr(t, err, "copy")
	requireEqual(t, aws.ToString(out.CopyObjectResult.ETag), md5Hex(body), "copy etag")

	got, gotBody := get(t, c, &s3.GetObjectInput{
		Bucket: aws.String("dst"), Key: aws.String("copied.txt"),
	})
	requireEqual(t, string(gotBody), string(body), "copied body")
	requireEqual(t, aws.ToString(got.ContentType), "text/plain", "copied content type")
	requireEqual(t, got.Metadata["origin"], "src", "copied metadata")

	// REPLACE onto itself is allowed and swaps the metadata.
	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            aws.String("dst"),
		Key:               aws.String("copied.txt"),
		CopySource:        copySource("dst", "copied.txt"),
		MetadataDirective: types.MetadataDirectiveReplace,
		ContentType:       aws.String("application/json"),
		Metadata:          map[string]string{"origin": "replaced"},
	})
	requireNoErr(t, err, "copy onto itself with REPLACE")

	got, gotBody = get(t, c, &s3.GetObjectInput{
		Bucket: aws.String("dst"), Key: aws.String("copied.txt"),
	})
	requireEqual(t, string(gotBody), string(body), "body after self-copy")
	requireEqual(t, aws.ToString(got.ContentType), "application/json", "replaced content type")
	requireEqual(t, got.Metadata["origin"], "replaced", "replaced metadata")

	requireCopyErrors(t, c, srcKey)
}

func requireCopyErrors(t *testing.T, c *s3.Client, srcKey string) {
	t.Helper()
	ctx := context.Background()

	_, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("dst"), Key: aws.String("copied.txt"),
		CopySource: copySource("dst", "copied.txt"),
	})
	requireCode(t, err, "InvalidRequest")

	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("dst"), Key: aws.String("x"),
		CopySource: copySource("src", "missing"),
	})
	requireCode(t, err, "NoSuchKey")

	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("no-such-bucket"), Key: aws.String("x"),
		CopySource: copySource("src", srcKey),
	})
	requireCode(t, err, "NoSuchBucket")
}

func TestDeleteObjects(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "batch")
	bucket := aws.String("batch")

	keys := []string{"a.txt", "b/c.txt", "d.txt"}
	for _, k := range keys {
		put(t, c, "batch", k, []byte(k))
	}

	ids := make([]types.ObjectIdentifier, 0, 3)
	ids = append(ids, types.ObjectIdentifier{Key: aws.String("never-existed")})
	for _, k := range keys[:2] {
		ids = append(ids, types.ObjectIdentifier{Key: aws.String(k)})
	}
	out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: bucket, Delete: &types.Delete{Objects: ids},
	})
	requireNoErr(t, err, "delete objects")
	requireEqual(t, len(out.Deleted), 3, "deleted count (missing keys count as deleted)")
	requireEqual(t, len(out.Errors), 0, "error count")

	out, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: bucket,
		Delete: &types.Delete{
			Objects: []types.ObjectIdentifier{{Key: aws.String("d.txt")}},
			Quiet:   aws.Bool(true),
		},
	})
	requireNoErr(t, err, "quiet delete objects")
	requireEqual(t, len(out.Deleted), 0, "quiet mode reports no deletions")

	remaining, _, _ := listAll(t, c, &s3.ListObjectsV2Input{Bucket: bucket})
	requireEqual(t, len(remaining), 0, "objects left")

	_, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("no-such-bucket"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("k")}}},
	})
	requireCode(t, err, "NoSuchBucket")
}

func do(t *testing.T, method, rawURL string, body io.Reader) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, rawURL, body)
	requireNoErr(t, err, "build request")
	res, err := http.DefaultClient.Do(req)
	requireNoErr(t, err, method+" "+rawURL)
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	requireNoErr(t, err, "read response")
	return res.StatusCode, b
}

func TestPresignedURLs(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "presign")
	presigner := s3.NewPresignClient(c)
	bucket, key := aws.String("presign"), aws.String("up/loaded.txt")

	putReq, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "presign put")
	status, _ := do(t, putReq.Method, putReq.URL, strings.NewReader("via presigned put"))
	requireEqual(t, status, http.StatusOK, "presigned put status")

	getReq, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "presign get")
	status, body := do(t, getReq.Method, getReq.URL, nil)
	requireEqual(t, status, http.StatusOK, "presigned get status")
	requireEqual(t, string(body), "via presigned put", "presigned get body")
}

func TestUnsupportedSubresources(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "guard")
	bucket, key := aws.String("guard"), aws.String("k")
	put(t, c, "guard", "k", []byte("original"))

	_, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: bucket,
		Key:    key,
		Tagging: &types.Tagging{
			TagSet: []types.Tag{{Key: aws.String("a"), Value: aws.String("b")}},
		},
	})
	requireCode(t, err, "NotImplemented")
	_, body := get(t, c, &s3.GetObjectInput{Bucket: bucket, Key: key})
	requireEqual(t, string(body), "original", "object untouched by tagging request")

	_, err = c.DeleteBucketCors(ctx, &s3.DeleteBucketCorsInput{Bucket: bucket})
	requireCode(t, err, "NotImplemented")
	_, err = c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: bucket})
	requireNoErr(t, err, "bucket survives delete-cors request")

	_, err = c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: bucket, Key: key})
	requireCode(t, err, "NotImplemented")
}
