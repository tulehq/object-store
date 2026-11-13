//go:build e2e

package e2e

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// The tests here run object-store against the real identity service and
// check that it enforces the identity policies identity hands over, as S3
// does within one account.

const devARN = "arn:aws:iam::000000000000:user/dev"

// policyEnv is identity, object-store accepting its keys, an IAM client
// with the root key, and the bucket "bkt" holding "public/a",
// "private/a" and "keep/a", made with the root key.
type policyEnv struct {
	srv      *httptest.Server
	ic       *iam.Client
	root     *s3.Client
	identity string
}

func startPolicyEnv(t *testing.T) *policyEnv {
	t.Helper()
	identity := startSibling(t, "identity", identityDirEnv)
	srv := startWithIdentity(t, identity.URL)
	root := s3As(srv, rootCreds())
	createBucket(t, root, "bkt")
	for _, k := range []string{"public/a", "private/a", "keep/a"} {
		put(t, root, "bkt", k, []byte(k))
	}
	return &policyEnv{srv: srv, ic: iamClient(identity.URL), root: root, identity: identity.URL}
}

// user creates the IAM user dev with docs as its inline policies and
// returns an S3 client signing with its key.
func (e *policyEnv) user(t *testing.T, docs ...string) *s3.Client {
	t.Helper()
	key := createUserKey(t, e.ic, "dev")
	for i, d := range docs {
		putUserPolicy(t, e.ic, "dev", "p"+string(rune('0'+i)), d)
	}
	return s3As(e.srv, key)
}

func allowDoc(actions, resource string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":` + actions +
		`,"Resource":"` + resource + `"}]}`
}

// notAllowed is S3's message for an action no identity policy allows.
func notAllowed(principal, action, resource string) string {
	on := ""
	if resource != "" {
		on = ` on resource: "` + resource + `"`
	}
	return "User: " + principal + " is not authorized to perform: " + action + on +
		" because no identity-based policy allows the " + action + " action"
}

func getObject(t *testing.T, c *s3.Client, key string) error {
	t.Helper()
	out, err := c.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String(key),
	})
	if err == nil {
		_ = out.Body.Close()
	}
	return err //nolint:wrapcheck // the test inspects the SDK error
}

func requireDenied(t *testing.T, err error, message string) {
	t.Helper()
	requireAPIError(t, err, "AccessDenied", message)
	requireResponse(t, err, http.StatusForbidden)
}

func TestPolicyNoneDeniesEverything(t *testing.T) {
	e := startPolicyEnv(t)
	// A user other than dev, whose key has no policy.
	user := s3As(e.srv, createUserKey(t, e.ic, "nobody"))
	const nobody = "arn:aws:iam::000000000000:user/nobody"

	_, err := user.ListBuckets(t.Context(), &s3.ListBucketsInput{})
	requireDenied(t, err, notAllowed(nobody, "s3:ListAllMyBuckets", ""))
	requireDenied(t, getObject(t, user, "public/a"),
		notAllowed(nobody, "s3:GetObject", "arn:aws:s3:::bkt/public/a"))
	_, err = user.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("new"), Body: strings.NewReader("x"),
	})
	requireDenied(t, err, notAllowed(nobody, "s3:PutObject", "arn:aws:s3:::bkt/new"))
	// A HEAD's 403 has no body.
	_, err = user.HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String("bkt")})
	requireResponse(t, err, http.StatusForbidden)
}

func TestPolicyAllowsExactlyItsAction(t *testing.T) {
	e := startPolicyEnv(t)
	user := e.user(t, allowDoc(`"s3:GetObject"`, "*"))
	ctx := t.Context()

	requireNoErr(t, getObject(t, user, "public/a"), "get")
	_, err := user.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("public/a"),
	})
	requireNoErr(t, err, "head, which is s3:GetObject")
	// Without s3:ListBucket, a missing key is 403, not 404.
	requireDenied(t, getObject(t, user, "missing"),
		notAllowed(devARN, "s3:ListBucket", "arn:aws:s3:::bkt"))
	_, err = user.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("missing"),
	})
	requireResponse(t, err, http.StatusForbidden)

	_, err = user.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("bkt")})
	requireDenied(t, err, notAllowed(devARN, "s3:ListBucket", "arn:aws:s3:::bkt"))
	_, err = user.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("public/a"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:DeleteObject", "arn:aws:s3:::bkt/public/a"))
	_, err = user.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("copy"), CopySource: aws.String("bkt/public/a"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutObject", "arn:aws:s3:::bkt/copy"))
	_, err = user.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("other")})
	requireDenied(t, err, notAllowed(devARN, "s3:CreateBucket", "arn:aws:s3:::other"))
}

func TestPolicyScopedToAResource(t *testing.T) {
	e := startPolicyEnv(t)
	user := e.user(t, allowDoc(`["s3:GetObject","s3:PutObject"]`, "arn:aws:s3:::bkt/public/*"))
	ctx := t.Context()

	requireNoErr(t, getObject(t, user, "public/a"), "get public/a")
	requireDenied(t, getObject(t, user, "private/a"),
		notAllowed(devARN, "s3:GetObject", "arn:aws:s3:::bkt/private/a"))

	// CopyObject needs s3:GetObject on its source as well.
	_, err := user.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("public/b"),
		CopySource: aws.String("bkt/public/a"),
	})
	requireNoErr(t, err, "copy within public/")
	_, err = user.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("public/c"),
		CopySource: aws.String("bkt/private/a"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:GetObject", "arn:aws:s3:::bkt/private/a"))
	// The source is evaluated decoded, however the header spells it.
	_, err = user.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("public/d"),
		CopySource: aws.String("/bkt/%70rivate/a"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:GetObject", "arn:aws:s3:::bkt/private/a"))

	// A presigned URL is evaluated as its signer.
	presign := s3.NewPresignClient(user)
	for key, status := range map[string]int{
		"public/a": http.StatusOK, "private/a": http.StatusForbidden,
	} {
		pre, err := presign.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String("bkt"), Key: aws.String(key),
		})
		requireNoErr(t, err, "presign "+key)
		got, _ := getWith(t, e.srv.Client(), pre.URL)
		requireEqual(t, got, status, "presigned get of "+key)
	}
}

func TestPolicyExplicitDenyWins(t *testing.T) {
	e := startPolicyEnv(t)
	user := e.user(t,
		allowDoc(`"s3:*"`, "*"),
		`{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"s3:DeleteObject",`+
			`"Resource":"arn:aws:s3:::bkt/keep/*"}]}`)
	ctx := t.Context()

	_, err := user.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("keep/a"),
	})
	requireDenied(t, err, "User: "+devARN+" is not authorized to perform: s3:DeleteObject "+
		`on resource: "arn:aws:s3:::bkt/keep/a" with an explicit deny in an identity-based policy`)

	// DeleteObjects is evaluated per key, each denied key answered on its
	// own.
	out, err := user.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("bkt"),
		Delete: &s3types.Delete{Objects: []s3types.ObjectIdentifier{
			{Key: aws.String("keep/a")}, {Key: aws.String("private/a")},
		}},
	})
	requireNoErr(t, err, "delete objects")
	requireEqual(t, len(out.Deleted), 1, "deleted keys")
	requireEqual(t, aws.ToString(out.Deleted[0].Key), "private/a", "deleted key")
	requireEqual(t, len(out.Errors), 1, "key errors")
	requireEqual(t, aws.ToString(out.Errors[0].Key), "keep/a", "denied key")
	requireEqual(t, aws.ToString(out.Errors[0].Code), "AccessDenied", "denied key's code")
	// The message the DeleteObjects API reference shows for a denied key.
	requireEqual(t, aws.ToString(out.Errors[0].Message), "Access Denied", "denied key's message")
	requireNoErr(t, getObject(t, e.root, "keep/a"), "keep/a survives")
}

func TestPolicyConditions(t *testing.T) {
	e := startPolicyEnv(t)
	// The client reaches the server from 127.0.0.1.
	user := e.user(t,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject",`+
			`"Resource":"*","Condition":{"IpAddress":{"aws:SourceIp":"127.0.0.0/8"}}},`+
			`{"Effect":"Allow","Action":"s3:PutObject","Resource":"*",`+
			`"Condition":{"IpAddress":{"aws:SourceIp":"192.0.2.0/24"}}},`+
			`{"Effect":"Allow","Action":"s3:ListBucket","Resource":"arn:aws:s3:::bkt",`+
			`"Condition":{"StringLike":{"s3:prefix":"public/*"}}}]}`)
	ctx := t.Context()

	requireNoErr(t, getObject(t, user, "private/a"), "get from 127.0.0.1")
	_, err := user.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("new"), Body: strings.NewReader("x"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutObject", "arn:aws:s3:::bkt/new"))

	_, err = user.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String("bkt"), Prefix: aws.String("public/"),
	})
	requireNoErr(t, err, "list public/")
	_, err = user.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String("bkt"), Prefix: aws.String("private/"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:ListBucket", "arn:aws:s3:::bkt"))
	_, err = user.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("bkt")})
	requireDenied(t, err, notAllowed(devARN, "s3:ListBucket", "arn:aws:s3:::bkt"))
}

func TestPolicyRoleSession(t *testing.T) {
	e := startPolicyEnv(t)
	ctx := t.Context()
	key := createUserKey(t, e.ic, "dev")

	_, err := e.ic.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: aws.String("Reader"),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[` +
			`{"Effect":"Allow","Principal":{"AWS":"` + devARN + `"},"Action":"sts:AssumeRole"}]}`),
	})
	requireNoErr(t, err, "create role")
	// aws:PrincipalArn of a session is its role's ARN.
	_, err = e.ic.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName: aws.String("Reader"), PolicyName: aws.String("read"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*","Condition":{"ArnEquals":` +
			`{"aws:PrincipalArn":"arn:aws:iam::000000000000:role/Reader"}}}]}`),
	})
	requireNoErr(t, err, "put role policy")

	sc := sts.New(sts.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(e.identity),
		Credentials:      credentials.StaticCredentialsProvider{Value: key},
		HTTPClient:       &http.Client{Timeout: serviceTimeout},
		RetryMaxAttempts: 1,
	})
	assumed, err := sc.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String("arn:aws:iam::000000000000:role/Reader"),
		RoleSessionName: aws.String("e2e"),
	})
	requireNoErr(t, err, "assume role")
	session := s3As(e.srv, aws.Credentials{
		AccessKeyID:     aws.ToString(assumed.Credentials.AccessKeyId),
		SecretAccessKey: aws.ToString(assumed.Credentials.SecretAccessKey),
		SessionToken:    aws.ToString(assumed.Credentials.SessionToken),
	})

	requireNoErr(t, getObject(t, session, "private/a"), "session get")
	_, err = session.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("new"), Body: strings.NewReader("x"),
	})
	requireDenied(t, err, notAllowed("arn:aws:sts::000000000000:assumed-role/Reader/e2e",
		"s3:PutObject", "arn:aws:s3:::bkt/new"))
	// dev itself has no policy.
	requireDenied(t, getObject(t, s3As(e.srv, key), "private/a"),
		notAllowed(devARN, "s3:GetObject", "arn:aws:s3:::bkt/private/a"))
}

// The configured key is object-store's static key and identity's root key
// at once, since object-store authenticates to identity with it, so one key
// covers both. It is never evaluated.
func TestPolicyRootKeyIsAlwaysAllowed(t *testing.T) {
	e := startPolicyEnv(t)
	ctx := t.Context()
	requireRootWorks(t, e.srv)
	requireNoErr(t, getObject(t, e.root, "private/a"), "root get")
	_, err := e.root.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("copy"), CopySource: aws.String("bkt/private/a"),
	})
	requireNoErr(t, err, "root copy")
	out, err := e.root.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("bkt"),
		Delete: &s3types.Delete{Objects: []s3types.ObjectIdentifier{{Key: aws.String("copy")}}},
	})
	requireNoErr(t, err, "root delete objects")
	requireEqual(t, len(out.Errors), 0, "root delete errors")
}

// Without an identity endpoint the static key does everything, as before.
func TestPolicyStandaloneIsUnchanged(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	createBucket(t, c, "solo")
	put(t, c, "solo", "a", []byte("a"))
	_, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("solo"), Key: aws.String("b"), CopySource: aws.String("solo/a"),
	})
	requireNoErr(t, err, "copy")
	out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("solo"),
		Delete: &s3types.Delete{Objects: []s3types.ObjectIdentifier{
			{Key: aws.String("a")}, {Key: aws.String("b")},
		}},
	})
	requireNoErr(t, err, "delete objects")
	requireEqual(t, len(out.Errors), 0, "delete errors")
	_, err = c.ListBuckets(ctx, &s3.ListBucketsInput{})
	requireNoErr(t, err, "list buckets")
}
