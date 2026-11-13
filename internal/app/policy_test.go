package app

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"tule.dev/object-store/internal/authz"
)

// The bucket policy API stores, returns and removes a bucket's policy with
// S3's request and response shapes and errors, and the stored policy is
// evaluated.
func TestTheBucketPolicyAPI(t *testing.T) {
	f := &fakeIdentity{}
	root, user := startAuthzApp(t, f)
	setup(t, root)
	ctx := t.Context()

	_, err := root.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: b})
	requireStatus(t, err, http.StatusNotFound, "NoSuchBucketPolicy")

	doc := `{"Version":"2012-10-17","Statement":{"Sid":"dev-read","Effect":"Allow",` +
		`"Principal":{"AWS":"` + testUserARN + `"},"Action":"s3:GetObject",` +
		`"Resource":"arn:aws:s3:::bkt/*"}}`
	if _, err = root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: b, Policy: aws.String(doc),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := root.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: b})
	if err != nil || aws.ToString(got.Policy) != doc {
		t.Fatalf("get: %v %q", err, aws.ToString(got.Policy))
	}

	// dev has no identity policy; the grant naming it is enough.
	f.setPolicies()
	out, err := user.GetObject(ctx, &s3.GetObjectInput{Bucket: b, Key: key})
	if err != nil {
		t.Fatalf("granted by the bucket policy: %v", err)
	}
	_ = out.Body.Close()
	_, err = user.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: b})
	requireStatus(t, err, http.StatusForbidden, "AccessDenied")

	if _, err = root.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: b}); err != nil {
		t.Fatal(err)
	}
	if _, err = root.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: b}); err != nil {
		t.Fatalf("deleting a policy that is gone: %v", err)
	}
	_, err = root.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: b})
	requireStatus(t, err, http.StatusNotFound, "NoSuchBucketPolicy")
	_, err = user.GetObject(ctx, &s3.GetObjectInput{Bucket: b, Key: key})
	requireStatus(t, err, http.StatusForbidden, "AccessDenied")
}

// PutBucketPolicy refuses what S3 refuses, before the bucket is looked up.
func TestPutBucketPolicyRefusals(t *testing.T) {
	root, _, url := startAuthzServer(t, &fakeIdentity{})
	setup(t, root)
	ctx := t.Context()
	doc := `{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
		`"Principal":{"AWS":"` + testUserARN + `"},"Action":"s3:GetObject",` +
		`"Resource":"arn:aws:s3:::bkt/*"}}`
	for name, policy := range map[string]string{
		"another bucket": strings.Replace(doc, "bkt/*", "other/*", 1),
		"no principal":   strings.Replace(doc, `"Principal":{"AWS":"`+testUserARN+`"},`, "", 1),
		"too large":      strings.Replace(doc, "bkt/*", "bkt/"+strings.Repeat("a", 20*1024), 1),
	} {
		_, err := root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
			Bucket: b, Policy: aws.String(policy),
		})
		if err == nil {
			t.Fatalf("%s: stored", name)
		}
		requireStatus(t, err, http.StatusBadRequest, "MalformedPolicy")
	}
	// dev may not put a policy, so its empty body is not reached.
	if status, _ := signedDo(t, http.MethodPut, url+"/bkt?policy", nil); status != 403 {
		t.Fatalf("an empty policy by dev: %d", status)
	}
	_, err := root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String("missing"),
		Policy: aws.String(strings.Replace(doc, "bkt/*", "missing/*", 1)),
	})
	requireStatus(t, err, http.StatusNotFound, "NoSuchBucket")
	_, err = root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: b, Policy: aws.String(doc), ExpectedBucketOwner: aws.String("111111111111"),
	})
	requireStatus(t, err, http.StatusForbidden, "AccessDenied")
}

// A bucket deleted and created again has no policy.
func TestABucketCreatedAgainHasNoPolicy(t *testing.T) {
	root, _ := startAuthzApp(t, &fakeIdentity{})
	ctx := t.Context()
	name := aws.String("again")
	if _, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: name,
		Policy: aws.String(`{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
			`"Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::again/*"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := root.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: name}); err != nil {
		t.Fatal(err)
	}
	_, err := root.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: name})
	requireStatus(t, err, http.StatusNotFound, "NoSuchBucketPolicy")
}

// DeleteObjects with an expected owner other than the bucket's fails as a
// whole with 403, rather than per key.
func TestDeleteObjectsChecksTheExpectedOwnerOnce(t *testing.T) {
	root, _ := startAuthzApp(t, &fakeIdentity{})
	setup(t, root)
	_, err := root.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
		Bucket:              b,
		ExpectedBucketOwner: aws.String("111111111111"),
		Delete:              &s3types.Delete{Objects: []s3types.ObjectIdentifier{{Key: key}}},
	})
	requireStatus(t, err, http.StatusForbidden, "AccessDenied")
	if _, err = root.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: b, Key: key}); err != nil {
		t.Fatalf("the key was deleted: %v", err)
	}
}

// policyPaths classifies every operation that writes or reads a stored
// bucket policy: "bind" binds what it stores, "show" shows what it
// returns, "" neither. An operation named for policies that is missing
// here fails TestBucketPolicyPrincipalsAreBound until it is classified and
// tested below.
var policyPaths = map[authz.Operation]string{
	authz.PutBucketPolicy:    "bind",
	authz.GetBucketPolicy:    "show",
	authz.DeleteBucketPolicy: "",
}

// Every operation named for policies is classified in policyPaths.
func TestEveryPolicyOperationIsClassified(t *testing.T) {
	for _, op := range authz.Operations() {
		if _, ok := policyPaths[op]; strings.Contains(string(op), "Policy") && !ok {
			t.Errorf("%s reads or writes a bucket policy and is not in policyPaths", op)
		}
	}
}

// PutBucketPolicy stores a policy with its users bound to their unique IDs,
// GetBucketPolicy shows each ID as its user's current ARN, or as the ID
// once that user is gone, and a user that does not exist is refused as S3
// refuses it. A failure to reach identity fails the request.
func TestBucketPolicyPrincipalsAreBound(t *testing.T) {
	f := &fakeIdentity{}
	root, user := startAuthzApp(t, f)
	setup(t, root)
	ctx := t.Context()
	get := func() string { return getPolicy(t, root) }
	doc := grantGet

	// bind: the stored document names dev's ID, which Show maps back.
	if _, err := root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: b, Policy: aws.String(doc(testUserARN)),
	}); err != nil {
		t.Fatal(err)
	}
	if bound, _ := f.calls(); bound != 1 {
		t.Fatalf("PutBucketPolicy bound %d times, want 1", bound)
	}
	moved := "arn:aws:iam::000000000000:user/moved/dev"
	f.setPrincipals(map[string]string{moved: testUserUID})
	// show: the ID is shown as dev's current ARN.
	if got := get(); got != doc(moved) {
		t.Fatalf("shown %s, want %s", got, doc(moved))
	}
	if _, shown := f.calls(); shown != 1 {
		t.Fatalf("GetBucketPolicy showed %d times, want 1", shown)
	}
	// Evaluation matches the stored ID.
	requireGet(t, user, "granted by its unique ID")

	// A user that is gone is shown by its ID.
	f.setPrincipals(map[string]string{})
	if got := get(); got != doc(testUserUID) {
		t.Fatalf("shown %s, want %s", got, doc(testUserUID))
	}

	// A user that does not exist is refused, and the stored policy kept.
	_, err := root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: b, Policy: aws.String(doc(testUserARN)),
	})
	requireStatus(t, err, http.StatusBadRequest, "MalformedPolicy")
	requireMessage(t, err, "Invalid principal in policy")

	// Identity unreachable: the write and the read fail, and nothing is
	// stored.
	f.setPrincipalsDown(true)
	_, err = root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: b, Policy: aws.String(doc(moved)),
	})
	requireStatus(t, err, http.StatusInternalServerError, "InternalError")
	_, err = root.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: b})
	requireStatus(t, err, http.StatusInternalServerError, "InternalError")
	// Evaluation does not need identity's principal-ID endpoint.
	requireGet(t, user, "evaluated with the principal-ID endpoint down")
	f.setPrincipalsDown(false)
	f.setPrincipals(map[string]string{testUserARN: testUserUID})
	if got := get(); got != doc(testUserARN) {
		t.Fatalf("after the failed write, shown %s, want %s", got, doc(testUserARN))
	}
}

// requireGet checks that c can read the test object.
func requireGet(t *testing.T, c *s3.Client, what string) {
	t.Helper()
	out, err := c.GetObject(t.Context(), &s3.GetObjectInput{Bucket: b, Key: key})
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	_ = out.Body.Close()
}

// getPolicy returns bkt's policy, read with c.
func getPolicy(t *testing.T, c *s3.Client) string {
	t.Helper()
	out, err := c.GetBucketPolicy(t.Context(), &s3.GetBucketPolicyInput{Bucket: b})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return aws.ToString(out.Policy)
}

// grantGet is a policy allowing the AWS principal s3:GetObject in bkt.
func grantGet(principal string) string {
	return `{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
		`"Principal":{"AWS":"` + principal + `"},"Action":"s3:GetObject",` +
		`"Resource":"arn:aws:s3:::bkt/*"}}`
}

// requireMessage checks that err is an S3 error with message.
func requireMessage(t *testing.T, err error, message string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorMessage() != message {
		t.Fatalf("got %v, want %q", err, message)
	}
}

// A policy under 20 KB as sent that a long unique ID pushes over it once
// bound is refused as an oversized policy is, and nothing is stored.
func TestABoundPolicyOver20KBIsRefused(t *testing.T) {
	f := &fakeIdentity{}
	root, _ := startAuthzApp(t, f)
	setup(t, root)
	// The longest ID the endpoint may answer: AIDA and 124 more.
	longID := "AIDA" + strings.Repeat("X", 124)
	f.setPrincipals(map[string]string{testUserARN: longID})

	sent := grantGet(testUserARN)
	pad := 20*1024 - len(sent) - 10
	sent = strings.Replace(sent, "bkt/*", "bkt/"+strings.Repeat("a", pad)+"*", 1)
	if len(sent) > 20*1024 || len(sent)-len(testUserARN)+len(longID) <= 20*1024 {
		t.Fatalf("the document is %d bytes as sent; the test needs it to fit only unbound",
			len(sent))
	}
	_, err := root.PutBucketPolicy(t.Context(), &s3.PutBucketPolicyInput{
		Bucket: b, Policy: aws.String(sent),
	})
	requireStatus(t, err, http.StatusBadRequest, "MalformedPolicy")
	requireMessage(t, err, "Policy exceeds the maximum allowed document size.")
	if bound, _ := f.calls(); bound != 1 {
		t.Fatalf("bound %d times, want 1", bound)
	}
	_, err = root.GetBucketPolicy(t.Context(), &s3.GetBucketPolicyInput{Bucket: b})
	requireStatus(t, err, http.StatusNotFound, "NoSuchBucketPolicy")
}

// PutBucketPolicy is authorized before its policy is bound: a caller
// denied s3:PutBucketPolicy never makes the server ask identity's
// principal-ID endpoint.
func TestADeniedPutBucketPolicyDoesNotAskIdentity(t *testing.T) {
	f := &fakeIdentity{}
	root, user := startAuthzApp(t, f)
	setup(t, root)
	_, err := user.PutBucketPolicy(t.Context(), &s3.PutBucketPolicyInput{
		Bucket: b, Policy: aws.String(grantGet(testUserARN)),
	})
	requireStatus(t, err, http.StatusForbidden, "AccessDenied")
	if bound, shown := f.calls(); bound != 0 || shown != 0 {
		t.Fatalf("a denied PutBucketPolicy made %d bind and %d show calls", bound, shown)
	}
	// The same request allowed does ask.
	f.setPolicies(`{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
		`"Action":"s3:PutBucketPolicy","Resource":"*"}}`)
	if _, err = user.PutBucketPolicy(t.Context(), &s3.PutBucketPolicyInput{
		Bucket: b, Policy: aws.String(grantGet(testUserARN)),
	}); err != nil {
		t.Fatal(err)
	}
	if bound, _ := f.calls(); bound != 1 {
		t.Fatalf("an allowed PutBucketPolicy bound %d times, want 1", bound)
	}
}
