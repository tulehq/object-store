package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
