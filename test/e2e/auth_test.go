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
	"net/http"
	"net/http/httptest"
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
// exactly as listed, so a test can send a list the SDK never writes. host is
// the request's Host; every other name is read from its headers.
func signListBuckets(t *testing.T, srv *httptest.Server, signedHeaders []string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/", nil)
	requireNoErr(t, err, "build request")
	now := time.Now().UTC()
	req.Header.Set("X-Amz-Date", now.Format(amzDate))
	req.Header.Set("X-Amz-Content-Sha256", emptyHash)

	var headers strings.Builder
	for _, name := range signedHeaders {
		val := strings.Join(req.Header.Values(name), ",")
		if name == "host" {
			val = req.URL.Host
		}
		headers.WriteString(name + ":" + val + "\n")
	}
	list := strings.Join(signedHeaders, ";")
	creq := strings.Join([]string{
		http.MethodGet, "/", "", headers.String(), list, emptyHash,
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
