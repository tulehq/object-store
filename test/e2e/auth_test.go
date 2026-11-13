//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const (
	amzDate   = "20060102T150405Z"
	emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

var testCreds = aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}

// tamper is a transport that edits each request after the SDK signed it,
// as whoever relays a signed request could.
type tamper struct {
	base http.RoundTripper
	edit func(t *testing.T, r *http.Request)
	t    *testing.T
}

func (tr tamper) RoundTrip(r *http.Request) (*http.Response, error) {
	tr.edit(tr.t, r)
	return tr.base.RoundTrip(r) //nolint:wrapcheck // the base transport's error, as is
}

// tampered returns a client option that edits every request after signing.
func tampered(t *testing.T, srv *httptest.Server, edit func(*testing.T, *http.Request)) func(
	*s3.Options,
) {
	return func(o *s3.Options) {
		o.HTTPClient = &http.Client{
			Transport: tamper{base: srv.Client().Transport, edit: edit, t: t},
		}
	}
}

// swapBody replaces old with replacement in a request body of the same
// length, keeping Content-Length and the signed payload hash.
func swapBody(old, replacement string) func(*testing.T, *http.Request) {
	return func(t *testing.T, r *http.Request) {
		t.Helper()
		if r.Body == nil {
			return
		}
		b, err := io.ReadAll(r.Body)
		requireNoErr(t, err, "read signed body")
		_ = r.Body.Close()
		if !bytes.Contains(b, []byte(old)) {
			t.Fatalf("signed body %q does not contain %q", b, old)
		}
		forged := bytes.Replace(b, []byte(old), []byte(replacement), 1)
		requireEqual(t, len(forged), len(b), "forged body length")
		r.Body = io.NopCloser(bytes.NewReader(forged))
		r.GetBody = nil
	}
}

// s3Error is an S3 error response body.
type s3Error struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// send sends req and returns its status and decoded S3 error, if any.
func send(t *testing.T, client *http.Client, req *http.Request) (int, s3Error) {
	t.Helper()
	res, err := client.Do(req)
	requireNoErr(t, err, req.Method+" "+req.URL.String())
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	requireNoErr(t, err, "read response")
	var e s3Error
	if res.StatusCode >= 300 && len(b) > 0 {
		requireNoErr(t, xml.Unmarshal(b, &e), "decode error "+string(b))
	}
	return res.StatusCode, e
}

func requireS3Error(t *testing.T, status int, e s3Error, wantStatus int, code, message string) {
	t.Helper()
	requireEqual(t, status, wantStatus, "status")
	requireEqual(t, e.Code, code, "code")
	if message != "" {
		requireEqual(t, e.Message, message, "message")
	}
}

func requireAPIError(t *testing.T, err error, code, message string) {
	t.Helper()
	requireCode(t, err, code)
	var apiErr smithy.APIError
	errors.As(err, &apiErr)
	requireEqual(t, apiErr.ErrorMessage(), message, "message")
}

// signedRequest builds a request signed with the SDK's signer for service
// and region at, with X-Amz-Content-Sha256 set to payloadHash.
func signedRequest(
	t *testing.T, srv *httptest.Server, method, path string, body []byte,
	payloadHash, service, region string, at time.Time,
) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path,
		bytes.NewReader(body))
	requireNoErr(t, err, "build request")
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	err = v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }).
		SignHTTP(context.Background(), testCreds, req, payloadHash, service, region, at)
	requireNoErr(t, err, "sign request")
	return req
}

// signListBuckets signs a ListBuckets request by hand, with SignedHeaders
// exactly as listed, so a test can send a list the SDK never writes.
func signListBuckets(t *testing.T, srv *httptest.Server, signedHeaders []string) *http.Request {
	t.Helper()
	return signGet(t, srv, "", "/", signedHeaders)
}

// signGet signs a GET of path by hand, with SignedHeaders exactly as
// listed, sent with host as its Host, or the server's when empty. host is
// read from the request's Host; every other name from its headers.
func signGet(t *testing.T, srv *httptest.Server, host, path string, signedHeaders []string,
) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
	requireNoErr(t, err, "build request")
	if host != "" {
		req.Host = host
	}
	now := time.Now().UTC()
	req.Header.Set("X-Amz-Date", now.Format(amzDate))
	req.Header.Set("X-Amz-Content-Sha256", emptyHash)

	var headers strings.Builder
	for _, name := range signedHeaders {
		val := strings.Join(req.Header.Values(name), ",")
		if name == "host" {
			val = req.Host
		}
		headers.WriteString(name + ":" + val + "\n")
	}
	list := strings.Join(signedHeaders, ";")
	creq := strings.Join([]string{
		http.MethodGet, path, "", headers.String(), list, emptyHash,
	}, "\n")
	scope := now.Format("20060102") + "/us-east-1/s3/aws4_request"
	sum := sha256.Sum256([]byte(creq))
	toSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", now.Format(amzDate), scope, hex.EncodeToString(sum[:]),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+testCreds.SecretAccessKey), now.Format("20060102"))
	for _, part := range []string{"us-east-1", "s3", "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+testCreds.AccessKeyID+"/"+
		scope+", SignedHeaders="+list+", Signature="+hex.EncodeToString(hmacSHA256(key, toSign)))
	return req
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func requireAbsent(t *testing.T, c *s3.Client, bucket, key string) {
	t.Helper()
	_, err := c.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	requireAs[*types.NotFound](t, err)
	for _, k := range listKeys(t, c, bucket) {
		if k == key {
			t.Fatalf("%q is listed", key)
		}
	}
}

func requireBody(t *testing.T, c *s3.Client, bucket, key, want string) {
	t.Helper()
	_, body := get(t, c, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	requireEqual(t, string(body), want, key+" body")
}

// TestUnsignedAmzHeader checks that an x-amz-* header added after signing is
// refused, so it cannot change what a signed request does.
func TestUnsignedAmzHeader(t *testing.T) {
	srv := startServer(t, false, nil)
	c := clientFor(srv)
	ctx := context.Background()
	createBucket(t, c, "unsigned")
	put(t, c, "unsigned", "secret", []byte("secret"))

	const notSigned = "There were headers present in the request which were not signed"
	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("unsigned"), Key: aws.String("meta"),
		Body: strings.NewReader("body"),
	}, tampered(t, srv, func(_ *testing.T, r *http.Request) {
		r.Header.Set("X-Amz-Meta-Injected", "yes")
	}))
	requireAPIError(t, err, "AccessDenied", notSigned)
	requireAbsent(t, c, "unsigned", "meta")

	// A presigned PUT given X-Amz-Copy-Source would be a CopyObject.
	putReq, err := s3.NewPresignClient(c).PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("unsigned"), Key: aws.String("copy"),
	})
	requireNoErr(t, err, "presign put")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putReq.URL, nil)
	requireNoErr(t, err, "build put")
	req.Header.Set("X-Amz-Copy-Source", "/unsigned/secret")
	status, e := send(t, srv.Client(), req)
	requireS3Error(t, status, e, http.StatusForbidden, "AccessDenied", notSigned)
	requireAbsent(t, c, "unsigned", "copy")
}

// TestPresignedNotYetValid checks that a presigned URL dated beyond the
// clock skew is refused, as S3 refuses it.
func TestPresignedNotYetValid(t *testing.T) {
	srv := startServer(t, false, nil)
	c := clientFor(srv)
	createBucket(t, c, "future")
	put(t, c, "future", "k", []byte("v"))

	presign := func(at time.Time) *http.Request {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			srv.URL+"/future/k?X-Amz-Expires=900", nil)
		requireNoErr(t, err, "build request")
		u, _, err := v4.NewSigner().PresignHTTP(context.Background(), testCreds, req,
			"UNSIGNED-PAYLOAD", "s3", "us-east-1", at)
		requireNoErr(t, err, "presign")
		req, err = http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
		requireNoErr(t, err, "build presigned request")
		return req
	}

	status, _ := send(t, srv.Client(), presign(time.Now().Add(5*time.Minute)))
	requireEqual(t, status, http.StatusOK, "presigned url dated within the skew")

	status, e := send(t, srv.Client(), presign(time.Now().Add(time.Hour)))
	requireS3Error(t, status, e, http.StatusForbidden, "AccessDenied", "Request is not yet valid")
}

// TestTamperedPutObject checks that an upload whose body does not match its
// signed payload hash stores nothing: no partial object for a new key, and
// the old object for an existing one.
func TestTamperedPutObject(t *testing.T) {
	srv := startServer(t, false, nil)
	c := clientFor(srv)
	ctx := context.Background()
	createBucket(t, c, "tamper")
	put(t, c, "tamper", "existing", []byte("original"))

	forge := tampered(t, srv, swapBody("signed-body", "forged-body"))
	for _, key := range []string{"new", "existing"} {
		_, err := c.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("tamper"), Key: aws.String(key),
			Body: strings.NewReader(strings.Repeat("x", 100_000) + "signed-body"),
		}, forge)
		requireCode(t, err, "XAmzContentSHA256Mismatch")
	}
	requireAbsent(t, c, "tamper", "new")
	requireBody(t, c, "tamper", "existing", "original")
}

// TestTamperedUploadPart checks that a part whose body does not match its
// signed payload hash is not stored.
func TestTamperedUploadPart(t *testing.T) {
	srv := startServer(t, false, nil)
	c := clientFor(srv)
	ctx := context.Background()
	createBucket(t, c, "tamper")
	up := startUpload(t, c, "tamper", "mp")
	up.part(1, []byte("first signed-body"))

	for _, n := range []int32{1, 2} {
		_, err := c.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: up.bucket, Key: up.key, UploadId: up.id, PartNumber: aws.Int32(n),
			Body: strings.NewReader("second signed-body"),
		}, tampered(t, srv, swapBody("signed-body", "forged-body")))
		requireCode(t, err, "XAmzContentSHA256Mismatch")
	}

	out, err := c.ListParts(ctx, &s3.ListPartsInput{
		Bucket: up.bucket, Key: up.key, UploadId: up.id,
	})
	requireNoErr(t, err, "list parts")
	requireEqual(t, len(out.Parts), 1, "parts")
	requireEqual(t, aws.ToString(out.Parts[0].ETag), md5Hex([]byte("first signed-body")),
		"part 1 etag")
}

// TestTamperedDeleteObjects checks that a DeleteObjects document forged to
// the signed one's length is refused before anything is deleted.
func TestTamperedDeleteObjects(t *testing.T) {
	srv := startServer(t, false, nil)
	c := clientFor(srv)
	createBucket(t, c, "tamper")
	put(t, c, "tamper", "keep", []byte("keep"))

	_, err := c.DeleteObjects(context.Background(), &s3.DeleteObjectsInput{
		Bucket: aws.String("tamper"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("gone")}}},
	}, tampered(t, srv, swapBody("<Key>gone</Key>", "<Key>keep</Key>")))
	requireCode(t, err, "XAmzContentSHA256Mismatch")
	requireBody(t, c, "tamper", "keep", "keep")
}

// TestTamperedCompleteMultipartUpload checks that a completion document
// forged to the signed one's length is refused before the upload completes.
func TestTamperedCompleteMultipartUpload(t *testing.T) {
	srv := startServer(t, false, nil)
	c := clientFor(srv)
	createBucket(t, c, "tamper")
	up := startUpload(t, c, "tamper", "mp")
	part := up.part(1, []byte("the part"))

	// Signed for a part ETag that was never uploaded, forged to the real one.
	wrong := md5Hex([]byte("not the part"))
	_, err := clientFor(srv, tampered(t, srv, swapBody(
		strings.Trim(wrong, `"`), strings.Trim(aws.ToString(part.ETag), `"`),
	))).CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
		Bucket: up.bucket, Key: up.key, UploadId: up.id,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: aws.String(wrong)},
		}},
	})
	requireCode(t, err, "XAmzContentSHA256Mismatch")
	requireAbsent(t, c, "tamper", "mp")

	_, err = up.complete(part)
	requireNoErr(t, err, "complete after the forged attempt")
}

// TestStrictSignedHeaders checks that a SignedHeaders list that is not
// lowercase, sorted and unique is refused, even when signed as listed.
func TestStrictSignedHeaders(t *testing.T) {
	srv := startServer(t, false, nil)

	status, _ := send(t, srv.Client(), signListBuckets(t, srv,
		[]string{"host", "x-amz-content-sha256", "x-amz-date"}))
	requireEqual(t, status, http.StatusOK, "canonical list")

	for _, list := range [][]string{
		{"x-amz-date", "host", "x-amz-content-sha256"},
		{"Host", "x-amz-content-sha256", "x-amz-date"},
		{"host", "host", "x-amz-content-sha256", "x-amz-date"},
	} {
		status, e := send(t, srv.Client(), signListBuckets(t, srv, list))
		requireS3Error(t, status, e, http.StatusBadRequest, "AuthorizationHeaderMalformed", "")
	}
}

// TestRepeatedSignedHeaders checks that a large header named many times in
// SignedHeaders is refused before it is canonicalized once per name.
func TestRepeatedSignedHeaders(t *testing.T) {
	srv := startServer(t, false, nil)
	req := signedRequest(t, srv, http.MethodGet, "/", nil, emptyHash, "s3", "us-east-1",
		time.Now())
	req.Header.Set("X-Amz-Meta-Pad", strings.Repeat("p", 128<<10))
	list := strings.Repeat("x-amz-meta-pad;", 4000) + "x-amz-meta-pad"
	auth := req.Header.Get("Authorization")
	start := strings.Index(auth, "SignedHeaders=") + len("SignedHeaders=")
	end := start + strings.Index(auth[start:], ",")
	req.Header.Set("Authorization", auth[:start]+list+auth[end:])

	began := time.Now()
	status, e := send(t, srv.Client(), req)
	elapsed := time.Since(began)
	requireS3Error(t, status, e, http.StatusBadRequest, "AuthorizationHeaderMalformed", "")
	if elapsed > 2*time.Second {
		t.Fatalf("refusing the repeated list took %s", elapsed)
	}
}

// TestNonHexPayloadHash checks that an X-Amz-Content-Sha256 that is neither
// a hex SHA-256, UNSIGNED-PAYLOAD nor STREAMING-* is refused, as S3 does.
func TestNonHexPayloadHash(t *testing.T) {
	srv := startServer(t, false, nil)
	c := clientFor(srv)
	createBucket(t, c, "hash")

	for _, hash := range []string{"UNSIGNED-PAYLOAD", hexSHA256("body")} {
		req := signedRequest(t, srv, http.MethodPut, "/hash/ok", []byte("body"), hash, "s3",
			"us-east-1", time.Now())
		status, _ := send(t, srv.Client(), req)
		requireEqual(t, status, http.StatusOK, hash)
	}

	req := signedRequest(t, srv, http.MethodPut, "/hash/bad", []byte("body"), "true", "s3",
		"us-east-1", time.Now())
	status, e := send(t, srv.Client(), req)
	requireS3Error(t, status, e, http.StatusBadRequest, "InvalidArgument",
		"x-amz-content-sha256 must be UNSIGNED-PAYLOAD, STREAMING-AWS4-HMAC-SHA256-PAYLOAD, "+
			"or a valid sha256 value.")
	requireAbsent(t, c, "hash", "bad")
}

func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestSignedForAnotherService checks that a request signed for another
// service is refused, while any region is accepted.
func TestSignedForAnotherService(t *testing.T) {
	srv := startServer(t, false, nil)

	req := signedRequest(t, srv, http.MethodGet, "/", nil, emptyHash, "s3", "local-1",
		time.Now())
	status, _ := send(t, srv.Client(), req)
	requireEqual(t, status, http.StatusOK, "any region")

	req = signedRequest(t, srv, http.MethodGet, "/", nil, emptyHash, "secretsmanager",
		"us-east-1", time.Now())
	status, e := send(t, srv.Client(), req)
	requireS3Error(t, status, e, http.StatusBadRequest, "AuthorizationHeaderMalformed", "")

	// A presigned URL scoped to another service.
	pre, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/?X-Amz-Expires="+strconv.Itoa(900), nil)
	requireNoErr(t, err, "build request")
	u, _, err := v4.NewSigner().PresignHTTP(context.Background(), testCreds, pre,
		"UNSIGNED-PAYLOAD", "secretsmanager", "us-east-1", time.Now())
	requireNoErr(t, err, "presign")
	parsed, err := url.Parse(u)
	requireNoErr(t, err, "parse presigned url")
	pre, err = http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		parsed.String(),
		nil,
	)
	requireNoErr(t, err, "build presigned request")
	status, e = send(t, srv.Client(), pre)
	requireS3Error(t, status, e, http.StatusBadRequest, "AuthorizationQueryParametersError", "")
}

// TestSessionTokenRefused checks that the static key refuses any session
// token, as S3 refuses an invalid one.
func TestSessionTokenRefused(t *testing.T) {
	srv := startServer(t, false, nil)
	c := clientFor(srv, func(o *s3.Options) {
		o.Credentials = credentials.NewStaticCredentialsProvider("test", "test", "a-token")
	})
	_, err := c.ListBuckets(context.Background(), &s3.ListBucketsInput{})
	requireAPIError(t, err, "InvalidToken", "The provided token is malformed or otherwise invalid.")

	presigned, err := s3.NewPresignClient(c).PresignGetObject(context.Background(),
		&s3.GetObjectInput{Bucket: aws.String("b"), Key: aws.String("k")})
	requireNoErr(t, err, "presign get")
	status, body := getWith(t, srv.Client(), presigned.URL)
	var e s3Error
	requireNoErr(t, xml.Unmarshal(body, &e), "decode error")
	requireS3Error(t, status, e, http.StatusBadRequest, "InvalidToken",
		"The provided token is malformed or otherwise invalid.")
}

// TestHostMustBeSigned checks that a signature must cover Host, which picks
// the bucket of a virtual-hosted request, so a relayed request cannot be
// pointed at another bucket.
func TestHostMustBeSigned(t *testing.T) {
	srv := startServer(t, false, nil)
	c := clientFor(srv)
	createBucket(t, c, "public")
	createBucket(t, c, "private")
	put(t, c, "public", "k", []byte("public"))
	put(t, c, "private", "k", []byte("private"))
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	requireNoErr(t, err, "split server address")
	public, private := "public.localhost:"+port, "private.localhost:"+port

	status, _ := send(t, srv.Client(), signGet(t, srv, public, "/k",
		[]string{"host", "x-amz-content-sha256", "x-amz-date"}))
	requireEqual(t, status, http.StatusOK, "virtual-hosted get with host signed")

	req := signGet(t, srv, public, "/k", []string{"x-amz-content-sha256", "x-amz-date"})
	req.Host = private
	status, e := send(t, srv.Client(), req)
	requireS3Error(t, status, e, http.StatusForbidden, "AccessDenied", "")

	presigned, err := s3.NewPresignClient(c).PresignGetObject(context.Background(),
		&s3.GetObjectInput{Bucket: aws.String("public"), Key: aws.String("k")})
	requireNoErr(t, err, "presign get")
	status, body := getWith(t, srv.Client(), presigned.URL)
	requireEqual(t, status, http.StatusOK, "presigned get")
	requireEqual(t, string(body), "public", "presigned get body")

	// SDK-signed and presigned virtual-hosted requests sign their Host.
	addr := srv.Listener.Addr().String()
	vhost := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
	vc := clientFor(srv, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("http://localhost:" + port)
		o.UsePathStyle = false
		o.HTTPClient = vhost
	})
	requireBody(t, vc, "public", "k", "public")
	presigned, err = s3.NewPresignClient(vc).PresignGetObject(context.Background(),
		&s3.GetObjectInput{Bucket: aws.String("public"), Key: aws.String("k")})
	requireNoErr(t, err, "presign virtual-hosted get")
	status, body = getWith(t, vhost, presigned.URL)
	requireEqual(t, status, http.StatusOK, "presigned virtual-hosted get")
	requireEqual(t, string(body), "public", "presigned virtual-hosted get body")
}
