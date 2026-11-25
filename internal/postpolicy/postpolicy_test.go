package postpolicy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tule.dev/sigv4"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// formRequest is a POST of fields, in order, and then a file part holding
// file, unless file is nil.
func formRequest(t *testing.T, fields [][2]string, file []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if file != nil {
		fw, err := mw.CreateFormFile("file", "report.pdf")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = fw.Write(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/b", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

// requireCode fails unless err is the S3 error code.
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var s3Err *s3api.Error
	if !errors.As(err, &s3Err) || s3Err.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

func TestReadFormMatchesNamesRegardlessOfCase(t *testing.T) {
	f, err := readForm(formRequest(t, [][2]string{
		{"Key", "uploads/${filename}"}, {"X-Amz-Date", "d"}, {"Content-Type", "a/b"},
	}, []byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if f.fields["key"] != "uploads/report.pdf" || f.fields["x-amz-date"] != "d" ||
		f.fields["content-type"] != "a/b" {
		t.Fatalf("fields %v", f.fields)
	}
}

func TestReadFormRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		code   string
		fields [][2]string
		file   []byte
	}{
		"no file":        {fields: [][2]string{{"key", "k"}}, code: "IncorrectNumberOfFilesInPostRequest"},
		"no key":         {fields: [][2]string{{"policy", "p"}}, file: []byte("x"), code: "UserKeyMustBeSpecified"},
		"a field twice":  {fields: [][2]string{{"key", "a"}, {"KEY", "b"}}, file: []byte("x"), code: "InvalidArgument"},
		"too many bytes": {fields: [][2]string{{"key", "k"}, {"x-ignore-pad", strings.Repeat("p", MaxFieldBytes)}}, file: []byte("x"), code: "MaxPostPreDataLengthExceededError"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readForm(formRequest(t, tc.fields, tc.file))
			requireCode(t, err, tc.code)
		})
	}
}

func encode(doc string) string { return base64.StdEncoding.EncodeToString([]byte(doc)) }

func TestParsePolicyRefusesMalformedDocuments(t *testing.T) {
	for name, doc := range map[string]string{
		"not JSON":            `{`,
		"no expiration":       `{"conditions":[]}`,
		"no conditions":       `{"expiration":"2030-01-01T00:00:00Z"}`,
		"a bad expiration":    `{"expiration":"tomorrow","conditions":[]}`,
		"an unknown operator": `{"expiration":"2030-01-01T00:00:00Z","conditions":[["ends-with","$key","x"]]}`,
		"a field without $":   `{"expiration":"2030-01-01T00:00:00Z","conditions":[["eq","key","x"]]}`,
		"a non-string value":  `{"expiration":"2030-01-01T00:00:00Z","conditions":[{"key":1}]}`,
		"a reversed range":    `{"expiration":"2030-01-01T00:00:00Z","conditions":[["content-length-range",10,1]]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parsePolicy(encode(doc))
			requireCode(t, err, "InvalidPolicyDocument")
		})
	}
	_, err := parsePolicy("not base64!")
	requireCode(t, err, "InvalidPolicyDocument")
}

func TestPolicyCheck(t *testing.T) {
	p, err := parsePolicy(encode(`{"expiration":"2030-01-01T00:00:00.000Z","conditions":[
		{"bucket":"b"},
		["starts-with","$Content-Type","image/"],
		["STARTS-WITH","$x-amz-meta-note",""],
		["content-length-range",1,100],
		["content-length-range",5,50]]}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.minSize != 5 || p.maxSize != 50 {
		t.Fatalf("size range %d-%d, want the intersection 5-50", p.minSize, p.maxSize)
	}
	check := func(bucket string, fields map[string]string) error {
		return p.check(&form{fields: fields}, bucket)
	}

	if err = check("b", map[string]string{"content-type": "image/png,image/gif"}); err != nil {
		t.Fatalf("a list of image types: %v", err)
	}
	// starts-with "" holds of a field the form does not send.
	err = check("b", map[string]string{"content-type": "image/png", "x-amz-meta-note": "n"})
	if err != nil {
		t.Fatalf("any note: %v", err)
	}
	requireCode(t, check("b", map[string]string{"content-type": "image/png,text/plain"}),
		"AccessDenied")
	requireCode(t, check("other", map[string]string{"content-type": "image/png"}), "AccessDenied")
	// A field no condition names is refused, but not x-ignore-*, policy
	// or x-amz-signature.
	if err = check("b", map[string]string{
		"content-type": "image/png", "x-ignore-a": "1", "policy": "p", "x-amz-signature": "s",
	}); err != nil {
		t.Fatalf("exempt fields: %v", err)
	}
	requireCode(t, check("b", map[string]string{"content-type": "image/png", "acl": "private"}),
		"AccessDenied")
}

func TestSizedReader(t *testing.T) {
	read := func(body string, lo, hi int64) error {
		_, err := io.ReadAll(&sizedReader{r: strings.NewReader(body), min: lo, max: hi})
		return err //nolint:wrapcheck // the test inspects it as is
	}
	if err := read("12345", 5, 5); err != nil {
		t.Fatalf("exactly in range: %v", err)
	}
	if err := read("", 0, -1); err != nil {
		t.Fatalf("unbounded empty: %v", err)
	}
	if err := read("123456", 0, 5); !errors.Is(err, contracts.ErrEntityTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	if err := read("1234", 5, 10); !errors.Is(err, contracts.ErrEntityTooSmall) {
		t.Fatalf("too small: %v", err)
	}
}

// fakeKeys hands out key for any scope, or err, and records the scope it
// was asked for.
type fakeKeys struct {
	err   error
	asked *sigv4.Scope
	key   sigv4.Key
}

func (k fakeKeys) SigningKey(_ context.Context, scope sigv4.Scope) (sigv4.Key, error) {
	*k.asked = scope
	return k.key, k.err
}

// signedForm is a form of fields whose policy, of conds, is signed with
// signing for the credential cred on 2030-01-02, and file.
func signedForm(
	t *testing.T, signing []byte, cred string, conds string, fields ...[2]string,
) *http.Request {
	t.Helper()
	encoded := encode(`{"expiration":"2030-12-31T00:00:00Z","conditions":[` + conds + `]}`)
	mac := hmac.New(sha256.New, signing)
	mac.Write([]byte(encoded))
	return formRequest(t, append([][2]string{
		{"key", "k"},
		{"policy", encoded},
		{"X-Amz-Algorithm", "AWS4-HMAC-SHA256"},
		{"X-Amz-Credential", cred},
		{"X-Amz-Date", "20300102T030405Z"},
		{"X-Amz-Signature", hex.EncodeToString(mac.Sum(nil))},
	}, fields...), []byte("x"))
}

// signedConds are the conditions every signedForm needs.
const signedConds = `{"key":"k"},{"x-amz-algorithm":"AWS4-HMAC-SHA256"},` +
	`{"x-amz-credential":"AKID/20300102/eu-west-1/s3/aws4_request"},` +
	`{"x-amz-date":"20300102T030405Z"}`

const goodCred = "AKID/20300102/eu-west-1/s3/aws4_request" //nolint:gosec // G101: a test credential scope

// signing is the signing key the tests' forms are signed with.
var signing = []byte("signing key")

// now is the time the tests verify their forms at.
var now = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

// keys is a key source handing out signing, recording the scope it was
// asked for in asked.
func keys(asked *sigv4.Scope) fakeKeys {
	return fakeKeys{asked: asked, key: sigv4.Key{
		Signing: signing, Principal: sigv4.Principal{AccountID: "000000000000"},
	}}
}

func TestVerify(t *testing.T) {
	t.Run("a session token is passed to the key source", func(t *testing.T) {
		var asked sigv4.Scope
		r := signedForm(t, signing, goodCred, signedConds+`,{"x-amz-security-token":"tok"}`,
			[2]string{"X-Amz-Security-Token", "tok"})
		up, err := Verify(t.Context(), r, "b", keys(&asked), now)
		if err != nil {
			t.Fatal(err)
		}
		want := sigv4.Scope{
			AccessKeyID: "AKID", SessionToken: "tok", Date: "20300102", Region: "eu-west-1",
			Service: "s3",
		}
		if asked != want || up.Key != "k" || up.Bucket != "b" || up.Status != http.StatusNoContent {
			t.Fatalf("scope %+v, upload %+v", asked, up)
		}
	})

	t.Run("a session token the policy does not cover is refused", func(t *testing.T) {
		var asked sigv4.Scope
		r := signedForm(t, signing, goodCred, signedConds,
			[2]string{"X-Amz-Security-Token", "tok"})
		_, err := Verify(t.Context(), r, "b", keys(&asked), now)
		requireCode(t, err, "AccessDenied")
	})

	t.Run("a key source failure is a KeyError", func(t *testing.T) {
		var asked sigv4.Scope
		k := keys(&asked)
		k.err = sigv4.ErrUnknownAccessKey
		_, err := Verify(t.Context(), signedForm(t, signing, goodCred, signedConds), "b", k, now)
		var kErr *KeyError
		if !errors.As(err, &kErr) || !errors.Is(err, sigv4.ErrUnknownAccessKey) {
			t.Fatalf("got %v, want a KeyError wrapping ErrUnknownAccessKey", err)
		}
	})
}

func TestVerifyRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		r    func(t *testing.T) *http.Request
		code string
	}{
		"no policy": {code: "AccessDenied", r: func(t *testing.T) *http.Request {
			return formRequest(t, [][2]string{{"key", "k"}}, []byte("x"))
		}},
		"another algorithm": {code: "InvalidRequest", r: func(t *testing.T) *http.Request {
			return withAlgorithm(t, signedForm(t, signing, goodCred, signedConds),
				"AWS4-ECDSA-P256-SHA256")
		}},
		"another service": {code: "InvalidArgument", r: func(t *testing.T) *http.Request {
			return signedForm(t, signing, "AKID/20300102/eu-west-1/sqs/aws4_request",
				signedConds)
		}},
		"another day": {code: "InvalidArgument", r: func(t *testing.T) *http.Request {
			return signedForm(t, signing, "AKID/20300103/eu-west-1/s3/aws4_request",
				signedConds)
		}},
		"another signing key": {code: "SignatureDoesNotMatch", r: func(t *testing.T) *http.Request {
			return signedForm(t, []byte("other key"), goodCred, signedConds)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			var asked sigv4.Scope
			_, err := Verify(t.Context(), tc.r(t), "b", keys(&asked), now)
			requireCode(t, err, tc.code)
		})
	}
}

// withAlgorithm returns r, a form, with its X-Amz-Algorithm field's value
// replaced by alg.
func withAlgorithm(t *testing.T, r *http.Request, alg string) *http.Request {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte("AWS4-HMAC-SHA256\r\n"), []byte(alg+"\r\n"), 1)
	r.Body = io.NopCloser(bytes.NewReader(b))
	return r
}
