//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestConditionalWrites(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "cond")
	bucket, key := aws.String("cond"), aws.String("k")

	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: key, Body: strings.NewReader("v1"), IfNoneMatch: aws.String("*"),
	})
	requireNoErr(t, err, "create-only put of a new key")

	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: key, Body: strings.NewReader("v2"), IfNoneMatch: aws.String("*"),
	})
	requireCode(t, err, "PreconditionFailed")

	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: key, Body: strings.NewReader("v2"), IfMatch: aws.String(`"nope"`),
	})
	requireCode(t, err, "PreconditionFailed")

	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: key, Body: strings.NewReader("v2"),
		IfMatch: aws.String(md5Hex([]byte("v1"))),
	})
	requireNoErr(t, err, "put with matching If-Match")
	_, body := get(t, c, &s3.GetObjectInput{Bucket: bucket, Key: key})
	requireEqual(t, string(body), "v2", "body after matching If-Match")

	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("missing"), Body: strings.NewReader("x"),
		IfMatch: aws.String(md5Hex([]byte("x"))),
	})
	requireCode(t, err, "NoSuchKey")

	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: key, CopySource: copySource("cond", "k"),
		MetadataDirective: types.MetadataDirectiveReplace, IfNoneMatch: aws.String("*"),
	})
	requireCode(t, err, "PreconditionFailed")

	up := startUpload(t, c, "cond", "k")
	p1 := up.part(1, []byte("mp"))
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: key, UploadId: up.id, IfNoneMatch: aws.String("*"),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{p1}},
	})
	requireCode(t, err, "PreconditionFailed")
	// The upload survives a failed precondition and can still complete.
	_, err = up.complete(p1)
	requireNoErr(t, err, "complete after failed precondition")
}

// TestNestedKeys checks that keys are opaque strings, as on S3: an object and
// keys nested under it coexist, and folder markers and "." or ".." segments
// are ordinary keys.
func TestNestedKeys(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "keys")
	bucket := aws.String("keys")

	keys := []string{"a", "a/b", "dir/", "x//y", "../up", "p/./q"}
	for _, k := range keys {
		put(t, c, "keys", k, []byte(k))
	}
	for _, k := range keys {
		_, body := get(t, c, &s3.GetObjectInput{Bucket: bucket, Key: aws.String(k)})
		requireEqual(t, string(body), k, "body of "+k)
	}
	requireEqual(t, strings.Join(listKeys(t, c, "keys"), ","),
		"../up,a,a/b,dir/,p/./q,x//y", "listed keys")

	_, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: aws.String("a")})
	requireNoErr(t, err, "delete a")
	_, body := get(t, c, &s3.GetObjectInput{Bucket: bucket, Key: aws.String("a/b")})
	requireEqual(t, string(body), "a/b", "a/b survives deleting a")
	requireEqual(t, strings.Join(listKeys(t, c, "keys"), ","),
		"../up,a/b,dir/,p/./q,x//y", "listed keys after delete")

	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("ends/."), Body: strings.NewReader("x"),
	})
	requireCode(t, err, "InvalidArgument")
}

func TestBucketConfigReads(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "conf")

	loc, err := c.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String("conf")})
	requireNoErr(t, err, "get bucket location")
	requireEqual(t, string(loc.LocationConstraint), "", "us-east-1 location constraint")

	ver, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String("conf")})
	requireNoErr(t, err, "get bucket versioning")
	requireEqual(t, string(ver.Status), "", "versioning never enabled")

	_, err = c.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String("nope")})
	requireCode(t, err, "NoSuchBucket")
}

func TestListObjectsV1(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "list-v1")
	bucket := aws.String("list-v1")

	for _, k := range []string{"a.txt", "b/1.txt", "b/2.txt", "c.txt", "d/3.txt"} {
		put(t, c, "list-v1", k, []byte(k))
	}

	var keys []string
	marker := ""
	for pages := 0; ; pages++ {
		out, err := c.ListObjects(ctx, &s3.ListObjectsInput{
			Bucket: bucket, MaxKeys: aws.Int32(2), Marker: aws.String(marker),
		})
		requireNoErr(t, err, "list objects v1")
		requireEqual(t, out.NextMarker == nil, true, "no NextMarker without a delimiter")
		for _, o := range out.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		marker = keys[len(keys)-1]
	}
	requireEqual(t, strings.Join(keys, ","), "a.txt,b/1.txt,b/2.txt,c.txt,d/3.txt", "v1 keys")

	out, err := c.ListObjects(ctx, &s3.ListObjectsInput{
		Bucket: bucket, Delimiter: aws.String("/"), MaxKeys: aws.Int32(2),
	})
	requireNoErr(t, err, "delimited list v1")
	requireEqual(t, aws.ToBool(out.IsTruncated), true, "delimited page truncated")
	requireEqual(t, aws.ToString(out.NextMarker), "b/", "NextMarker is the common prefix")

	out, err = c.ListObjects(ctx, &s3.ListObjectsInput{
		Bucket: bucket, Delimiter: aws.String("/"), Marker: out.NextMarker,
	})
	requireNoErr(t, err, "delimited list v1 page 2")
	requireEqual(t, len(out.Contents), 1, "page 2 keys")
	requireEqual(t, aws.ToString(out.Contents[0].Key), "c.txt", "page 2 key")
	requireEqual(t, len(out.CommonPrefixes), 1, "page 2 prefixes")
	requireEqual(t, aws.ToString(out.CommonPrefixes[0].Prefix), "d/", "page 2 prefix")
}

func TestListParts(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "parts")

	up := startUpload(t, c, "parts", "big.bin")
	for n := int32(1); n <= 3; n++ {
		up.part(n, bytes.Repeat([]byte{byte(n)}, int(n)))
	}

	out, err := c.ListParts(ctx, &s3.ListPartsInput{
		Bucket: up.bucket, Key: up.key, UploadId: up.id, MaxParts: aws.Int32(2),
	})
	requireNoErr(t, err, "list parts")
	requireEqual(t, len(out.Parts), 2, "first page parts")
	requireEqual(t, aws.ToBool(out.IsTruncated), true, "first page truncated")
	requireEqual(t, aws.ToString(out.NextPartNumberMarker), "2", "next part marker")
	requireEqual(t, aws.ToInt64(out.Parts[1].Size), int64(2), "part 2 size")
	requireEqual(t, aws.ToString(out.Parts[1].ETag), md5Hex([]byte{2, 2}), "part 2 etag")

	out, err = c.ListParts(ctx, &s3.ListPartsInput{
		Bucket: up.bucket, Key: up.key, UploadId: up.id, PartNumberMarker: out.NextPartNumberMarker,
	})
	requireNoErr(t, err, "list parts page 2")
	requireEqual(t, len(out.Parts), 1, "second page parts")
	requireEqual(t, aws.ToInt32(out.Parts[0].PartNumber), 3, "second page part number")
	requireEqual(t, aws.ToBool(out.IsTruncated), false, "second page not truncated")

	requireNoErr(t, up.abort(), "abort")
	_, err = c.ListParts(ctx, &s3.ListPartsInput{Bucket: up.bucket, Key: up.key, UploadId: up.id})
	requireCode(t, err, "NoSuchUpload")
}

func TestListMultipartUploads(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "uploads")
	createBucket(t, c, "other")
	bucket := aws.String("uploads")

	startUpload(t, c, "other", "elsewhere")
	for _, k := range []string{"a", "b/1", "b/2", "c", "c"} {
		startUpload(t, c, "uploads", k)
	}

	var seen []string
	in := &s3.ListMultipartUploadsInput{Bucket: bucket, MaxUploads: aws.Int32(2)}
	for {
		out, err := c.ListMultipartUploads(ctx, in)
		requireNoErr(t, err, "list uploads")
		for _, u := range out.Uploads {
			seen = append(seen, aws.ToString(u.Key))
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		in.KeyMarker, in.UploadIdMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
	requireEqual(t, strings.Join(seen, ","), "a,b/1,b/2,c,c", "uploads across pages")

	out, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: bucket, Delimiter: aws.String("/"),
	})
	requireNoErr(t, err, "delimited list uploads")
	requireEqual(t, len(out.Uploads), 3, "top-level uploads")
	requireEqual(t, len(out.CommonPrefixes), 1, "upload prefixes")
	requireEqual(t, aws.ToString(out.CommonPrefixes[0].Prefix), "b/", "upload prefix")

	out, err = c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: bucket, Prefix: aws.String("b/"),
	})
	requireNoErr(t, err, "prefixed list uploads")
	requireEqual(t, len(out.Uploads), 2, "prefixed uploads")
}

func TestVirtualHostedStyle(t *testing.T) {
	srv := startServer(t, false, nil)
	addr := srv.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(addr)

	// Send every <bucket>.localhost request to the test server, whatever the
	// resolver makes of it.
	httpClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
	c := clientFor(srv, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("http://localhost:" + port)
		o.UsePathStyle = false
		o.HTTPClient = httpClient
	})
	ctx := context.Background()

	createBucket(t, c, "vhost")
	put(t, c, "vhost", "dir/file name.txt", []byte("hosted"))
	_, body := get(t, c, &s3.GetObjectInput{
		Bucket: aws.String("vhost"), Key: aws.String("dir/file name.txt"),
	})
	requireEqual(t, string(body), "hosted", "virtual-hosted get")

	keys := listKeys(t, c, "vhost")
	requireEqual(t, strings.Join(keys, ","), "dir/file name.txt", "virtual-hosted list")

	list, err := c.ListBuckets(ctx, &s3.ListBucketsInput{})
	requireNoErr(t, err, "list buckets on the bare domain")
	requireEqual(t, len(list.Buckets), 1, "bucket count")
}

// listKeys lists every key in bucket without checking ETags.
func listKeys(t *testing.T, c *s3.Client, bucket string) []string {
	t.Helper()
	var keys []string
	p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	for p.HasMorePages() {
		page, err := p.NextPage(context.Background())
		requireNoErr(t, err, "list page")
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys
}

func TestBrowserCORS(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createBucket(t, c, "web")
	presigner := s3.NewPresignClient(c)

	putReq, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("web"), Key: aws.String("upload.txt"),
	})
	requireNoErr(t, err, "presign put")

	const origin = "http://app.example.test"
	pre, err := http.NewRequestWithContext(ctx, http.MethodOptions, putReq.URL, nil)
	requireNoErr(t, err, "build preflight")
	pre.Header.Set("Origin", origin)
	pre.Header.Set("Access-Control-Request-Method", http.MethodPut)
	pre.Header.Set("Access-Control-Request-Headers", "content-type")
	res, err := http.DefaultClient.Do(pre)
	requireNoErr(t, err, "preflight")
	_ = res.Body.Close()
	requireEqual(t, res.Header.Get("Access-Control-Allow-Origin"), origin, "preflight allow-origin")
	requireEqual(t, strings.Contains(res.Header.Get("Access-Control-Allow-Methods"), "PUT"), true,
		"preflight allows PUT")

	// What a browser sends for a cross-site fetch() PUT.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putReq.URL,
		strings.NewReader("from the browser"))
	requireNoErr(t, err, "build put")
	req.Header.Set("Origin", origin)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	res, err = http.DefaultClient.Do(req)
	requireNoErr(t, err, "cross-site put")
	_ = res.Body.Close()
	requireEqual(t, res.StatusCode, http.StatusOK, "cross-site put status")
	requireEqual(t, res.Header.Get("Access-Control-Allow-Origin"), origin, "put allow-origin")
	requireEqual(t, strings.Contains(res.Header.Get("Access-Control-Expose-Headers"), "Etag"), true,
		"ETag exposed")
}

func TestSignatureVerification(t *testing.T) {
	srv := startServer(t, true, map[string]string{
		"TULE_Auth_AccessKeyID":     "AKIDTEST",
		"TULE_Auth_SecretAccessKey": "s3cr3t",
	})
	creds := func(id, secret string) func(*s3.Options) {
		return func(o *s3.Options) {
			o.Credentials = credentials.NewStaticCredentialsProvider(id, secret, "")
		}
	}
	c := clientFor(srv, creds("AKIDTEST", "s3cr3t"))
	ctx := context.Background()

	createBucket(t, c, "auth")
	put(t, c, "auth", "dir/a b=c.txt", []byte("signed"))
	_, body := get(t, c, &s3.GetObjectInput{
		Bucket: aws.String("auth"), Key: aws.String("dir/a b=c.txt"),
	})
	requireEqual(t, string(body), "signed", "signed get")

	// Streaming upload with a trailing checksum: the seed signature is checked.
	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("auth"), Key: aws.String("stream"),
		Body:              bytes.NewBuffer(bytes.Repeat([]byte("x"), 70_000)),
		ContentLength:     aws.Int64(70_000),
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
	})
	requireNoErr(t, err, "signed streaming put")

	up := startUpload(t, c, "auth", "mp")
	_, err = up.complete(up.part(1, []byte("part")))
	requireNoErr(t, err, "signed multipart upload")

	_, err = clientFor(srv, creds("AKIDTEST", "wrong")).ListBuckets(ctx, &s3.ListBucketsInput{})
	requireCode(t, err, "SignatureDoesNotMatch")
	_, err = clientFor(srv, creds("OTHER", "s3cr3t")).ListBuckets(ctx, &s3.ListBucketsInput{})
	requireCode(t, err, "InvalidAccessKeyId")

	status, _ := getWith(t, srv.Client(), srv.URL+"/auth/stream")
	requireEqual(t, status, http.StatusForbidden, "unsigned request")

	presigner := s3.NewPresignClient(c)
	getReq, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("auth"), Key: aws.String("dir/a b=c.txt"),
	})
	requireNoErr(t, err, "presign get")
	status, body = getWith(t, srv.Client(), getReq.URL)
	requireEqual(t, status, http.StatusOK, "presigned get status")
	requireEqual(t, string(body), "signed", "presigned get body")

	// Move the signing time back so the URL has expired; expiry is checked
	// before the signature.
	u, err := url.Parse(getReq.URL)
	requireNoErr(t, err, "parse presigned url")
	q := u.Query()
	q.Set("X-Amz-Date", time.Now().UTC().Add(-2*time.Hour).Format("20060102T150405Z"))
	u.RawQuery = q.Encode()
	status, body = getWith(t, srv.Client(), u.String())
	requireEqual(t, status, http.StatusForbidden, "expired presigned get status")
	requireEqual(t, strings.Contains(string(body), "Request has expired"), true, "expired message")
}

func getWith(t *testing.T, client *http.Client, rawURL string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	requireNoErr(t, err, "build request")
	res, err := client.Do(req)
	requireNoErr(t, err, "GET "+rawURL)
	defer func() { _ = res.Body.Close() }()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(res.Body)
	requireNoErr(t, err, "read response")
	return res.StatusCode, buf.Bytes()
}
