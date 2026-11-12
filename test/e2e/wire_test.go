//go:build e2e

package e2e

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// editRequest returns a per-call option that edits the SDK's request before
// it is signed.
func editRequest(edit func(*smithyhttp.Request)) func(*s3.Options) {
	return func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, func(st *middleware.Stack) error {
			return st.Build.Add(middleware.BuildMiddlewareFunc("editRequest",
				func(ctx context.Context, in middleware.BuildInput, next middleware.BuildHandler) (
					middleware.BuildOutput, middleware.Metadata, error,
				) {
					if req, ok := in.Request.(*smithyhttp.Request); ok {
						edit(req)
					}
					return next.HandleBuild(ctx, in)
				}), middleware.After)
		})
	}
}

// requireResponse fails unless err carries an HTTP response with status,
// and returns that response's header.
func requireResponse(t *testing.T, err error, status int) http.Header {
	t.Helper()
	var respErr *awshttp.ResponseError
	if !errors.As(err, &respErr) {
		t.Fatalf("want an HTTP response error, got %v", err)
	}
	requireEqual(t, respErr.HTTPStatusCode(), status, "status")
	return respErr.Response.Header
}

// TestMethodNotAllowed checks that a method a path is not routed for gets
// an empty 405 listing the routed methods, not the router's problem
// details.
func TestMethodNotAllowed(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "methods")

	patch := editRequest(func(r *smithyhttp.Request) { r.Method = http.MethodPatch })
	_, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("methods"), Key: aws.String("k"),
	}, patch)
	h := requireResponse(t, err, http.StatusMethodNotAllowed)
	allow := h.Values("Allow")
	slices.Sort(allow)
	requireEqual(t, strings.Join(allow, ","), "DELETE,GET,HEAD,POST,PUT", "allow")
	requireEqual(t, h.Get("Content-Type"), "", "content type")
	requireEqual(t, h.Get("Content-Length"), "0", "content length")
	requireEqual(t, h.Get("X-Content-Type-Options"), "nosniff", "security headers")

	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("methods"), Key: aws.String("k"),
	}, editRequest(func(r *smithyhttp.Request) { r.Method = "FOO" }))
	h = requireResponse(t, err, http.StatusMethodNotAllowed)
	requireEqual(t, len(h.Values("Allow")), 0, "allow for an unknown method")
	requireEqual(t, h.Get("Content-Length"), "0", "content length for an unknown method")
}

// TestCrossOriginDenied checks that a cross-site write the cross-origin
// protection rejects, because its key holds "//" and so misses the bypass
// pattern, gets net/http's plain-text 403.
func TestCrossOriginDenied(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "csrf")

	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("csrf"), Key: aws.String("a//b"), Body: strings.NewReader("x"),
	}, editRequest(func(r *smithyhttp.Request) {
		r.Header.Set("Origin", "https://other.example.test")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	}))
	h := requireResponse(t, err, http.StatusForbidden)
	requireEqual(t, h.Get("Content-Type"), "text/plain; charset=utf-8", "content type")
	requireEqual(t, h.Get("Cache-Control"), "", "cache control")
}

func TestReservedBucketNames(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	for _, name := range []string{"xn--tule", "tule-s3alias"} {
		_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)})
		requireCode(t, err, "InvalidBucketName")
	}
}

// TestObjectSemantics checks object behavior that does not follow the
// storage backend's own rules: keys with control characters, metadata past
// 2 KB or outside ASCII, and content types detected from the body.
func TestObjectSemantics(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "objects")

	const tabKey = "tab\tkey"
	put(t, c, "objects", tabKey, []byte("tab"))
	_, body := get(t, c, &s3.GetObjectInput{Bucket: aws.String("objects"), Key: aws.String(tabKey)})
	requireEqual(t, string(body), "tab", "control character key")

	meta := map[string]string{"big": strings.Repeat("m", 3000), "utf8": "héllo"}
	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("meta"),
		Body: strings.NewReader("m"), Metadata: meta,
	})
	requireNoErr(t, err, "put with metadata")
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("meta"),
	})
	requireNoErr(t, err, "head with metadata")
	requireEqual(t, head.Metadata["big"], meta["big"], "large metadata")
	requireEqual(t, head.Metadata["utf8"], meta["utf8"], "non-ASCII metadata")

	// The SDK sends application/octet-stream unless told otherwise; other
	// clients may send no Content-Type at all.
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("page"),
		Body: strings.NewReader("<html><body>hi</body></html>"),
	}, editRequest(func(r *smithyhttp.Request) { r.Header.Del("Content-Type") }))
	requireNoErr(t, err, "put without a content type")
	out, _ := get(t, c, &s3.GetObjectInput{Bucket: aws.String("objects"), Key: aws.String("page")})
	requireEqual(t, aws.ToString(out.ContentType), "text/html; charset=utf-8", "detected type")
}

// TestRanges checks range reads against the object bytes, which the storage
// service keeps apart from what it stores after them.
func TestRanges(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "ranges")
	put(t, c, "ranges", "k", []byte("hello world"))

	for _, tt := range []struct{ rng, want string }{
		{"bytes=-5", "world"},
		{"bytes=-100", "hello world"},
		{"bytes=6-", "world"},
		{"bytes=0-4", "hello"},
		{"bytes=6-100", "world"},
	} {
		_, body := get(t, c, &s3.GetObjectInput{
			Bucket: aws.String("ranges"), Key: aws.String("k"), Range: aws.String(tt.rng),
		})
		requireEqual(t, string(body), tt.want, tt.rng)
	}

	_, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("ranges"), Key: aws.String("k"), Range: aws.String("bytes=11-"),
	})
	requireResponse(t, err, http.StatusRequestedRangeNotSatisfiable)
}

func TestEmptyObject(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "empty")
	put(t, c, "empty", "k", nil)

	const emptyETag = `"d41d8cd98f00b204e9800998ecf8427e"`
	out, body := get(t, c, &s3.GetObjectInput{Bucket: aws.String("empty"), Key: aws.String("k")})
	requireEqual(t, len(body), 0, "get body")
	requireEqual(t, aws.ToInt64(out.ContentLength), 0, "get length")
	requireEqual(t, aws.ToString(out.ETag), emptyETag, "get etag")

	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("empty"), Key: aws.String("k"),
	})
	requireNoErr(t, err, "head")
	requireEqual(t, aws.ToInt64(head.ContentLength), 0, "head length")
	requireEqual(t, aws.ToString(head.ETag), emptyETag, "head etag")
}

// TestTrailerLikeBody checks that an object whose bytes end the way what the
// storage service stores after them does round-trips exactly.
func TestTrailerLikeBody(t *testing.T) {
	c := newClient(t)
	createBucket(t, c, "trailer")

	const fake = `{"lastModified":"2000-01-01T00:00:00Z","meta":{},"contentType":"x/y"}`
	body := append([]byte("payload"), fake...)
	body = append(body, 0, 0, 0, 0, 0, 0, 0, byte(len(fake)))
	body = append(body, "TULEOBJ1"...)

	out := put(t, c, "trailer", "k", body)
	requireEqual(t, aws.ToString(out.ETag), md5Hex(body), "put etag")
	got, b := get(t, c, &s3.GetObjectInput{Bucket: aws.String("trailer"), Key: aws.String("k")})
	requireEqual(t, string(b), string(body), "body")
	requireEqual(t, aws.ToString(got.ContentType), "application/octet-stream", "content type")

	_, b = get(t, c, &s3.GetObjectInput{
		Bucket: aws.String("trailer"), Key: aws.String("k"), Range: aws.String("bytes=-8"),
	})
	requireEqual(t, string(b), "TULEOBJ1", "suffix range")
}
