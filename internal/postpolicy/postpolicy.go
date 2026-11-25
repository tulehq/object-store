// Package postpolicy reads S3's browser-based uploads, POST Object: a
// multipart/form-data form whose fields, up to the file, carry a POST policy
// signed with SigV4, and checks the form against the policy (S3 API
// Reference, "POST Object" and "POST Policy").
//
// The fields before the file are read and checked before any of the file
// is: the signature over the policy, the policy's expiration, each of its
// conditions, and that every field the form sends is covered by one. The
// file is then read through a reader that fails once it is outside the
// policy's content-length-range.
package postpolicy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tule.dev/sigv4"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// MaxFieldBytes bounds the bytes of the fields before the file, names and
// values together. S3 refuses fields that are too large with
// MaxPostPreDataLengthExceededError but documents no limit; this one is a
// choice, ample for a policy and the fields it covers.
const MaxFieldBytes = 64 << 10

// algorithm is the only x-amz-algorithm accepted: SigV4.
const algorithm = "AWS4-HMAC-SHA256"

// amzDate is the format of x-amz-date.
const amzDate = "20060102T150405Z"

// The fields the form must or may send, by lowercase name. S3 matches field
// names regardless of case: the AWS SDKs send X-Amz-Date, X-Amz-Credential
// and the like, and name them so in the policy's conditions.
const (
	fieldFile          = "file"
	fieldKey           = "key"
	fieldPolicy        = "policy"
	fieldContentType   = "content-type"
	fieldAlgorithm     = "x-amz-algorithm"
	fieldCredential    = "x-amz-credential" //nolint:gosec // G101: a field's name
	fieldDate          = "x-amz-date"
	fieldSignature     = "x-amz-signature"
	fieldSecurityToken = "x-amz-security-token" //nolint:gosec // G101: a field's name
	fieldSuccessStatus = "success_action_status"
	ignorePrefix       = "x-ignore-"
)

// supported are the fields this server acts on. Any other field the policy
// covers, such as acl, tagging, x-amz-meta-* or success_action_redirect, is
// refused with NotImplemented rather than dropped; x-ignore-* fields are
// ignored, as S3 ignores them.
var supported = map[string]bool{
	fieldKey: true, fieldPolicy: true, fieldContentType: true, fieldAlgorithm: true,
	fieldCredential: true, fieldDate: true, fieldSignature: true, fieldSecurityToken: true,
	fieldSuccessStatus: true,
}

// Upload is a POST Object whose form verified: the object to store, as the
// form names it, and who signed its policy.
type Upload struct {
	// Body is the file's content. Reading it fails with
	// contracts.ErrEntityTooLarge once it is longer than the policy's
	// content-length-range allows, and with contracts.ErrEntityTooSmall in
	// place of io.EOF when it ended shorter, so a write of it fails rather
	// than storing it.
	Body        io.Reader
	Bucket      string
	Key         string
	ContentType string
	// Principal is who the policy's credential belongs to.
	Principal sigv4.Principal
	// Status is the status a stored upload is answered with: 204, or 200
	// when success_action_status asks for it.
	Status int
}

type uploadKey struct{}

// NewContext returns ctx carrying up, for the handler that stores it.
func NewContext(ctx context.Context, up *Upload) context.Context {
	return context.WithValue(ctx, uploadKey{}, up)
}

// FromContext returns the Upload ctx carries, false when it carries none:
// the request is not a verified POST Object.
func FromContext(ctx context.Context) (*Upload, bool) {
	up, ok := ctx.Value(uploadKey{}).(*Upload)
	return up, ok
}

// KeyError is the error of the KeySource asked for the policy's signing
// key, which the caller maps as its signature verifier maps the same
// failure.
type KeyError struct{ Err error }

func (e *KeyError) Error() string { return "postpolicy: signing key: " + e.Err.Error() }
func (e *KeyError) Unwrap() error { return e.Err }

// Verify reads r, a POST Object to bucket, up to its file, and returns the
// upload it makes once the policy's signature verifies with the key keys
// gives for its credential, the policy has not expired at now, and the form
// meets every condition of it. A failure of keys is a *KeyError; any other
// is an S3 error.
func Verify(
	ctx context.Context,
	r *http.Request,
	bucket string,
	keys sigv4.KeySource,
	now time.Time,
) (*Upload, error) {
	f, err := readForm(r)
	if err != nil {
		return nil, err
	}
	key, err := f.verifySignature(ctx, keys)
	if err != nil {
		return nil, err
	}
	p, err := parsePolicy(f.fields[fieldPolicy])
	if err != nil {
		return nil, err
	}
	if now.After(p.expiration) {
		return nil, notAllowed("Policy expired.")
	}
	if err = p.check(f, bucket); err != nil {
		return nil, err
	}
	if err = f.checkSupported(); err != nil {
		return nil, err
	}

	return &Upload{
		Body:        &sizedReader{r: f.file, min: p.minSize, max: p.maxSize},
		Principal:   key.Principal,
		Bucket:      bucket,
		Key:         f.fields[fieldKey],
		ContentType: f.fields[fieldContentType],
		Status:      successStatus(f.fields[fieldSuccessStatus]),
	}, nil
}

// notAllowed is the error of a form its policy does not allow: 403
// AccessDenied with a message starting "Invalid according to Policy: ", a
// choice modeled on what S3 is reported to answer, not verified against
// it. S3's error code list describes the condition as 400
// InvalidPolicyDocument ("The content of the form does not meet the
// conditions specified in the policy document"), which is answered here to
// a policy that does not parse.
func notAllowed(reason string) *s3api.Error {
	return s3api.NewError(http.StatusForbidden, "AccessDenied",
		"Invalid according to Policy: "+reason)
}

// invalidPolicy is the error of a policy document that does not parse.
func invalidPolicy(reason string) *s3api.Error {
	return s3api.NewError(http.StatusBadRequest, "InvalidPolicyDocument",
		"Invalid Policy: "+reason)
}

// errMalformed is the error of a body that is not multipart/form-data.
func errMalformed() *s3api.Error {
	return s3api.NewError(http.StatusBadRequest, "MalformedPOSTRequest",
		"The body of your POST request is not well-formed multipart/form-data.")
}

// form is a POST Object's fields before its file, by lowercase name, and
// the file, open to be read.
type form struct {
	fields map[string]string
	file   *multipart.Part
}

// readForm reads r's fields up to its file part, which it leaves unread. A
// field ${filename} names in the key is expanded with the file's name, as S3
// expands it before checking the policy.
func readForm(r *http.Request) (*form, error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, s3api.NewError(http.StatusPreconditionFailed, "RequestIsNotMultiPartContent",
			"Bucket POST must be of the enclosure-type multipart/form-data.")
	}
	mr := multipart.NewReader(r.Body, params["boundary"])

	f := &form{fields: map[string]string{}}
	remaining := int64(MaxFieldBytes)
	for f.file == nil {
		part, pErr := mr.NextPart()
		if errors.Is(pErr, io.EOF) {
			return nil, s3api.NewError(http.StatusBadRequest,
				"IncorrectNumberOfFilesInPostRequest",
				"POST requires exactly one file upload per request.")
		}
		if pErr != nil {
			return nil, errMalformed()
		}
		if err = f.read(part, &remaining); err != nil {
			return nil, err
		}
	}

	key, ok := f.fields[fieldKey]
	if !ok {
		return nil, s3api.NewError(http.StatusBadRequest, "UserKeyMustBeSpecified",
			"The bucket POST request must contain the specified field name. If it is "+
				"specified, check the order of the fields.")
	}
	f.fields[fieldKey] = strings.ReplaceAll(key, "${filename}", f.file.FileName())
	return f, nil
}

// read reads part, a field or, by its name, the file, which it keeps
// unread, counting the field's bytes against remaining.
func (f *form) read(part *multipart.Part, remaining *int64) error {
	name := strings.ToLower(part.FormName())
	if name == "" {
		return errMalformed()
	}
	if name == fieldFile {
		f.file = part
		return nil
	}
	*remaining -= int64(len(name))
	value, err := io.ReadAll(io.LimitReader(part, *remaining+1))
	if err != nil {
		return errMalformed()
	}
	*remaining -= int64(len(value))
	if *remaining < 0 {
		return s3api.NewError(http.StatusBadRequest, "MaxPostPreDataLengthExceededError",
			"Your POST request fields preceding the upload file were too large.")
	}
	if _, dup := f.fields[name]; dup {
		// Which of two values S3 would check and which store is not
		// documented; refusing both is a choice.
		return s3api.ErrInvalidArgument(
			fmt.Sprintf("The POST form field %s appears more than once.", name))
	}
	f.fields[name] = string(value)
	return nil
}

// verifySignature returns the signing key of the form's credential, from
// keys, once the policy's signature verifies with it.
func (f *form) verifySignature(ctx context.Context, keys sigv4.KeySource) (sigv4.Key, error) {
	encoded, ok := f.fields[fieldPolicy]
	if !ok {
		// A form without a policy is anonymous, which only a publicly
		// writable bucket accepts ("POST Object", policy); there are none
		// here.
		return sigv4.Key{}, s3api.NewError(http.StatusForbidden, "AccessDenied", "Access Denied")
	}
	scope, err := f.scope()
	if err != nil {
		return sigv4.Key{}, err
	}
	key, err := keys.SigningKey(ctx, scope)
	if err != nil {
		return sigv4.Key{}, &KeyError{Err: err}
	}
	if key.Principal.AccountID == "" {
		return sigv4.Key{}, &KeyError{Err: errors.New("the key's principal has no account ID")}
	}
	if !signatureMatches(key.Signing, encoded, f.fields[fieldSignature]) {
		return sigv4.Key{}, s3api.NewError(http.StatusForbidden, "SignatureDoesNotMatch",
			"The request signature we calculated does not match the signature you provided. "+
				"Check your key and signing method.")
	}
	return key, nil
}

// scope returns the credential scope the form's x-amz-credential names,
// with its x-amz-security-token, once it checked that the form is signed
// with SigV4 for s3 on the day of its x-amz-date.
func (f *form) scope() (sigv4.Scope, error) {
	for _, name := range []string{fieldAlgorithm, fieldCredential, fieldDate, fieldSignature} {
		if f.fields[name] == "" {
			return sigv4.Scope{}, s3api.ErrInvalidArgument(
				fmt.Sprintf("Bucket POST must contain a field named '%s'.", name))
		}
	}
	if f.fields[fieldAlgorithm] != algorithm {
		return sigv4.Scope{}, s3api.ErrInvalidRequest(
			"Only the AWS4-HMAC-SHA256 x-amz-algorithm is supported.")
	}
	scope, err := parseCredential(f.fields[fieldCredential], f.fields[fieldDate])
	if err != nil {
		return sigv4.Scope{}, err
	}
	scope.SessionToken = f.fields[fieldSecurityToken]
	return scope, nil
}

// parseCredential returns the scope cred, an x-amz-credential, names, once
// it checked that it is scoped to s3 on the day of date, the x-amz-date.
func parseCredential(cred, date string) (sigv4.Scope, error) {
	// <access key ID>/<date>/<region>/<service>/aws4_request
	parts := strings.Split(cred, "/")
	if len(parts) != 5 || parts[0] == "" || parts[2] == "" || parts[4] != "aws4_request" {
		return sigv4.Scope{}, s3api.ErrInvalidArgument(
			"x-amz-credential must be <access key ID>/<date>/<region>/s3/aws4_request.")
	}
	if parts[3] != "s3" {
		return sigv4.Scope{}, s3api.ErrInvalidArgument(
			"x-amz-credential must be scoped to the s3 service.")
	}
	signed, err := time.Parse(amzDate, date)
	if err != nil {
		return sigv4.Scope{}, s3api.ErrInvalidArgument(
			"x-amz-date must be an ISO 8601 basic date-time, such as 20130728T000000Z.")
	}
	if signed.Format("20060102") != parts[1] {
		return sigv4.Scope{}, s3api.ErrInvalidArgument(
			"The date of x-amz-credential must be the day of x-amz-date.")
	}
	return sigv4.Scope{
		AccessKeyID: parts[0], Date: parts[1], Region: parts[2], Service: parts[3],
	}, nil
}

// checkSupported refuses a field this server does not act on (see
// supported), and success_action_status 201, whose XML document is not
// answered.
func (f *form) checkSupported() error {
	if f.fields[fieldSuccessStatus] == "201" {
		return s3api.ErrNotImplemented("success_action_status 201 is not supported.")
	}
	for name := range f.fields {
		if !supported[name] && !strings.HasPrefix(name, ignorePrefix) {
			return s3api.ErrNotImplemented(
				fmt.Sprintf("The POST form field %s is not supported.", name))
		}
	}
	return nil
}

// signatureMatches reports whether sig, hex, is the HMAC-SHA256 of the
// base64 policy as sent, keyed with the derived signing key ("POST Policy",
// and the AWS SDKs' PresignPostObject).
func signatureMatches(signing []byte, encoded, sig string) bool {
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, signing)
	mac.Write([]byte(encoded))
	return hmac.Equal(mac.Sum(nil), got)
}

// successStatus is the status a stored upload is answered with:
// success_action_status 200 asks for its own, and anything else, absent or
// not valid, is 204 ("POST Object", success_action_status). 201, which
// answers an XML document, is refused before (see checkSupported).
func successStatus(v string) int {
	if v == "200" {
		return http.StatusOK
	}
	return http.StatusNoContent
}

// policy is a decoded POST policy.
type policy struct {
	expiration time.Time
	conditions []condition
	// minSize and maxSize bound the file, from content-length-range; max
	// is -1 when no condition bounds it.
	minSize, maxSize int64
}

// condition is one condition of a policy: field (lowercase, without its
// "$") must equal value (eq) or start with it (starts-with). raw is the
// condition as the policy has it, for the error naming it.
type condition struct {
	op, field, value, raw string
}

// parsePolicy decodes encoded, the base64 policy field.
func parsePolicy(encoded string) (*policy, error) {
	doc, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, invalidPolicy("the policy is not base64.")
	}
	var raw struct {
		Expiration *string           `json:"expiration"`
		Conditions []json.RawMessage `json:"conditions"`
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	if err = dec.Decode(&raw); err != nil {
		return nil, invalidPolicy("the policy is not a JSON object.")
	}
	if raw.Expiration == nil {
		return nil, invalidPolicy("Policy missing expiration.")
	}
	if raw.Conditions == nil {
		return nil, invalidPolicy("Policy missing conditions.")
	}
	// ISO 8601 in GMT, such as 2013-08-01T12:00:00.000Z ("POST Policy",
	// Expiration); RFC 3339 parsing takes it with or without fractions.
	exp, err := time.Parse(time.RFC3339, *raw.Expiration)
	if err != nil {
		return nil, invalidPolicy("Invalid 'expiration' value: '" + *raw.Expiration + "'")
	}

	p := &policy{expiration: exp, maxSize: -1}
	for _, c := range raw.Conditions {
		if err = p.add(c); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// add adds c, one element of the conditions array: an object of exact
// matches, {"field": "value"}, or an array, ["eq" or "starts-with",
// "$field", "value"] or ["content-length-range", min, max].
func (p *policy) add(c json.RawMessage) error {
	var obj map[string]any
	if err := json.Unmarshal(c, &obj); err == nil {
		return p.addExact(obj, string(c))
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(c, &arr); err != nil || len(arr) != 3 {
		return invalidPolicy("a condition is neither an object nor an array of three.")
	}
	var op string
	if err := json.Unmarshal(arr[0], &op); err != nil {
		return invalidPolicy("a condition's operator is not a string.")
	}
	switch op = strings.ToLower(op); op {
	case "content-length-range":
		return p.addRange(arr[1], arr[2])
	case "eq", "starts-with":
		return p.addMatch(op, arr[1], arr[2], string(c))
	default:
		return invalidPolicy("unknown condition operator " + op + ".")
	}
}

// addExact adds obj's exact matches; raw is the condition as the policy has
// it.
func (p *policy) addExact(obj map[string]any, raw string) error {
	if len(obj) == 0 {
		return invalidPolicy("an empty condition object.")
	}
	for field, v := range obj {
		s, ok := v.(string)
		if !ok {
			return invalidPolicy("the condition on " + field + " is not a string.")
		}
		p.conditions = append(p.conditions, condition{
			op: "eq", field: strings.ToLower(field), value: s, raw: raw,
		})
	}
	return nil
}

// addRange narrows the file's size range to content-length-range's bounds,
// a and b.
func (p *policy) addRange(a, b json.RawMessage) error {
	lo, errA := strconv.ParseInt(string(a), 10, 64)
	hi, errB := strconv.ParseInt(string(b), 10, 64)
	if errA != nil || errB != nil || lo < 0 || hi < lo {
		return invalidPolicy("content-length-range takes two non-negative integers.")
	}
	p.minSize = max(p.minSize, lo)
	if p.maxSize < 0 || hi < p.maxSize {
		p.maxSize = hi
	}
	return nil
}

// addMatch adds the condition op, eq or starts-with, on the field a names,
// "$field", with the value b; raw is the condition as the policy has it.
func (p *policy) addMatch(op string, a, b json.RawMessage, raw string) error {
	var field, value string
	if json.Unmarshal(a, &field) != nil || json.Unmarshal(b, &value) != nil {
		return invalidPolicy("the " + op + " condition takes two strings.")
	}
	name, ok := strings.CutPrefix(field, "$")
	if !ok || name == "" {
		return invalidPolicy("the field of a condition must start with $.")
	}
	p.conditions = append(p.conditions, condition{
		op: op, field: strings.ToLower(name), value: value, raw: raw,
	})
	return nil
}

// check checks f, a form posted to bucket, against p's conditions, and
// that each field f sends is covered by one, except x-amz-signature, file,
// policy and x-ignore-* fields ("POST Policy", Conditions).
func (p *policy) check(f *form, bucket string) error {
	covered := map[string]bool{}
	for _, c := range p.conditions {
		covered[c.field] = true
		// bucket is matched against the bucket the form is posted to.
		v := f.fields[c.field]
		if c.field == "bucket" {
			v = bucket
		}
		if !c.holds(v) {
			return notAllowed("Policy Condition failed: " + c.raw)
		}
	}
	for name := range f.fields {
		if name == fieldSignature || name == fieldPolicy || strings.HasPrefix(name, ignorePrefix) {
			continue
		}
		if !covered[name] {
			return notAllowed("Extra input fields: " + name)
		}
	}
	return nil
}

// holds reports whether v, a field's value ("" when the form does not send
// it), meets c. A starts-with on Content-Type holds of a comma-separated
// list when each of its values does ("POST Policy", Condition Matching).
func (c condition) holds(v string) bool {
	if c.op == "eq" {
		return v == c.value
	}
	if c.field != fieldContentType {
		return strings.HasPrefix(v, c.value)
	}
	for item := range strings.SplitSeq(v, ",") {
		if !strings.HasPrefix(strings.TrimSpace(item), c.value) {
			return false
		}
	}
	return true
}

// sizedReader reads a file that must be min to max bytes long, max -1 for
// no bound. A read error of the form other than its end is the
// MalformedPOSTRequest of a body cut short.
type sizedReader struct {
	r        io.Reader
	n        int64
	min, max int64
}

func (s *sizedReader) Read(b []byte) (int, error) {
	n, err := s.r.Read(b)
	s.n += int64(n)
	if s.max >= 0 && s.n > s.max {
		return 0, contracts.ErrEntityTooLarge
	}
	switch {
	case errors.Is(err, io.EOF):
		if s.n < s.min {
			return n, contracts.ErrEntityTooSmall
		}
		return n, io.EOF
	case err != nil:
		return n, fmt.Errorf("postpolicy: read the file: %w", s3api.NewError(
			http.StatusBadRequest, "MalformedPOSTRequest",
			"The body of your POST request is not well-formed multipart/form-data."))
	}
	return n, nil
}
