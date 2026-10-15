//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // S3 ETags are MD5 digests
	"encoding/hex"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/gasmod/gas/config/providers"

	"tule.dev/object-store/internal/app"
)

// newClient starts the app in-process on a fresh storage root and returns an
// SDK client pointed at it.
func newClient(t *testing.T) *s3.Client {
	t.Helper()
	return startClient(t, false)
}

// startClient is newClient with the choice of serving over TLS. The SDK only
// streams unseekable bodies with aws-chunked trailing checksums over TLS.
func startClient(t *testing.T, useTLS bool) *s3.Client {
	t.Helper()
	return clientFor(startServer(t, useTLS, nil))
}

// startServer starts the app in-process on a fresh storage root, with env
// setting extra TULE_ variables. It accepts the test/test credentials
// clientFor signs with unless env changes them.
func startServer(t *testing.T, useTLS bool, env map[string]string) *httptest.Server {
	t.Helper()

	t.Setenv("TULE_GasEnv", "testing")
	t.Setenv("TULE_Storage_RootDir", t.TempDir())
	t.Setenv("TULE_Auth_AccessKeyID", "test")
	t.Setenv("TULE_Auth_SecretAccessKey", "test")
	for k, v := range env {
		t.Setenv(k, v)
	}

	cfg, err := app.LoadConfigFrom(providers.NewEnvProvider(
		providers.WithEnvSeparator("_"),
		providers.WithEnvPrefix("TULE_"),
	))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	if err = a.Start(); err != nil {
		t.Fatalf("start app: %v", err)
	}

	srv := httptest.NewUnstartedServer(a.Handler())
	if useTLS {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(func() {
		srv.Close()
		if sErr := a.Shutdown(); sErr != nil {
			t.Errorf("shutdown: %v", sErr)
		}
	})

	return srv
}

// clientFor returns a path-style SDK client for srv, with the test/test
// credentials unless opts change them.
func clientFor(srv *httptest.Server, opts ...func(*s3.Options)) *s3.Client {
	return s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		HTTPClient:   srv.Client(),
	}, opts...)
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want API error %s, got %v", code, err)
	}
	if apiErr.ErrorCode() != code {
		t.Fatalf("want error code %s, got %s (%v)", code, apiErr.ErrorCode(), err)
	}
}

// requireAs fails unless err matches the modeled SDK error type T.
func requireAs[T error](t *testing.T, err error) {
	t.Helper()
	var target T
	if !errors.As(err, &target) {
		t.Fatalf("want %T, got %v", target, err)
	}
}

func requireNoErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func requireEqual[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // see import
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func put(t *testing.T, c *s3.Client, bucket, key string, body []byte) *s3.PutObjectOutput {
	t.Helper()
	out, err := c.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
	requireNoErr(t, err, "put "+key)
	return out
}

// get fetches an object and reads its whole body.
func get(t *testing.T, c *s3.Client, in *s3.GetObjectInput) (*s3.GetObjectOutput, []byte) {
	t.Helper()
	out, err := c.GetObject(context.Background(), in)
	requireNoErr(t, err, "get "+aws.ToString(in.Key))
	body, err := io.ReadAll(out.Body)
	_ = out.Body.Close()
	requireNoErr(t, err, "read "+aws.ToString(in.Key))
	return out, body
}

func createBucket(t *testing.T, c *s3.Client, bucket string) {
	t.Helper()
	_, err := c.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	requireNoErr(t, err, "create bucket")
}

func TestBuckets(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	alpha := aws.String("alpha")

	createBucket(t, c, "alpha")

	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: alpha})
	requireAs[*types.BucketAlreadyOwnedByYou](t, err)

	_, err = c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("Bad_Name")})
	requireCode(t, err, "InvalidBucketName")

	list, err := c.ListBuckets(ctx, &s3.ListBucketsInput{})
	requireNoErr(t, err, "list buckets")
	requireEqual(t, len(list.Buckets), 1, "bucket count")
	requireEqual(t, aws.ToString(list.Buckets[0].Name), "alpha", "bucket name")
	requireEqual(t, list.Buckets[0].CreationDate != nil, true, "has creation date")

	_, err = c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: alpha})
	requireNoErr(t, err, "head bucket")
	_, err = c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("missing")})
	requireAs[*types.NotFound](t, err)

	put(t, c, "alpha", "nested/key.txt", []byte("x"))
	_, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: alpha})
	requireCode(t, err, "BucketNotEmpty")

	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: alpha, Key: aws.String("nested/key.txt"),
	})
	requireNoErr(t, err, "delete object")
	_, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: alpha})
	requireNoErr(t, err, "delete bucket")

	_, err = c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: alpha})
	requireAs[*types.NotFound](t, err)
	_, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: alpha})
	requireCode(t, err, "NoSuchBucket")
}

func TestObjects(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "objects")

	bucket := aws.String("objects")
	key := aws.String("dir/sub dir/file=+&@!.txt")
	body := []byte("hello, object store")

	out := put(t, c, "objects", *key, body)
	requireEqual(t, aws.ToString(out.ETag), md5Hex(body), "put etag")

	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "head")
	requireEqual(t, aws.ToInt64(head.ContentLength), int64(len(body)), "head length")
	requireEqual(t, aws.ToString(head.ETag), md5Hex(body), "head etag")
	requireEqual(t, head.LastModified != nil, true, "head has last-modified")

	got, gotBody := get(t, c, &s3.GetObjectInput{Bucket: bucket, Key: key})
	requireEqual(t, string(gotBody), string(body), "get body")
	requireEqual(t, aws.ToString(got.ETag), md5Hex(body), "get etag")

	_, gotBody = get(t, c, &s3.GetObjectInput{
		Bucket: bucket, Key: key, Range: aws.String("bytes=0-4"),
	})
	requireEqual(t, string(gotBody), "hello", "ranged get body")

	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: aws.String("missing")})
	requireAs[*types.NoSuchKey](t, err)
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String("missing")})
	requireAs[*types.NotFound](t, err)
	_, err = c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("no-such-bucket"), Key: aws.String("k"),
	})
	requireCode(t, err, "NoSuchBucket")

	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "delete")
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key})
	requireAs[*types.NotFound](t, err)
}

// TestStreamingUpload sends a body the SDK cannot seek, which it uploads with
// the aws-chunked encoding and a trailing checksum.
func TestStreamingUpload(t *testing.T) {
	c := startClient(t, true)
	createBucket(t, c, "stream")

	body := bytes.Repeat([]byte("0123456789abcdef"), 10_000)
	_, err := c.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:        aws.String("stream"),
		Key:           aws.String("big.bin"),
		Body:          io.MultiReader(bytes.NewReader(body)),
		ContentLength: aws.Int64(int64(len(body))),
		// Without an explicit checksum the SDK sends a plain unsigned body.
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
	})
	requireNoErr(t, err, "streaming put")

	got, gotBody := get(t, c, &s3.GetObjectInput{
		Bucket: aws.String("stream"), Key: aws.String("big.bin"),
	})
	requireEqual(t, bytes.Equal(gotBody, body), true, "body round-trips")
	requireEqual(t, aws.ToString(got.ETag), md5Hex(body), "etag")
}

// listAll pages through a listing and returns every key and common prefix.
func listAll(
	t *testing.T,
	c *s3.Client,
	in *s3.ListObjectsV2Input,
) (keys, prefixes []string, pages int) {
	t.Helper()
	p := s3.NewListObjectsV2Paginator(c, in)
	for p.HasMorePages() {
		page, err := p.NextPage(context.Background())
		requireNoErr(t, err, "list page")
		pages++
		for _, o := range page.Contents {
			k := aws.ToString(o.Key)
			requireEqual(t, aws.ToString(o.ETag), md5Hex([]byte(k)), "etag of "+k)
			keys = append(keys, k)
		}
		for _, cp := range page.CommonPrefixes {
			prefixes = append(prefixes, aws.ToString(cp.Prefix))
		}
	}
	return keys, prefixes, pages
}

func TestListObjectsV2(t *testing.T) {
	c := newClient(t)
	createBucket(t, c, "list")
	bucket := aws.String("list")

	for _, k := range []string{"a.txt", "b/1.txt", "b/2.txt", "b/c/3.txt", "d.txt", "e/4.txt"} {
		put(t, c, "list", k, []byte(k))
	}

	keys, prefixes, pages := listAll(t, c, &s3.ListObjectsV2Input{
		Bucket: bucket, Delimiter: aws.String("/"), MaxKeys: aws.Int32(2),
	})
	requireEqual(t, strings.Join(keys, ","), "a.txt,d.txt", "delimited keys")
	requireEqual(t, strings.Join(prefixes, ","), "b/,e/", "common prefixes")
	requireEqual(t, pages, 2, "pages")

	keys, prefixes, pages = listAll(t, c, &s3.ListObjectsV2Input{
		Bucket: bucket, Prefix: aws.String("b/"),
	})
	requireEqual(t, strings.Join(keys, ","), "b/1.txt,b/2.txt,b/c/3.txt", "prefixed keys")
	requireEqual(t, len(prefixes), 0, "prefixed common prefixes")
	requireEqual(t, pages, 1, "prefixed pages")

	_, err := c.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String("missing"),
	})
	requireCode(t, err, "NoSuchBucket")
}
