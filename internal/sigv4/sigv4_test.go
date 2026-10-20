package sigv4

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"tule.dev/object-store/internal/s3api"
)

var (
	creds    = Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret"}
	signTime = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
)

func verifier() *Verifier {
	v := NewVerifier(creds)
	v.now = func() time.Time { return signTime.Add(time.Minute) }
	return v
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// body is what signed requests upload.
const body = "hello"

// signedRequest builds a PUT of body signed by the AWS SDK signer the way the
// S3 client signs: without re-escaping the path.
func signedRequest(t *testing.T, target string, key Credentials) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPut,
		target,
		strings.NewReader(body),
	)
	hash := sha256Hex(body)
	r.Header.Set("X-Amz-Content-Sha256", hash)
	r.Header.Set("Content-Type", "text/plain")

	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	err := signer.SignHTTP(context.Background(), aws.Credentials{
		AccessKeyID: key.AccessKeyID, SecretAccessKey: key.SecretAccessKey,
	}, r, hash, "s3", "eu-west-1", signTime)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return r
}

func presignedRequest(t *testing.T, method, target string, expires time.Duration) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	q := r.URL.Query()
	q.Set("X-Amz-Expires", strconv.Itoa(int(expires.Seconds())))
	r.URL.RawQuery = q.Encode()

	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	u, _, err := signer.PresignHTTP(context.Background(), aws.Credentials{
		AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey,
	}, r, unsigned, "s3", "us-east-1", signTime)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	return httptest.NewRequestWithContext(context.Background(), method, u, nil)
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var s3Err *s3api.Error
	if !errors.As(err, &s3Err) || s3Err.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestVerifyHeader(t *testing.T) {
	const target = "http://localhost:9000/bucket/dir/sub%20dir/file%3D%2B%26%40%21.txt?uploads="
	r := signedRequest(t, target, creds)
	if err := verifier().Verify(r); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, err := io.ReadAll(r.Body)
	if err != nil || string(got) != body {
		t.Fatalf("body: %q %v", got, err)
	}
}

func TestVerifyHeaderFailures(t *testing.T) {
	const target = "http://localhost:9000/bucket/key"

	r := signedRequest(t, target, creds)
	r.URL.Path = "/bucket/other"
	wantCode(t, verifier().Verify(r), "SignatureDoesNotMatch")

	r = signedRequest(t, target, Credentials{"AKID", "wrong"})
	wantCode(t, verifier().Verify(r), "SignatureDoesNotMatch")

	r = signedRequest(t, target, Credentials{"OTHER", "secret"})
	wantCode(t, verifier().Verify(r), "InvalidAccessKeyId")

	r = signedRequest(t, target, creds)
	v := verifier()
	v.now = func() time.Time { return signTime.Add(time.Hour) }
	wantCode(t, v.Verify(r), "RequestTimeTooSkewed")

	r = httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	wantCode(t, verifier().Verify(r), "AccessDenied")

	r.Header.Set("Authorization", "AWS AKID:abc")
	wantCode(t, verifier().Verify(r), "InvalidRequest")
}

func TestVerifyPayloadMismatch(t *testing.T) {
	r := signedRequest(t, "http://localhost:9000/bucket/key", creds)
	if err := verifier().Verify(r); err != nil {
		t.Fatalf("verify: %v", err)
	}
	r.Body = &payloadVerifier{
		body: io.NopCloser(strings.NewReader("tampered")),
		h:    sha256.New(),
		want: sha256Hex(body),
	}
	_, err := io.ReadAll(r.Body)
	wantCode(t, err, "XAmzContentSHA256Mismatch")
}

func TestVerifyPresigned(t *testing.T) {
	const target = "http://localhost:9000/bucket/up/loaded%20file.txt"

	r := presignedRequest(t, http.MethodGet, target, time.Hour)
	if err := verifier().Verify(r); err != nil {
		t.Fatalf("verify: %v", err)
	}

	r = presignedRequest(t, http.MethodGet, target, 30*time.Second)
	wantCode(t, verifier().Verify(r), "AccessDenied")

	r = presignedRequest(t, http.MethodGet, target, time.Hour)
	r.Method = http.MethodPut
	wantCode(t, verifier().Verify(r), "SignatureDoesNotMatch")
}

// TestVerifyKeepsHeaderValues covers a signed header whose value carries runs
// of spaces: collapsing them is part of computing the canonical request, and
// must not reach the request the handlers go on to read.
func TestVerifyKeepsHeaderValues(t *testing.T) {
	const (
		target = "http://localhost:9000/bucket/key"
		note   = "  two  spaces  inside  "
	)

	r := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPut,
		target,
		strings.NewReader(body),
	)
	hash := sha256Hex(body)
	r.Header.Set("X-Amz-Content-Sha256", hash)
	r.Header.Set("X-Amz-Meta-Note", note)

	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if err := signer.SignHTTP(context.Background(), aws.Credentials{
		AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey,
	}, r, hash, "s3", "eu-west-1", signTime); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if got := r.Header.Get("X-Amz-Meta-Note"); got != note {
		t.Skipf("the signer itself rewrote the header to %q", got)
	}

	if err := verifier().Verify(r); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got := r.Header.Get("X-Amz-Meta-Note"); got != note {
		t.Fatalf("header value = %q, want %q", got, note)
	}
}

// TestVerifyRejectsUnsignedAmzHeaders covers x-amz-* headers added to a
// request after it was signed. They are not covered by the signature, so a
// holder of a signed request or a presigned URL could otherwise change what an
// upload stores.
func TestVerifyRejectsUnsignedAmzHeaders(t *testing.T) {
	const target = "http://localhost:9000/bucket/key"

	r := signedRequest(t, target, creds)
	r.Header.Set("X-Amz-Meta-Injected", "yes")
	wantCode(t, verifier().Verify(r), "AccessDenied")

	r = presignedRequest(t, http.MethodPut, target, time.Hour)
	r.Header.Set("X-Amz-Meta-Injected", "yes")
	wantCode(t, verifier().Verify(r), "AccessDenied")
}

// signWithHeaders signs r over exactly names, the way a client that leaves a
// header the SDK would sign out of its SignedHeaders list does.
func signWithHeaders(t *testing.T, r *http.Request, names []string) {
	t.Helper()

	scope := signTime.Format("20060102") + "/eu-west-1/s3/" + scopeSuffix
	r.Header.Set("X-Amz-Date", signTime.Format(amzDateFmt))
	sig := verifier().signature(r, &signed{
		date:          signTime,
		scope:         scope,
		region:        "eu-west-1",
		service:       "s3",
		payloadHash:   r.Header.Get("X-Amz-Content-Sha256"),
		signedHeaders: names,
	})

	r.Header.Set("Authorization", algorithm+" Credential="+creds.AccessKeyID+"/"+scope+
		",SignedHeaders="+strings.Join(names, ";")+",Signature="+sig)
}

// TestVerifyRequiresSignedHost covers a request validly signed without its
// Host header. The host chooses the bucket in virtual-hosted-style addressing,
// so the same signed request would otherwise address whatever bucket its
// sender points it at.
func TestVerifyRequiresSignedHost(t *testing.T) {
	r := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"http://localhost:9000/bucket/key",
		nil,
	)
	r.Header.Set("X-Amz-Content-Sha256", unsigned)
	signWithHeaders(t, r, []string{"x-amz-content-sha256", "x-amz-date"})

	wantCode(t, verifier().Verify(r), "AccessDenied")
}

// TestVerifyBindsTheHostHeader covers a request that lists its Host header
// under a name that is not lowercase. The host chooses the bucket in
// virtual-hosted-style addressing, so a signature taken over one host must
// not verify against another, whatever case the client wrote the name in.
func TestVerifyBindsTheHostHeader(t *testing.T) {
	r := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"http://bucket.localhost:9000/key",
		nil,
	)
	r.Header.Set("X-Amz-Content-Sha256", unsigned)
	signWithHeaders(t, r, []string{"Host", "x-amz-content-sha256", "x-amz-date"})

	r.Host = "other.localhost:9000"
	wantCode(t, verifier().Verify(r), "SignatureDoesNotMatch")
}
