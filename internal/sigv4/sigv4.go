// Package sigv4 verifies AWS Signature Version 4 on S3 requests, in the
// Authorization header and in presigned URLs.
//
// Verification is request-level: the signature over the method, path, query,
// signed headers and declared payload hash is checked, and a hex payload hash
// is checked against the body as it is read. The per-chunk signatures of
// aws-chunked (STREAMING-*) bodies are not verified.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"tule.dev/object-store/internal/s3api"
)

const (
	algorithm   = "AWS4-HMAC-SHA256"
	amzDateFmt  = "20060102T150405Z"
	maxSkew     = 15 * time.Minute
	maxExpires  = 7 * 24 * 60 * 60
	unsigned    = "UNSIGNED-PAYLOAD"
	scopeSuffix = "aws4_request"
)

// Credentials is the one access key the server accepts.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// Verifier checks request signatures against Credentials.
type Verifier struct {
	now   func() time.Time
	creds Credentials
}

// NewVerifier returns a Verifier for creds.
func NewVerifier(creds Credentials) *Verifier {
	return &Verifier{creds: creds, now: time.Now}
}

// signed is what a request claims about its signature.
type signed struct {
	date          time.Time
	scope         string
	region        string
	service       string
	signature     string
	payloadHash   string
	signedHeaders []string
	presigned     bool
}

// Verify checks r's signature and returns an *s3api.Error when it is missing
// or wrong. When the request declares a hex payload hash, r.Body is replaced
// with a reader that fails with XAmzContentSHA256Mismatch at EOF if the body
// does not match it.
func (v *Verifier) Verify(r *http.Request) error {
	var (
		s   *signed
		err error
	)
	switch {
	case r.URL.Query().Get("X-Amz-Algorithm") != "":
		s, err = v.parsePresigned(r)
	case r.Header.Get("Authorization") != "":
		s, err = v.parseHeader(r)
	default:
		return accessDenied("Access Denied")
	}
	if err != nil {
		return err
	}

	if err = checkSignedHeaders(r, s); err != nil {
		return err
	}

	want := v.signature(r, s)
	if !hmac.Equal([]byte(want), []byte(s.signature)) {
		return s3api.NewError(http.StatusForbidden, "SignatureDoesNotMatch",
			"The request signature we calculated does not match the signature you provided. "+
				"Check your key and signing method.")
	}

	if isHexSHA256(s.payloadHash) && r.Body != nil {
		r.Body = &payloadVerifier{body: r.Body, h: sha256.New(), want: s.payloadHash}
	}
	return nil
}

func (v *Verifier) parseHeader(r *http.Request) (*signed, error) {
	params, err := authParams(r.Header.Get("Authorization"))
	if err != nil {
		return nil, err
	}

	date, err := time.Parse(amzDateFmt, r.Header.Get("X-Amz-Date"))
	if err != nil {
		return nil, accessDenied("AWS authentication requires a valid X-Amz-Date header")
	}
	if skew := v.now().Sub(date); skew > maxSkew || skew < -maxSkew {
		return nil, s3api.NewError(http.StatusForbidden, "RequestTimeTooSkewed",
			"The difference between the request time and the current time is too large.")
	}

	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		return nil, s3api.ErrInvalidRequest(
			"Missing required header for this request: x-amz-content-sha256")
	}

	s := &signed{
		date:          date,
		signature:     params["Signature"],
		payloadHash:   payloadHash,
		signedHeaders: strings.Split(params["SignedHeaders"], ";"),
	}
	if err = v.parseCredential(params["Credential"], s, malformedHeader); err != nil {
		return nil, err
	}
	return s, nil
}

// authParams parses the Credential, SignedHeaders and Signature fields of a
// SigV4 Authorization header.
func authParams(auth string) (map[string]string, error) {
	fields, ok := strings.CutPrefix(auth, algorithm+" ")
	if !ok {
		return nil, s3api.ErrInvalidRequest("The authorization mechanism you have provided " +
			"is not supported. Please use " + algorithm + ".")
	}

	params := make(map[string]string, 3)
	for f := range strings.SplitSeq(fields, ",") {
		k, val, _ := strings.Cut(strings.TrimSpace(f), "=")
		params[k] = val
	}
	if params["Credential"] == "" || params["SignedHeaders"] == "" || params["Signature"] == "" {
		return nil, malformedHeader("The authorization header is malformed; it must contain " +
			"Credential, SignedHeaders and Signature.")
	}
	return params, nil
}

// checkPresignParams checks that a presigned URL carries every SigV4 query
// parameter.
func checkPresignParams(q url.Values) error {
	if q.Get("X-Amz-Algorithm") != algorithm {
		return malformedQuery("X-Amz-Algorithm only supports \"" + algorithm + "\"")
	}
	for _, name := range []string{
		"X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires", "X-Amz-SignedHeaders", "X-Amz-Signature",
	} {
		if q.Get(name) == "" {
			return malformedQuery("Query-string authentication version 4 requires the " +
				"X-Amz-Algorithm, X-Amz-Credential, X-Amz-Signature, X-Amz-Date, " +
				"X-Amz-SignedHeaders, and X-Amz-Expires parameters.")
		}
	}
	return nil
}

func (v *Verifier) parsePresigned(r *http.Request) (*signed, error) {
	q := r.URL.Query()
	if err := checkPresignParams(q); err != nil {
		return nil, err
	}

	date, err := time.Parse(amzDateFmt, q.Get("X-Amz-Date"))
	if err != nil {
		return nil, malformedQuery(
			"X-Amz-Date must be in the ISO8601 Long Format \"yyyyMMdd'T'HHmmss'Z'\"",
		)
	}
	expires, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || expires < 0 || expires > maxExpires {
		return nil, malformedQuery("X-Amz-Expires must be a number of seconds between 0 and 604800")
	}
	if v.now().After(date.Add(time.Duration(expires) * time.Second)) {
		return nil, accessDenied("Request has expired")
	}

	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = unsigned
	}

	s := &signed{
		date:          date,
		signature:     q.Get("X-Amz-Signature"),
		payloadHash:   payloadHash,
		signedHeaders: strings.Split(q.Get("X-Amz-SignedHeaders"), ";"),
		presigned:     true,
	}
	if err = v.parseCredential(q.Get("X-Amz-Credential"), s, malformedQuery); err != nil {
		return nil, err
	}
	return s, nil
}

// parseCredential fills s from a "<key>/<date>/<region>/<service>/aws4_request"
// credential, reporting a malformed one with malformed.
func (v *Verifier) parseCredential(cred string, s *signed, malformed func(string) error) error {
	parts := strings.Split(cred, "/")
	if len(parts) != 5 || parts[4] != scopeSuffix {
		return malformed("The credential is malformed; expecting " +
			"\"<YOUR-AKID>/YYYYMMDD/REGION/SERVICE/aws4_request\".")
	}
	if parts[0] != v.creds.AccessKeyID {
		return s3api.NewError(http.StatusForbidden, "InvalidAccessKeyId",
			"The AWS Access Key Id you provided does not exist in our records.")
	}
	if parts[1] != s.date.Format("20060102") {
		return malformed("The credential date does not match the request date.")
	}

	s.region, s.service = parts[2], parts[3]
	s.scope = strings.Join(parts[1:], "/")
	return nil
}

// checkSignedHeaders rejects a request that carries an x-amz-* header its
// signature does not cover, or that does not sign its Host at all, which is
// what S3 requires. Anyone holding a signed request or a presigned URL can
// change what the signature leaves out: x-amz-meta-* is the user metadata an
// upload stores, and the Host header chooses the bucket in
// virtual-hosted-style addressing.
//
// Headers S3 does not require signed are not required here either, so a
// presigned PUT can still be sent with a Content-Type, or with the system
// headers stored alongside the object, that its signature never covered. That
// is S3's boundary, and the README documents it.
//
// x-amz-content-sha256 is exempt: it is the declared payload hash, which the
// canonical request covers on a line of its own, so changing it changes the
// signature whether or not it is listed. Clients that presign with
// UNSIGNED-PAYLOAD and send the header anyway depend on that.
func checkSignedHeaders(r *http.Request, s *signed) error {
	if !isSigned(s, "host") {
		return accessDenied("The Host header must be signed")
	}
	for name := range r.Header {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-") || lower == "x-amz-content-sha256" {
			continue
		}
		if !isSigned(s, lower) {
			return accessDenied("There were headers present in the request which were not signed")
		}
	}
	return nil
}

// isSigned reports whether s lists name among its signed headers. S3 requires
// the list to be lowercase; a client that sends it otherwise still means the
// same header.
func isSigned(s *signed, name string) bool {
	return slices.ContainsFunc(s.signedHeaders, func(h string) bool {
		return strings.EqualFold(strings.TrimSpace(h), name)
	})
}

// signature computes the signature r should carry for the claims in s.
func (v *Verifier) signature(r *http.Request, s *signed) string {
	creq := strings.Join([]string{
		r.Method,
		canonicalURI(r.URL.Path),
		canonicalQuery(r.URL.Query(), s.presigned),
		canonicalHeaders(r, s.signedHeaders),
		strings.Join(s.signedHeaders, ";"),
		s.payloadHash,
	}, "\n")

	creqHash := sha256.Sum256([]byte(creq))
	toSign := strings.Join([]string{
		algorithm,
		s.date.Format(amzDateFmt),
		s.scope,
		hex.EncodeToString(creqHash[:]),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+v.creds.SecretAccessKey), s.date.Format("20060102"))
	key = hmacSHA256(key, s.region)
	key = hmacSHA256(key, s.service)
	key = hmacSHA256(key, scopeSuffix)

	return hex.EncodeToString(hmacSHA256(key, toSign))
}

func canonicalURI(p string) string {
	if p == "" {
		return "/"
	}
	return uriEncode(p, false)
}

// canonicalQuery sorts the query by name and then value, URI-encoding both.
// A presigned URL's own signature is not part of what it signs.
func canonicalQuery(q url.Values, presigned bool) string {
	var pairs []string
	for k, vals := range q {
		if presigned && k == "X-Amz-Signature" {
			continue
		}
		for _, val := range vals {
			pairs = append(pairs, uriEncode(k, true)+"="+uriEncode(val, true))
		}
	}
	slices.Sort(pairs)
	return strings.Join(pairs, "&")
}

// canonicalHeaders lists the signed headers as "name:value\n" lines, with
// values trimmed and inner runs of spaces collapsed.
//
// S3 requires the names to be lowercase, and they are lowercased here before
// anything is looked up: "Host" has to resolve to r.Host like "host" does,
// since net/http keeps the host out of r.Header and an empty value would bind
// the signature to no host at all. A client that signed a name in another
// case computed a different canonical request and fails the comparison, which
// is what S3 does with it too.
func canonicalHeaders(r *http.Request, names []string) string {
	var sb strings.Builder
	for _, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		var val string
		switch vals := r.Header.Values(name); {
		case name == "host":
			val = r.Host
		case name == "content-length" && len(vals) == 0 && r.ContentLength >= 0:
			// net/http may keep the length only in r.ContentLength.
			val = strconv.FormatInt(r.ContentLength, 10)
		default:
			for i, x := range vals {
				vals[i] = strings.Join(strings.Fields(x), " ")
			}
			val = strings.Join(vals, ",")
		}
		sb.WriteString(name)
		sb.WriteByte(':')
		sb.WriteString(val)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// uriEncode percent-encodes everything but the RFC 3986 unreserved characters,
// and slashes unless encodeSlash is set.
func uriEncode(s string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var sb strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~',
			c == '/' && !encodeSlash:
			sb.WriteByte(c)
		default:
			sb.WriteByte('%')
			sb.WriteByte(hexDigits[c>>4])
			sb.WriteByte(hexDigits[c&0xf])
		}
	}
	return sb.String()
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func isHexSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// payloadVerifier fails the read that reaches EOF when the body's SHA-256 does
// not match the declared one, so an upload reading it is not committed.
type payloadVerifier struct {
	body io.ReadCloser
	h    hash.Hash
	want string
}

func (p *payloadVerifier) Read(b []byte) (int, error) {
	n, err := p.body.Read(b)
	p.h.Write(b[:n])
	if errors.Is(err, io.EOF) && !strings.EqualFold(hex.EncodeToString(p.h.Sum(nil)), p.want) {
		return n, s3api.NewError(http.StatusBadRequest, "XAmzContentSHA256Mismatch",
			"The provided 'x-amz-content-sha256' header does not match what was computed.")
	}
	return n, err //nolint:wrapcheck // io.Reader contract: EOF must pass through unwrapped
}

func (p *payloadVerifier) Close() error {
	return p.body.Close() //nolint:wrapcheck // passes the body's close error through
}

func accessDenied(msg string) error {
	return s3api.NewError(http.StatusForbidden, "AccessDenied", msg)
}

func malformedHeader(msg string) error {
	return s3api.NewError(http.StatusBadRequest, "AuthorizationHeaderMalformed", msg)
}

func malformedQuery(msg string) error {
	return s3api.NewError(http.StatusBadRequest, "AuthorizationQueryParametersError", msg)
}
