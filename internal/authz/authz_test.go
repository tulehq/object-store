package authz

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gasmod/gas/storage"
	"tule.dev/sigv4"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

const (
	userARN    = "arn:aws:iam::000000000000:user/team/dev"
	sessionARN = "arn:aws:sts::111111111111:assumed-role/Reader/e2e"
	roleARN    = "arn:aws:iam::111111111111:role/ops/Reader"
)

// buckets is a PolicySource holding a policy document by bucket; a bucket
// missing from it does not exist, and one mapped to "" has no policy.
type buckets map[string]string

// Every bucket is of generation "g1".
func (b buckets) GetBucketPolicy(_ context.Context, bucket string) (string, []byte, error) {
	doc, ok := b[bucket]
	switch {
	case !ok:
		return "", nil, storage.ErrBucketNotFound
	case doc == "":
		return "g1", nil, contracts.ErrNoSuchBucketPolicy
	}
	return "g1", []byte(doc), nil
}

func (b buckets) Pin(
	ctx context.Context,
	refs ...contracts.BucketRef,
) (context.Context, func(), error) {
	for _, r := range refs {
		if _, ok := b[r.Name]; !ok || r.Generation != "g1" {
			return nil, nil, storage.ErrBucketNotFound
		}
	}
	return ctx, func() {}, nil
}

// newAuthorizer returns an Authorizer for the account 000000000000 whose
// buckets b and src and dst have the policies in policies (none by
// default), and any others policies names.
func newAuthorizer(policies buckets) *Authorizer {
	all := buckets{"b": "", "src": "", "dst": ""}
	for k, v := range policies {
		all[k] = v
	}
	return New("000000000000", all)
}

// request returns a GET of target signed by p, as Verify leaves it.
func request(p sigv4.Principal, target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, http.NoBody)
	r.RemoteAddr = "203.0.113.7:4321"
	return r.WithContext(sigv4.NewContext(r.Context(), p))
}

func user(docs ...string) sigv4.Principal {
	p := sigv4.Principal{
		AccountID: "000000000000", ARN: userARN, PrincipalARN: userARN,
		UserID: "AIDAUSEREXAMPLE00001", Type: sigv4.PrincipalTypeUser, AccessKeyID: "AKIAUSER",
	}
	for _, d := range docs {
		p.Policies = append(p.Policies, json.RawMessage(d))
	}
	return p
}

func allow(action, resource string) string {
	return `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"` + action +
		`","Resource":"` + resource + `"}}`
}

func requireDenied(t *testing.T, err error, message string) {
	t.Helper()
	var s3Err *s3api.Error
	if !errors.As(err, &s3Err) {
		t.Fatalf("got %v, want AccessDenied", err)
	}
	if s3Err.Status != http.StatusForbidden || s3Err.Code != "AccessDenied" ||
		s3Err.Message != message {
		t.Fatalf("got %d %s %q, want 403 AccessDenied %q",
			s3Err.Status, s3Err.Code, s3Err.Message, message)
	}
}

var obj = s3api.Target{Bucket: "b", Key: "dir/k.txt"}

func TestRootIsAlwaysAllowed(t *testing.T) {
	a := newAuthorizer(nil)
	r := request(rootOf("000000000000"), "/b/dir/k.txt")
	if err := a.check(r, DeleteObject, obj); err != nil {
		t.Fatalf("root: %v", err)
	}
}

func TestAPrincipalWithNoPolicyIsDenied(t *testing.T) {
	err := newAuthorizer(nil).check(request(user(), "/b/dir/k.txt"), GetObject, obj)
	requireDenied(t, err, `User: `+userARN+` is not authorized to perform: s3:GetObject `+
		`on resource: "arn:aws:s3:::b/dir/k.txt" because no identity-based policy allows `+
		`the s3:GetObject action`)
}

func TestAnActionWithoutAResourceTypeNamesNone(t *testing.T) {
	err := newAuthorizer(nil).check(request(user(), "/"), ListBuckets, s3api.Target{})
	requireDenied(t, err, `User: `+userARN+` is not authorized to perform: `+
		`s3:ListAllMyBuckets because no identity-based policy allows the `+
		`s3:ListAllMyBuckets action`)
}

func TestAnAllowAllowsOnlyItsActionAndResource(t *testing.T) {
	a := newAuthorizer(nil)
	p := user(allow("s3:GetObject", "arn:aws:s3:::b/dir/*"))
	if err := a.check(request(p, "/b/dir/k.txt"), GetObject, obj); err != nil {
		t.Fatalf("allowed get: %v", err)
	}
	if err := a.check(request(p, "/b/dir/k.txt"), HeadObject, obj); err != nil {
		t.Fatalf("head is s3:GetObject: %v", err)
	}
	other := s3api.Target{Bucket: "b", Key: "other"}
	requireDenied(t, a.check(request(p, "/b/other"), GetObject, other),
		`User: `+userARN+` is not authorized to perform: s3:GetObject on resource: `+
			`"arn:aws:s3:::b/other" because no identity-based policy allows the s3:GetObject action`)
	if err := a.check(request(p, "/b/dir/k.txt"), PutObject, obj); err == nil {
		t.Fatal("put allowed by a get policy")
	}
}

func TestAnExplicitDenyWins(t *testing.T) {
	p := user(allow("s3:*", "*"),
		`{"Version":"2012-10-17","Statement":{"Effect":"Deny","Action":"s3:DeleteObject",`+
			`"Resource":"*"}}`)
	err := newAuthorizer(nil).check(request(p, "/b/dir/k.txt"), DeleteObject, obj)
	requireDenied(t, err, `User: `+userARN+` is not authorized to perform: s3:DeleteObject `+
		`on resource: "arn:aws:s3:::b/dir/k.txt" with an explicit deny in an identity-based policy`)
}

// CopyObject needs s3:PutObject on the destination, then s3:GetObject on
// the source.
func TestCopyObjectChecksBothResources(t *testing.T) {
	a := newAuthorizer(nil)
	src := s3api.Target{Bucket: "src", Key: "a"}
	dst := s3api.Target{Bucket: "dst", Key: "b"}
	both := user(allow("s3:PutObject", "arn:aws:s3:::dst/*"),
		allow("s3:GetObject", "arn:aws:s3:::src/*"))
	if err := a.check(request(both, "/dst/b"), CopyObject, dst, src); err != nil {
		t.Fatalf("copy: %v", err)
	}
	putOnly := user(allow("s3:PutObject", "arn:aws:s3:::dst/*"))
	requireDenied(t, a.check(request(putOnly, "/dst/b"), CopyObject, dst, src),
		`User: `+userARN+` is not authorized to perform: s3:GetObject on resource: `+
			`"arn:aws:s3:::src/a" because no identity-based policy allows the s3:GetObject action`)
	getOnly := user(allow("s3:GetObject", "arn:aws:s3:::src/*"))
	requireDenied(t, a.check(request(getOnly, "/dst/b"), CopyObject, dst, src),
		`User: `+userARN+` is not authorized to perform: s3:PutObject on resource: `+
			`"arn:aws:s3:::dst/b" because no identity-based policy allows the s3:PutObject action`)
}

func TestAHandlerMistakeFailsClosed(t *testing.T) {
	a := newAuthorizer(nil)
	root := request(rootOf("000000000000"), "/")
	if err := a.check(root, Operation("PutBucketAcl"), obj); err == nil {
		t.Error("an operation without an entry was allowed")
	}
	if err := a.check(root, CopyObject, obj); err == nil {
		t.Error("CopyObject with one target was allowed")
	}
	unverified := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	err := a.check(unverified, ListBuckets, s3api.Target{})
	if !errors.Is(err, errNoPrincipal) {
		t.Errorf("an unverified request: %v", err)
	}
}

func TestAPolicyThatDoesNotParseIsAnInternalError(t *testing.T) {
	err := newAuthorizer(
		nil,
	).check(request(user(`{"Statement":[]}`), "/"), ListBuckets, s3api.Target{})
	var s3Err *s3api.Error
	if err == nil || errors.As(err, &s3Err) {
		t.Fatalf("got %v, want an internal error", err)
	}
}

func TestParsedPoliciesAreCachedAndBounded(t *testing.T) {
	a := newAuthorizer(nil)
	a.limit = 2
	doc := allow("s3:GetObject", "*")
	for range 3 {
		if err := a.check(request(user(doc), "/b/k"), GetObject, obj); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.cache) != 1 {
		t.Fatalf("cache holds %d documents, want 1", len(a.cache))
	}
	for _, res := range []string{"arn:aws:s3:::x/*", "arn:aws:s3:::y/*", "arn:aws:s3:::z/*"} {
		if err := a.check(request(user(allow("s3:GetObject", res)), "/b/k"),
			GetObject, obj); err == nil {
			t.Fatalf("%s allowed b/dir/k.txt", res)
		}
	}
	if len(a.cache) != 2 {
		t.Fatalf("cache holds %d documents, want its limit of 2", len(a.cache))
	}
}

func TestGlobalKeys(t *testing.T) {
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	r := request(user(), "/")
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("X-Forwarded-For", "198.51.100.1")

	p := user()
	p.Tags = map[string]string{"team": "storage"}
	got := globalKeys(r, p, now)
	want := map[string][]string{
		"aws:SourceIp":          {"203.0.113.7"},
		"aws:CurrentTime":       {"2026-11-13T10:00:00Z"},
		"aws:EpochTime":         {"1794564000"},
		"aws:SecureTransport":   {"true"},
		"aws:RequestedRegion":   {"us-east-1"},
		"aws:PrincipalTag/team": {"storage"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("user keys = %v\nwant %v", got, want)
	}
}

// requireKeys checks that keys holds each of want's keys with its one
// value, and none of want's keys valued "".
func requireKeys(t *testing.T, what string, keys map[string][]string, want map[string]string) {
	t.Helper()
	for k, v := range want {
		got := keys[k]
		if v == "" && got != nil || v != "" && (len(got) != 1 || got[0] != v) {
			t.Errorf("%s: %s = %v, want %q", what, k, got, v)
		}
	}
}

func TestServiceKeysFollowTheAction(t *testing.T) {
	c, err := newAuthorizer(nil).Caller(request(user(), "/b?list-type=2&prefix=logs/&max-keys=5"))
	if err != nil {
		t.Fatal(err)
	}
	c.req.Header.Set("X-Amz-Acl", "private")
	list, err := c.context(check{action: "s3:ListBucket"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireKeys(t, "ListBucket", list, map[string]string{
		"s3:prefix": "logs/", "s3:max-keys": "5", "s3:delimiter": "", "s3:x-amz-acl": "",
		"s3:authType": "REST-HEADER", "s3:signatureversion": "AWS4-HMAC-SHA256",
	})
	put, err := c.context(check{action: "s3:PutObject"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireKeys(t, "PutObject", put, map[string]string{"s3:x-amz-acl": "private", "s3:prefix": ""})
}

func TestAConditionOnAServiceKeyIsHonored(t *testing.T) {
	a := newAuthorizer(nil)
	p := user(`{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"s3:ListBucket",` +
		`"Resource":"arn:aws:s3:::b","Condition":{"StringLike":{"s3:prefix":"home/${aws:username}/*"}}}}`)
	b := s3api.Target{Bucket: "b"}
	if err := a.check(
		request(p, "/b?list-type=2&prefix=home/dev/x"),
		ListObjectsV2,
		b,
	); err != nil {
		t.Fatalf("own prefix: %v", err)
	}
	if err := a.check(
		request(p, "/b?list-type=2&prefix=home/other/"),
		ListObjectsV2,
		b,
	); err == nil {
		t.Fatal("another prefix allowed")
	}
	if err := a.check(request(p, "/b?list-type=2"), ListObjectsV2, b); err == nil {
		t.Fatal("no prefix allowed")
	}
}

func TestAPresignedURLIsQueryStringAuth(t *testing.T) {
	r := request(user(), "/b/k?X-Amz-Algorithm=AWS4-HMAC-SHA256")
	if v, _ := authType(r); v != "REST-QUERY-STRING" {
		t.Fatalf("authType = %q", v)
	}
}

func TestAnOverlongContextValueIsRefused(t *testing.T) {
	r := request(user(allow("s3:*", "*")), "/b?prefix="+strings.Repeat("a", maxContextValue+1))
	var s3Err *s3api.Error
	err := newAuthorizer(nil).check(r, ListObjects, s3api.Target{Bucket: "b"})
	if !errors.As(err, &s3Err) || s3Err.Code != "InvalidArgument" {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
	root := request(
		rootOf("000000000000"),
		"/b?prefix="+strings.Repeat("a", maxContextValue+1),
	)
	if err = newAuthorizer(
		nil,
	).check(root, ListObjects, s3api.Target{Bucket: "b"}); err != nil {
		t.Fatalf("root is not evaluated: %v", err)
	}
}

// A principal of another account needs an Allow in its identity policies
// and in the bucket's policy; a deny or a missing allow in either is
// reported as such. This replaces the refusal of every cross-account
// request before bucket policies were evaluated.
func TestAnotherAccountNeedsBothPolicies(t *testing.T) {
	p := session(allow("s3:*", "*"),
		`{"Version":"2012-10-17","Statement":{"Effect":"Deny","Action":"s3:PutObject",`+
			`"Resource":"*"}}`)
	a := newAuthorizer(nil)
	requireDenied(t, a.check(request(p, "/b/dir/k.txt"), GetObject, obj),
		`User: `+sessionARN+` is not authorized to perform: s3:GetObject on resource: `+
			`"arn:aws:s3:::b/dir/k.txt" because no resource-based policy allows the s3:GetObject action`)
	requireDenied(t, a.check(request(p, "/b/dir/k.txt"), PutObject, obj),
		`User: `+sessionARN+` is not authorized to perform: s3:PutObject on resource: `+
			`"arn:aws:s3:::b/dir/k.txt" with an explicit deny in an identity-based policy`)

	granted := newAuthorizer(
		buckets{"b": grant(`"111111111111"`, "s3:GetObject", "arn:aws:s3:::b/*")},
	)
	if err := granted.check(request(p, "/b/dir/k.txt"), GetObject, obj); err != nil {
		t.Fatalf("granted by both: %v", err)
	}
	requireDenied(t, granted.check(request(session(), "/b/dir/k.txt"), GetObject, obj),
		`User: `+sessionARN+` is not authorized to perform: s3:GetObject on resource: `+
			`"arn:aws:s3:::b/dir/k.txt" because no identity-based policy allows the s3:GetObject action`)

	// Another account's root is not root here: it has its account's
	// identity allow, so the bucket policy decides.
	root := rootOf("111111111111")
	requireDenied(t, a.check(request(root, "/b/dir/k.txt"), GetObject, obj),
		`User: arn:aws:iam::111111111111:root is not authorized to perform: s3:GetObject `+
			`on resource: "arn:aws:s3:::b/dir/k.txt" because no resource-based policy allows `+
			`the s3:GetObject action`)
	if err := granted.check(request(root, "/b/dir/k.txt"), GetObject, obj); err != nil {
		t.Fatalf("another account's root, granted: %v", err)
	}
	// The server's own account's root is never evaluated.
	own := rootOf("000000000000")
	deny := newAuthorizer(buckets{"b": `{"Version":"2012-10-17","Statement":{"Effect":"Deny",` +
		`"Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/*"}}`})
	if err := deny.check(request(own, "/b/dir/k.txt"), GetObject, obj); err != nil {
		t.Fatalf("root: %v", err)
	}
}

// s3:x-amz-copy-source is the source the copy reads, decoded, so a client
// cannot step around a condition by encoding the header differently.
func TestCopySourceIsTheDecodedSource(t *testing.T) {
	a := newAuthorizer(nil)
	p := user(allow("s3:GetObject", "*"),
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:PutObject",`+
			`"Resource":"*"},{"Effect":"Deny","Action":"s3:PutObject","Resource":"*",`+
			`"Condition":{"StringLike":{"s3:x-amz-copy-source":"src/private/*"}}}]}`)
	dst := s3api.Target{Bucket: "dst", Key: "b"}
	for _, header := range []string{"src/private/a", "/src/private/a", "src/%70rivate/a"} {
		r := request(p, "/dst/b")
		r.Header.Set("X-Amz-Copy-Source", header)
		err := a.check(r, CopyObject, dst, s3api.Target{Bucket: "src", Key: "private/a"})
		requireDenied(t, err, `User: `+userARN+` is not authorized to perform: s3:PutObject `+
			`on resource: "arn:aws:s3:::dst/b" with an explicit deny in an identity-based policy`)
	}
	r := request(p, "/dst/b")
	r.Header.Set("X-Amz-Copy-Source", "src/private/a")
	if err := a.check(
		r,
		CopyObject,
		dst,
		s3api.Target{Bucket: "src", Key: "public/a"},
	); err != nil {
		t.Fatalf("copy of public/a: %v", err)
	}
}

// The BlockNonConditionalObjectCreation statement of the S3 User Guide's
// "Enforce conditional writes" (Example 2), as an identity policy: writes
// that create an object need If-Match; the multipart steps that create none
// are exempt.
func TestObjectCreationOperation(t *testing.T) {
	a := newAuthorizer(nil)
	p := user(allow("s3:PutObject", "*"), `{"Version":"2012-10-17","Statement":[{`+
		`"Sid":"BlockNonConditionalObjectCreation","Effect":"Deny","Action":"s3:PutObject",`+
		`"Resource":"arn:aws:s3:::b/*","Condition":{"Null":{"s3:if-match":"true"},`+
		`"Bool":{"s3:ObjectCreationOperation":"true"}}}]}`)
	explicit := `User: ` + userARN + ` is not authorized to perform: s3:PutObject on resource: ` +
		`"arn:aws:s3:::b/dir/k.txt" with an explicit deny in an identity-based policy`
	src := s3api.Target{Bucket: "b", Key: "src"}
	for op, denied := range map[Operation]bool{
		PutObject: true, CopyObject: true, CompleteMultipartUpload: true,
		CreateMultipartUpload: false, UploadPart: false,
	} {
		targets := []s3api.Target{obj}
		if op == CopyObject {
			targets = append(targets, src)
		}
		// GetObject on the source is not allowed, so a copy that passes
		// the Deny stops there instead.
		err := a.check(request(p, "/b/dir/k.txt"), op, targets...)
		if denied {
			requireDenied(t, err, explicit)
			conditional := request(p, "/b/dir/k.txt")
			conditional.Header.Set("If-Match", `"etag"`)
			err = a.check(conditional, op, targets...)
			if op == CopyObject {
				requireDenied(t, err, notAllowedMsg("s3:GetObject", "arn:aws:s3:::b/src"))
			} else if err != nil {
				t.Errorf("%s with If-Match: %v", op, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", op, err)
		}
	}
}

func notAllowedMsg(action, resource string) string {
	return `User: ` + userARN + ` is not authorized to perform: ` + action + ` on resource: "` +
		resource + `" because no identity-based policy allows the ` + action + ` action`
}

// A read that finds no object answers NoSuchKey only to a caller allowed
// s3:ListBucket on the bucket, as the GetObject and HeadObject references
// say, and the read's ?prefix plays no part in that.
func TestNotFoundNeedsListBucket(t *testing.T) {
	a := newAuthorizer(nil)
	missing := fmt.Errorf("get: %w", storage.ErrObjectNotFound)
	reader := user(allow("s3:GetObject", "*"))
	requireDenied(t, a.notFound(request(reader, "/b/k"), missing),
		notAllowedMsg("s3:ListBucket", "arn:aws:s3:::b"))

	lister := user(allow("s3:GetObject", "*"), allow("s3:ListBucket", "arn:aws:s3:::b"))
	if err := a.notFound(request(lister, "/b/k"), missing); !errors.Is(err, missing) {
		t.Fatalf("lister: %v", err)
	}

	prefixed := user(`{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":` +
		`"s3:ListBucket","Resource":"*","Condition":{"StringLike":{"s3:prefix":"public/*"}}}}`)
	err := a.notFound(request(prefixed, "/b/private/k?prefix=public/"), missing)
	requireDenied(t, err, notAllowedMsg("s3:ListBucket", "arn:aws:s3:::b"))

	root := rootOf("000000000000")
	if err = a.notFound(request(root, "/b/k"), missing); !errors.Is(err, missing) {
		t.Fatalf("root: %v", err)
	}
	other := errors.New("other")
	if err = a.notFound(request(reader, "/b/k"), other); !errors.Is(err, other) {
		t.Fatalf("another error: %v", err)
	}
}

func TestOverlongNamesAreRefusedBeforeEvaluation(t *testing.T) {
	a := newAuthorizer(nil)
	for _, p := range []sigv4.Principal{
		user(allow("s3:*", "*")), rootOf("000000000000"),
	} {
		long := s3api.Target{Bucket: "b", Key: strings.Repeat("k", maxKey+1)}
		var s3Err *s3api.Error
		err := a.check(request(p, "/"), GetObject, long)
		if !errors.As(err, &s3Err) || s3Err.Code != "KeyTooLongError" || s3Err.Status != 400 {
			t.Errorf("long key: %v", err)
		}
		err = a.check(request(p, "/"), CopyObject, obj, long)
		if !errors.As(err, &s3Err) || s3Err.Code != "KeyTooLongError" {
			t.Errorf("long copy source: %v", err)
		}
		err = a.check(request(p, "/"), ListObjects,
			s3api.Target{Bucket: strings.Repeat("b", maxBucketName+1)})
		if !errors.As(err, &s3Err) || s3Err.Code != "InvalidBucketName" {
			t.Errorf("long bucket: %v", err)
		}
		ok := s3api.Target{
			Bucket: strings.Repeat("b", maxBucketName),
			Key:    strings.Repeat("k", maxKey),
		}
		if err = a.check(request(p, "/"), GetObject, ok); err != nil {
			t.Errorf("names at the limits: %v", err)
		}
	}
}

// aws:RequestedRegion is the server's region whatever region the request
// was signed for, so a Deny on us-east-1 cannot be stepped around.
func TestRequestedRegionIsTheServers(t *testing.T) {
	p := user(allow("s3:*", "*"), `{"Version":"2012-10-17","Statement":{"Effect":"Deny",`+
		`"Action":"s3:*","Resource":"*","Condition":{"StringEquals":`+
		`{"aws:RequestedRegion":"us-east-1"}}}}`)
	r := request(p, "/b/dir/k.txt")
	r.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=AKIAUSER/20261113/eu-west-1/s3/aws4_request",
	)
	r = r.WithContext(sigv4.NewContext(r.Context(), p))
	err := newAuthorizer(nil).check(r, GetObject, obj)
	requireDenied(t, err, `User: `+userARN+` is not authorized to perform: s3:GetObject `+
		`on resource: "arn:aws:s3:::b/dir/k.txt" with an explicit deny in an identity-based policy`)
}

// An empty X-Amz-Algorithm is not a presigned URL to Verify, nor here.
func TestAnEmptyAlgorithmIsHeaderAuth(t *testing.T) {
	if v, _ := authType(request(user(), "/b/k?X-Amz-Algorithm=")); v != "REST-HEADER" {
		t.Fatalf("authType = %q", v)
	}
}

// aws:PrincipalOrgID and aws:PrincipalOrgPaths are the organization
// identity reports, absent for an account in no organization, and unknown
// when identity could not tell: then a negated condition on them neither
// grants nor lets a Deny lapse, and a positive one does not grant.
func TestOrganizationKeys(t *testing.T) {
	const (
		org  = "o-a1b2c3d4e5"
		path = "o-a1b2c3d4e5/r-ab12/ou-ab12-11111111/"
	)
	member := func(p sigv4.Principal) sigv4.Principal {
		p.OrgID, p.OrgPaths = org, []string{path}
		return p
	}
	unknown := func(p sigv4.Principal) sigv4.Principal {
		p.OrgUnknown = true
		return p
	}
	inOrg := grant(`"*"`, "s3:GetObject", "arn:aws:s3:::b/*")
	inOrg = strings.Replace(inOrg, `"Resource"`,
		`"Condition":{"StringEquals":{"aws:PrincipalOrgID":"`+org+`"},`+
			`"ForAnyValue:StringLike":{"aws:PrincipalOrgPaths":"`+org+`/r-ab12/*"}},"Resource"`, 1)
	outside := `{"Version":"2012-10-17","Statement":[` +
		`{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::src/*"},` +
		`{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::src/*",` +
		`"Condition":{"StringNotEquals":{"aws:PrincipalOrgID":"` + org + `"}}}]}`
	notIn := `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Principal":"*",` +
		`"Action":"s3:GetObject","Resource":"arn:aws:s3:::dst/*",` +
		`"Condition":{"StringNotEquals":{"aws:PrincipalOrgID":"o-zzzzzzzzzz"}}}}`
	a := newAuthorizer(buckets{"b": inOrg, "src": outside, "dst": notIn})
	src := s3api.Target{Bucket: "src", Key: obj.Key}
	dst := s3api.Target{Bucket: "dst", Key: obj.Key}

	for _, tc := range []struct {
		target s3api.Target
		name   string
		p      sigv4.Principal
		allow  bool
	}{
		{name: "a member, positive", p: member(user()), target: obj, allow: true},
		{name: "no organization, positive", p: user(), target: obj},
		{name: "unknown, positive", p: unknown(user()), target: obj},
		{name: "a member, negated deny", p: member(user()), target: src, allow: true},
		{name: "no organization, negated deny", p: user(), target: src},
		{name: "unknown, negated deny", p: unknown(user()), target: src},
		{name: "a member, negated allow", p: member(user()), target: dst, allow: true},
		{name: "no organization, negated allow", p: user(), target: dst, allow: true},
		{name: "unknown, negated allow", p: unknown(user()), target: dst},
	} {
		err := a.check(request(tc.p, "/"+tc.target.Bucket+"/"+tc.target.Key), GetObject, tc.target)
		if tc.allow != (err == nil) {
			t.Errorf("%s: got %v, want allowed %t", tc.name, err, tc.allow)
		}
	}

	c, err := a.Caller(request(unknown(user()), "/"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.unknown, []string{orgIDKey, orgPathsKey}) {
		t.Fatalf("unknown = %v", c.unknown)
	}
	if _, ok := c.keys[orgIDKey]; ok {
		t.Fatal("an unknown organization ID is in the context")
	}
}
