//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// The tests here run object-store against the real identity service and
// check that bucket policies are evaluated with identity policies as S3
// evaluates them, within the account and across accounts.

// putBucketPolicy sets the policy of bkt with the root key.
func (e *policyEnv) putBucketPolicy(t *testing.T, statements ...string) {
	t.Helper()
	_, err := e.root.PutBucketPolicy(t.Context(), &s3.PutBucketPolicyInput{
		Bucket: aws.String("bkt"),
		Policy: aws.String(`{"Version":"2012-10-17","Statement":[` +
			strings.Join(statements, ",") + `]}`),
	})
	requireNoErr(t, err, "put bucket policy")
}

// statement is one bucket policy statement.
func statement(effect, principal, action, resource, condition string) string {
	s := `{"Effect":"` + effect + `","Principal":` + principal + `,"Action":"` + action +
		`","Resource":"` + resource + `"`
	if condition != "" {
		s += `,"Condition":` + condition
	}
	return s + "}"
}

// denied is S3's AccessDenied message for principal and action on
// resource, ending with reason.
func denied(principal, action, resource, reason string) string {
	return "User: " + principal + " is not authorized to perform: " + action +
		` on resource: "` + resource + `" ` + reason
}

// crossAccountEnv is policyEnv over identity with a member account, and
// partner, an IAM user of that account made by its administrator, with an
// identity Allow for every S3 action in its inline policy "s3".
type crossAccountEnv struct {
	*policyEnv
	memberIAM  *iam.Client
	partner    *s3.Client
	partnerARN string
}

func startCrossAccountEnv(t *testing.T) *crossAccountEnv {
	t.Helper()
	account, identityURL := startMemberAccount(t)
	srv := startWithIdentity(t, identityURL)
	e := &policyEnv{
		srv: srv, ic: iamClient(identityURL), root: s3As(srv, rootCreds()), identity: identityURL,
	}
	createBucket(t, e.root, "bkt")
	put(t, e.root, "bkt", "public/a", []byte("a"))

	sc := sts.New(sts.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(identityURL),
		Credentials:      credentials.StaticCredentialsProvider{Value: rootCreds()},
		HTTPClient:       &http.Client{Timeout: serviceTimeout},
		RetryMaxAttempts: 1,
	})
	assumed, err := sc.AssumeRole(t.Context(), &sts.AssumeRoleInput{
		RoleArn: aws.String(
			"arn:aws:iam::" + account + ":role/OrganizationAccountAccessRole"),
		RoleSessionName: aws.String("e2e"),
	})
	requireNoErr(t, err, "assume role")
	admin := aws.Credentials{
		AccessKeyID:     aws.ToString(assumed.Credentials.AccessKeyId),
		SecretAccessKey: aws.ToString(assumed.Credentials.SecretAccessKey),
		SessionToken:    aws.ToString(assumed.Credentials.SessionToken),
	}
	memberIAM := iam.New(iam.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(identityURL),
		Credentials: credentials.StaticCredentialsProvider{Value: admin},
		HTTPClient:  &http.Client{Timeout: serviceTimeout}, RetryMaxAttempts: 1,
	})
	key := createUserKey(t, memberIAM, "partner")
	putUserPolicy(t, memberIAM, "partner", "s3", allowDoc(`"s3:*"`, "*"))
	return &crossAccountEnv{
		policyEnv: e, memberIAM: memberIAM, partner: s3As(srv, key),
		partnerARN: "arn:aws:iam::" + account + ":user/partner",
	}
}

// A user of another account with an identity Allow is allowed what the
// bucket policy grants its account, and refused, with the resource-based
// message, what it does not. The bucket policy operations answer it with
// 405 MethodNotAllowed even when both policies allow them.
func TestBucketPolicyCrossAccount(t *testing.T) {
	e := startCrossAccountEnv(t)
	ctx := t.Context()
	account := strings.Split(e.partnerARN, ":")[4]

	requireDenied(t, getObject(t, e.partner, "public/a"), denied(
		e.partnerARN,
		"s3:GetObject",
		"arn:aws:s3:::bkt/public/a",
		"because no resource-based policy allows the s3:GetObject action",
	))

	e.putBucketPolicy(
		t,
		statement(
			"Allow",
			`{"AWS":"`+account+`"}`,
			"s3:GetObject",
			"arn:aws:s3:::bkt/public/*",
			"",
		),
		statement("Allow", `{"AWS":"`+account+`"}`, "s3:GetBucketPolicy", "arn:aws:s3:::bkt", ""),
	)
	requireNoErr(t, getObject(t, e.partner, "public/a"), "partner get, granted")
	_, err := e.partner.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("public/b"), Body: strings.NewReader("b"),
	})
	requireDenied(t, err, denied(e.partnerARN, "s3:PutObject", "arn:aws:s3:::bkt/public/b",
		"because no resource-based policy allows the s3:PutObject action"))
	_, err = e.partner.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String("bkt")})
	requireCode(t, err, "MethodNotAllowed")
	requireResponse(t, err, http.StatusMethodNotAllowed)

	// Creating a bucket another account owns is BucketAlreadyExists, even
	// with both policies allowing it.
	member := `{"AWS":"` + account + `"}`
	e.putBucketPolicy(t,
		statement("Allow", member, "s3:GetObject", "arn:aws:s3:::bkt/public/*", ""),
		statement("Allow", member, "s3:CreateBucket", "arn:aws:s3:::bkt", ""))
	_, err = e.partner.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("bkt")})
	requireCode(t, err, "BucketAlreadyExists")
	requireResponse(t, err, http.StatusConflict)
	_, err = e.root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("bkt")})
	requireCode(t, err, "BucketAlreadyOwnedByYou")

	// Without the identity Allow, the bucket policy's grant is not enough.
	_, err = e.memberIAM.DeleteUserPolicy(ctx, &iam.DeleteUserPolicyInput{
		UserName: aws.String("partner"), PolicyName: aws.String("s3"),
	})
	requireNoErr(t, err, "delete partner policy")
	requireDenied(t, getObject(t, e.partner, "public/a"), denied(
		e.partnerARN,
		"s3:GetObject",
		"arn:aws:s3:::bkt/public/a",
		"because no identity-based policy allows the s3:GetObject action",
	))
}

// An explicit Deny in the bucket policy beats an identity Allow; the root
// key is unaffected.
func TestBucketPolicyDenyWins(t *testing.T) {
	e := startPolicyEnv(t)
	dev := e.user(t, allowDoc(`"s3:*"`, "*"))
	e.putBucketPolicy(t, statement("Deny", `"*"`, "s3:GetObject", "arn:aws:s3:::bkt/private/*", ""))
	requireDenied(t, getObject(t, dev, "private/a"), denied(devARN, "s3:GetObject",
		"arn:aws:s3:::bkt/private/a", "with an explicit deny in a resource-based policy"))
	requireNoErr(t, getObject(t, dev, "public/a"), "dev get public/a")
	requireNoErr(t, getObject(t, e.root, "private/a"), "root get private/a")
}

// Within the account, a bucket policy naming the IAM user grants on its own,
// while one naming only the account needs the identity Allow.
func TestBucketPolicySameAccount(t *testing.T) {
	e := startPolicyEnv(t)
	dev := e.user(t)
	e.putBucketPolicy(t,
		statement("Allow", `{"AWS":"`+devARN+`"}`, "s3:GetObject", "arn:aws:s3:::bkt/public/*", ""),
		statement("Allow", `{"AWS":"arn:aws:iam::000000000000:root"}`, "s3:GetObject",
			"arn:aws:s3:::bkt/keep/*", ""))
	requireNoErr(t, getObject(t, dev, "public/a"), "granted to dev")
	requireDenied(t, getObject(t, dev, "keep/a"), notAllowed(devARN, "s3:GetObject",
		"arn:aws:s3:::bkt/keep/a"))
	putUserPolicy(t, e.ic, "dev", "keep", allowDoc(`"s3:GetObject"`, "arn:aws:s3:::bkt/keep/*"))
	requireNoErr(t, getObject(t, dev, "keep/a"), "account grant with the identity allow")
}

// A role session matches a grant to its role's ARN with the role's path,
// and aws:PrincipalArn is that ARN, path included.
func TestBucketPolicyRoleWithPath(t *testing.T) {
	e := startPolicyEnv(t)
	ctx := t.Context()
	key := createUserKey(t, e.ic, "dev")
	roleARN := "arn:aws:iam::000000000000:role/ops/Reader"
	_, err := e.ic.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: aws.String("Reader"), Path: aws.String("/ops/"),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[` +
			`{"Effect":"Allow","Principal":{"AWS":"` + devARN + `"},"Action":"sts:AssumeRole"}]}`),
	})
	requireNoErr(t, err, "create role")
	e.putBucketPolicy(t,
		statement("Allow", `{"AWS":"`+roleARN+`"}`, "s3:*", "arn:aws:s3:::bkt/*", ""),
		statement("Deny", `"*"`, "s3:PutObject", "arn:aws:s3:::bkt/*",
			`{"ArnLike":{"aws:PrincipalArn":"arn:aws:iam::000000000000:role/ops/*"}}`))

	sc := sts.New(sts.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(e.identity),
		Credentials: credentials.StaticCredentialsProvider{Value: key},
		HTTPClient:  &http.Client{Timeout: serviceTimeout}, RetryMaxAttempts: 1,
	})
	assumed, err := sc.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: aws.String(roleARN), RoleSessionName: aws.String("e2e"),
	})
	requireNoErr(t, err, "assume role")
	session := s3As(e.srv, aws.Credentials{
		AccessKeyID:     aws.ToString(assumed.Credentials.AccessKeyId),
		SecretAccessKey: aws.ToString(assumed.Credentials.SecretAccessKey),
		SessionToken:    aws.ToString(assumed.Credentials.SessionToken),
	})
	// The role has no identity policy: the grant to its ARN is enough.
	requireNoErr(t, getObject(t, session, "private/a"), "session get")
	_, err = session.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("new"), Body: strings.NewReader("x"),
	})
	requireDenied(t, err, denied("arn:aws:sts::000000000000:assumed-role/Reader/e2e",
		"s3:PutObject", "arn:aws:s3:::bkt/new", "with an explicit deny in a resource-based policy"))
}

// aws:PrincipalTag/<key> is the IAM user's tag.
func TestBucketPolicyPrincipalTag(t *testing.T) {
	e := startPolicyEnv(t)
	dev := e.user(t)
	e.putBucketPolicy(t, statement("Allow", `"*"`, "s3:GetObject", "arn:aws:s3:::bkt/*",
		`{"StringEquals":{"aws:PrincipalTag/team":"storage"}}`))
	requireDenied(t, getObject(t, dev, "public/a"), notAllowed(devARN, "s3:GetObject",
		"arn:aws:s3:::bkt/public/a"))
	_, err := e.ic.TagUser(t.Context(), &iam.TagUserInput{
		UserName: aws.String("dev"),
		Tags:     []iamtypes.Tag{{Key: aws.String("team"), Value: aws.String("storage")}},
	})
	requireNoErr(t, err, "tag user")
	requireNoErr(t, getObject(t, dev, "public/a"), "tagged dev get")
}
