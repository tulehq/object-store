package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"tule.dev/sigv4"

	"tule.dev/object-store/internal/s3api"
)

// session is a session of the role ops/Reader of account 111111111111.
func session(docs ...string) sigv4.Principal {
	p := sigv4.Principal{
		AccountID: "111111111111", ARN: sessionARN, PrincipalARN: roleARN,
		UserID: "AROAROLEEXAMPLE00001:e2e", Type: sigv4.PrincipalTypeAssumedRole,
		AccessKeyID: "ASIASESSION",
	}
	for _, d := range docs {
		p.Policies = append(p.Policies, json.RawMessage(d))
	}
	return p
}

// grant is a bucket policy allowing principal, an AWS principal value as
// JSON, action on resource.
func grant(principal, action, resource string) string {
	return `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Principal":{"AWS":` +
		principal + `},"Action":"` + action + `","Resource":"` + resource + `"}}`
}

func deniedMsg(arn, action, resource, reason string) string {
	return `User: ` + arn + ` is not authorized to perform: ` + action + ` on resource: "` +
		resource + `" ` + reason
}

// Within one account, a bucket policy naming the IAM user grants on its own;
// one naming only the account delegates to the identity policies.
func TestABucketPolicyWithinTheAccount(t *testing.T) {
	direct := newAuthorizer(
		buckets{"b": grant(`"`+userARN+`"`, "s3:GetObject", "arn:aws:s3:::b/*")},
	)
	if err := direct.check(request(user(), "/b/dir/k.txt"), GetObject, obj); err != nil {
		t.Fatalf("a grant naming the IAM user: %v", err)
	}
	account := newAuthorizer(
		buckets{"b": grant(`"000000000000"`, "s3:GetObject", "arn:aws:s3:::b/*")},
	)
	requireDenied(t, account.check(request(user(), "/b/dir/k.txt"), GetObject, obj),
		deniedMsg(userARN, "s3:GetObject", "arn:aws:s3:::b/dir/k.txt",
			"because no identity-based policy allows the s3:GetObject action"))
	if err := account.check(request(user(allow("s3:GetObject", "*")), "/b/dir/k.txt"),
		GetObject, obj); err != nil {
		t.Fatalf("an account grant with an identity allow: %v", err)
	}
}

func TestAnExplicitDenyInTheBucketPolicyWins(t *testing.T) {
	a := newAuthorizer(buckets{"b": `{"Version":"2012-10-17","Statement":{"Effect":"Deny",` +
		`"Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}}`})
	requireDenied(t, a.check(request(user(allow("s3:*", "*")), "/b/dir/k.txt"), GetObject, obj),
		deniedMsg(userARN, "s3:GetObject", "arn:aws:s3:::b/dir/k.txt",
			"with an explicit deny in a resource-based policy"))
}

// A role ARN with its path names the role's sessions, and aws:PrincipalArn
// is that ARN, path included.
func TestARoleSessionMatchesItsRoleWithThePath(t *testing.T) {
	p := session(allow("s3:GetObject", "*"))
	a := newAuthorizer(buckets{"b": grant(`"`+roleARN+`"`, "s3:GetObject", "arn:aws:s3:::b/*")})
	if err := a.check(request(p, "/b/dir/k.txt"), GetObject, obj); err != nil {
		t.Fatalf("a grant to the role: %v", err)
	}
	pathless := newAuthorizer(buckets{"b": grant(`"arn:aws:iam::111111111111:role/Reader"`,
		"s3:GetObject", "arn:aws:s3:::b/*")})
	if err := pathless.check(request(p, "/b/dir/k.txt"), GetObject, obj); err == nil {
		t.Fatal("a grant to the role ARN without its path matched")
	}
	cond := newAuthorizer(buckets{"b": `{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
		`"Principal":{"AWS":"111111111111"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*",` +
		`"Condition":{"ArnLike":{"aws:PrincipalArn":"arn:aws:iam::111111111111:role/ops/*"}}}}`})
	if err := cond.check(request(p, "/b/dir/k.txt"), GetObject, obj); err != nil {
		t.Fatalf("a condition on the role's path: %v", err)
	}
	// A session whose role was deleted is refused.
	orphan := session(allow("s3:GetObject", "*"))
	orphan.PrincipalARN = ""
	if err := cond.check(request(orphan, "/b/dir/k.txt"), GetObject, obj); err == nil {
		t.Fatal("a session whose role was deleted was allowed")
	}
}

func TestAConditionOnAPrincipalTag(t *testing.T) {
	a := newAuthorizer(buckets{"b": `{"Version":"2012-10-17","Statement":{"Effect":"Deny",` +
		`"Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/*",` +
		`"Condition":{"StringNotEquals":{"aws:PrincipalTag/team":"storage"}}}}`})
	p := user(allow("s3:*", "*"))
	requireDenied(t, a.check(request(p, "/b/dir/k.txt"), GetObject, obj),
		deniedMsg(userARN, "s3:GetObject", "arn:aws:s3:::b/dir/k.txt",
			"with an explicit deny in a resource-based policy"))
	p.Tags = map[string]string{"Team": "storage"}
	if err := a.check(request(p, "/b/dir/k.txt"), GetObject, obj); err != nil {
		t.Fatalf("tagged: %v", err)
	}
}

// CopyObject's destination is evaluated with the destination bucket's
// policy and its source with the source bucket's.
func TestCopyObjectUsesEachBucketsPolicy(t *testing.T) {
	src := s3api.Target{Bucket: "src", Key: "a"}
	dst := s3api.Target{Bucket: "dst", Key: "b"}
	p := session(allow("s3:*", "*"))
	a := newAuthorizer(
		buckets{"dst": grant(`"111111111111"`, "s3:PutObject", "arn:aws:s3:::dst/*")},
	)
	requireDenied(t, a.check(request(p, "/dst/b"), CopyObject, dst, src),
		deniedMsg(sessionARN, "s3:GetObject", "arn:aws:s3:::src/a",
			"because no resource-based policy allows the s3:GetObject action"))
	both := newAuthorizer(buckets{
		"dst": grant(`"111111111111"`, "s3:PutObject", "arn:aws:s3:::dst/*"),
		"src": grant(`"111111111111"`, "s3:GetObject", "arn:aws:s3:::src/*"),
	})
	if err := both.check(request(p, "/dst/b"), CopyObject, dst, src); err != nil {
		t.Fatalf("copy granted by both buckets: %v", err)
	}
}

// DeleteObjects evaluates each key with the bucket's policy, read once for
// the request.
func TestDeleteObjectsEvaluatesEachKey(t *testing.T) {
	reads := 0
	src := countingSource{buckets: buckets{"b": grant(`"111111111111"`, "s3:DeleteObject",
		"arn:aws:s3:::b/tmp/*")}, reads: &reads}
	a := New("000000000000", src)
	c, err := a.Caller(request(session(allow("s3:*", "*")), "/b?delete"))
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Authorize(DeleteObjects, s3api.Target{Bucket: "b", Key: "tmp/a"}); err != nil {
		t.Fatalf("tmp/a: %v", err)
	}
	if err = c.Authorize(DeleteObjects, s3api.Target{Bucket: "b", Key: "keep"}); err == nil {
		t.Fatal("keep was allowed")
	}
	if reads != 1 {
		t.Fatalf("the bucket policy was read %d times, want 1", reads)
	}
}

type countingSource struct {
	buckets
	reads *int
}

func (s countingSource) GetBucketPolicy(
	ctx context.Context,
	bucket string,
) (string, []byte, error) {
	*s.reads++
	return s.buckets.GetBucketPolicy(ctx, bucket)
}

// ListBuckets has no resource policy and is evaluated in the caller's own
// account, so another account's principal needs only its identity allow,
// and is not in the server's account.
func TestListBucketsIsEvaluatedInTheCallersAccount(t *testing.T) {
	a := newAuthorizer(nil)
	c, err := a.Caller(request(session(allow("s3:ListAllMyBuckets", "*")), "/"))
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Authorize(ListBuckets, s3api.Target{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if c.InAccount() {
		t.Fatal("a principal of another account is in the server's account")
	}
}

func TestResourceAccountIsTheOwner(t *testing.T) {
	doc := `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"s3:*",` +
		`"Resource":"*","Condition":{"StringEquals":{"s3:ResourceAccount":"000000000000"}}}}`
	a := newAuthorizer(nil)
	if err := a.check(request(user(doc), "/b/dir/k.txt"), GetObject, obj); err != nil {
		t.Fatalf("owned bucket: %v", err)
	}
	if err := a.check(request(session(doc), "/"), ListBuckets, s3api.Target{}); err == nil {
		t.Fatal("ListBuckets of account 111111111111 matched s3:ResourceAccount 000000000000")
	}
}

// The bucket policy operations answer an allowed principal of another
// account with 405 MethodNotAllowed.
func TestPolicyOperationsAreTheOwnersOnly(t *testing.T) {
	a := newAuthorizer(buckets{"b": grant(`"111111111111"`, "s3:*Policy", "arn:aws:s3:::b")})
	for _, op := range []Operation{PutBucketPolicy, GetBucketPolicy, DeleteBucketPolicy} {
		err := a.check(request(session(allow("s3:*", "*")), "/b?policy"), op,
			s3api.Target{Bucket: "b"})
		var s3Err *s3api.Error
		if !errors.As(err, &s3Err) || s3Err.Status != http.StatusMethodNotAllowed ||
			s3Err.Code != "MethodNotAllowed" {
			t.Errorf("%s: got %v, want 405 MethodNotAllowed", op, err)
		}
		requireDenied(
			t,
			a.check(request(session(), "/b?policy"), op, s3api.Target{Bucket: "b"}),
			deniedMsg(sessionARN, Actions(op)[0], "arn:aws:s3:::b",
				"because no identity-based policy allows the "+Actions(op)[0]+" action"),
		)
		if err = a.check(request(user(allow("s3:*", "*")), "/b?policy"), op,
			s3api.Target{Bucket: "b"}); err != nil {
			t.Errorf("%s by the owner's user: %v", op, err)
		}
	}
}

func TestAnExpectedOwnerOtherThanTheServersIsRefused(t *testing.T) {
	a := newAuthorizer(nil)
	root := rootOf("000000000000")
	for _, tc := range []struct {
		header string
		op     Operation
		ok     bool
	}{
		{expectedOwnerHeader + ": 000000000000", GetObject, true},
		{expectedOwnerHeader + ": 111111111111", GetObject, false},
		{expectedOwnerHeader + ": x", HeadBucket, false},
		{expectedSourceOwnerHeader + ": 111111111111", CopyObject, false},
		{expectedSourceOwnerHeader + ": 111111111111", GetObject, true},
		{expectedOwnerHeader + ": 111111111111", ListBuckets, true},
	} {
		r := request(root, "/b/dir/k.txt")
		name, value, _ := strings.Cut(tc.header, ": ")
		r.Header.Set(name, value)
		targets := map[Operation][]s3api.Target{
			GetObject: {obj}, HeadBucket: {{Bucket: "b"}}, CopyObject: {obj, obj},
			ListBuckets: {{}},
		}[tc.op]
		err := a.check(r, tc.op, targets...)
		if tc.ok && err != nil {
			t.Errorf("%s %s: %v", tc.op, tc.header, err)
		}
		if !tc.ok {
			var s3Err *s3api.Error
			if !errors.As(err, &s3Err) || s3Err.Status != http.StatusForbidden ||
				s3Err.Code != "AccessDenied" || s3Err.Message != "Access Denied" {
				t.Errorf("%s %s: got %v, want 403 AccessDenied", tc.op, tc.header, err)
			}
		}
	}
	// Every value of a repeated header is checked.
	r := request(root, "/b")
	r.Header.Add(expectedOwnerHeader, "000000000000")
	r.Header.Add(expectedOwnerHeader, "111111111111")
	if err := a.check(r, HeadBucket, s3api.Target{Bucket: "b"}); err == nil {
		t.Fatal("a repeated header with another account was allowed")
	}
}

// A stored policy that no longer parses, or cannot be read, fails the
// request: it is never evaluated as absent.
func TestABucketPolicyThatCannotBeUsedFailsClosed(t *testing.T) {
	for name, src := range map[string]PolicySource{
		"unparsable": buckets{"b": `{"Statement":[]}`},
		"unreadable": failingSource{},
	} {
		err := New("000000000000", src).check(
			request(user(allow("s3:*", "*")), "/b/dir/k.txt"), GetObject, obj)
		var s3Err *s3api.Error
		if err == nil || errors.As(err, &s3Err) {
			t.Errorf("%s: got %v, want an internal error", name, err)
		}
	}
}

type failingSource struct{ buckets }

func (failingSource) GetBucketPolicy(context.Context, string) (string, []byte, error) {
	return "", nil, errors.New("disk on fire")
}

// The Caller carries the principal's attributes as identity reports them,
// for policy.Authorize to match and to supply aws:PrincipalArn and the
// other principal keys from.
func TestTheCallerCarriesThePrincipalsAttributes(t *testing.T) {
	c, err := newAuthorizer(nil).Caller(request(session(), "/"))
	if err != nil {
		t.Fatal(err)
	}
	if c.principal.AccountID != "111111111111" || c.principal.ARN != sessionARN ||
		c.principal.PrincipalARN != roleARN || c.principal.UserID != "AROAROLEEXAMPLE00001:e2e" ||
		string(c.principal.Type) != "AssumedRole" {
		t.Fatalf("caller = %+v", c.principal)
	}
	if _, ok := c.keys["aws:PrincipalArn"]; ok {
		t.Fatal("aws:PrincipalArn is derived here rather than supplied by policy.Authorize")
	}
}

func TestValidateBucketPolicy(t *testing.T) {
	stmt := func(fields string) string {
		return `{"Version":"2012-10-17","Statement":[{"Sid":"read-only_1","Effect":"Allow",` +
			fields + `}]}`
	}
	ok := stmt(`"Principal":{"AWS":"111111111111"},"Action":["s3:GetObject","S3:List*"],` +
		`"Resource":["arn:aws:s3:::b","arn:aws:s3:::b/home/${aws:username}/*"]`)
	if err := ValidateBucketPolicy("b", []byte(ok)); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
	for name, tc := range map[string]struct{ doc, message string }{
		"not JSON": {"{", "Syntax errors in policy."},
		"no principal": {
			stmt(`"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"`),
			"Missing required field Principal.",
		},
		"a group": {stmt(`"Principal":{"AWS":"arn:aws:iam::111111111111:group/g"},` +
			`"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"`), "Invalid principal in policy"},
		"another bucket": {stmt(`"Principal":"*","Action":"s3:GetObject",` +
			`"Resource":"arn:aws:s3:::other/*"`), "Policy has invalid resource"},
		"a bucket prefix": {stmt(`"Principal":"*","Action":"s3:GetObject",` +
			`"Resource":"arn:aws:s3:::bb/*"`), "Policy has invalid resource"},
		"every resource": {
			stmt(`"Principal":"*","Action":"s3:GetObject","Resource":"*"`),
			"Policy has invalid resource",
		},
		"a NotResource": {stmt(`"Principal":"*","Action":"s3:GetObject",` +
			`"NotResource":["arn:aws:s3:::b/*","arn:aws:s3:::*"]`), "Policy has invalid resource"},
		"another service": {stmt(`"Principal":"*","Action":["s3:GetObject","sqs:SendMessage"],` +
			`"Resource":"arn:aws:s3:::b/*"`), "Policy has invalid action"},
		"too large": {
			stmt(`"Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/` +
				strings.Repeat("a", MaxBucketPolicySize) + `"`),
			"Policy exceeds the maximum allowed document size.",
		},
	} {
		err := ValidateBucketPolicy("b", []byte(tc.doc))
		var s3Err *s3api.Error
		if !errors.As(err, &s3Err) || s3Err.Status != http.StatusBadRequest ||
			s3Err.Code != "MalformedPolicy" || s3Err.Message != tc.message {
			t.Errorf("%s: got %v, want 400 MalformedPolicy %q", name, err, tc.message)
		}
	}
}

// rootOf is the root user of account, as identity and the static key
// report it.
func rootOf(account string) sigv4.Principal {
	arn := "arn:aws:iam::" + account + ":root"
	return sigv4.Principal{
		AccountID: account, ARN: arn, PrincipalARN: arn, UserID: account,
		Type: sigv4.PrincipalTypeAccount, Root: true,
	}
}

// Root and the Account type must agree: evaluation is skipped for one and
// full identity access granted for the other.
func TestRootAndTheAccountTypeMustAgree(t *testing.T) {
	a := newAuthorizer(nil)
	notRoot := rootOf("000000000000")
	notRoot.Root = false
	rootUser := user(allow("s3:*", "*"))
	rootUser.Root = true
	for name, p := range map[string]sigv4.Principal{
		"an Account that is not Root": notRoot, "a Root User": rootUser,
	} {
		err := a.check(request(p, "/b/dir/k.txt"), GetObject, obj)
		var s3Err *s3api.Error
		if err == nil || errors.As(err, &s3Err) {
			t.Errorf("%s: got %v, want an internal error", name, err)
		}
	}
}

// check is Authorize's error alone.
func (a *Authorizer) check(r *http.Request, op Operation, targets ...s3api.Target) error {
	_, err := a.Authorize(r, op, targets...)
	return err
}

// notFound is the NotFound of r's caller, for the bucket "b".
func (a *Authorizer) notFound(r *http.Request, err error) error {
	c, cErr := a.Caller(r)
	if cErr != nil {
		return cErr
	}
	return c.NotFound("b", err)
}
