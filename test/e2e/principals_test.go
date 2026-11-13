//go:build e2e

package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	awsorg "github.com/aws/aws-sdk-go-v2/service/organizations"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// The tests here check that bucket policies name users and roles by unique
// ID, as S3 stores them, and that aws:PrincipalOrgID and
// aws:PrincipalOrgPaths are supplied, against the real identity and
// accounts services.

// getPolicy returns bkt's policy, read with the root key.
func (e *policyEnv) getPolicy(t *testing.T) string {
	t.Helper()
	out, err := e.root.GetBucketPolicy(t.Context(), &s3.GetBucketPolicyInput{
		Bucket: aws.String("bkt"),
	})
	requireNoErr(t, err, "get bucket policy")
	return aws.ToString(out.Policy)
}

// grantDoc is a bucket policy allowing the AWS principal s3:GetObject on
// bkt's objects.
func grantDoc(principal string) string {
	return `{"Version":"2012-10-17","Statement":[` +
		statement("Allow", `{"AWS":"`+principal+`"}`, "s3:GetObject", "arn:aws:s3:::bkt/*", "") +
		`]}`
}

// A grant to an IAM user names it by its unique ID: shown as its ARN while
// it exists, it does not pass to a user created again under the same name,
// and is then shown as the old user's ID.
func TestBucketPolicyNamesAUserByItsUniqueID(t *testing.T) {
	e := startPolicyEnv(t)
	ctx := t.Context()
	dev := e.user(t)
	got, err := e.ic.GetUser(ctx, &iam.GetUserInput{UserName: aws.String("dev")})
	requireNoErr(t, err, "get user")
	oldID := aws.ToString(got.User.UserId)

	e.putBucketPolicy(t, statement("Allow", `{"AWS":"`+devARN+`"}`, "s3:GetObject",
		"arn:aws:s3:::bkt/*", ""))
	requireEqual(t, e.getPolicy(t), grantDoc(devARN), "the policy while dev exists")
	requireNoErr(t, getObject(t, dev, "public/a"), "dev, granted")

	keys, err := e.ic.ListAccessKeys(ctx, &iam.ListAccessKeysInput{UserName: aws.String("dev")})
	requireNoErr(t, err, "list dev's keys")
	for _, k := range keys.AccessKeyMetadata {
		_, err = e.ic.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{
			UserName: aws.String("dev"), AccessKeyId: k.AccessKeyId,
		})
		requireNoErr(t, err, "delete dev's key")
	}
	_, err = e.ic.DeleteUser(ctx, &iam.DeleteUserInput{UserName: aws.String("dev")})
	requireNoErr(t, err, "delete dev")
	requireEqual(t, e.getPolicy(t), grantDoc(oldID), "the policy once dev is gone")

	again := e.user(t)
	requireDenied(t, getObject(t, again, "public/a"),
		notAllowed(devARN, "s3:GetObject", "arn:aws:s3:::bkt/public/a"))
	requireEqual(t, e.getPolicy(t), grantDoc(oldID), "the policy after dev is created again")

	// Saved again, the ARN binds to the new user.
	e.putBucketPolicy(t, statement("Allow", `{"AWS":"`+devARN+`"}`, "s3:GetObject",
		"arn:aws:s3:::bkt/*", ""))
	requireNoErr(t, getObject(t, again, "public/a"), "the new dev, granted anew")
}

// A role ARN is bound like a user's: a session of the role is granted by
// it, and the policy is shown with the role's ARN.
func TestBucketPolicyNamesARoleByItsUniqueID(t *testing.T) {
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
	e.putBucketPolicy(t, statement("Allow", `{"AWS":"`+roleARN+`"}`, "s3:GetObject",
		"arn:aws:s3:::bkt/*", ""))
	requireEqual(t, e.getPolicy(t), grantDoc(roleARN), "the policy while the role exists")

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
	requireNoErr(t, getObject(t, session, "public/a"), "session, granted by the role's ID")
}

// A policy naming a user or role that does not exist is refused as S3
// refuses it, and the stored policy is kept.
func TestBucketPolicyRefusesAPrincipalThatDoesNotExist(t *testing.T) {
	e := startPolicyEnv(t)
	e.putBucketPolicy(t, statement("Allow", `"*"`, "s3:GetObject", "arn:aws:s3:::bkt/*", ""))
	before := e.getPolicy(t)
	for _, arn := range []string{
		"arn:aws:iam::000000000000:user/nobody",
		"arn:aws:iam::000000000000:role/nobody",
	} {
		_, err := e.root.PutBucketPolicy(t.Context(), &s3.PutBucketPolicyInput{
			Bucket: aws.String("bkt"), Policy: aws.String(grantDoc(arn)),
		})
		requireAPIError(t, err, "MalformedPolicy", "Invalid principal in policy")
		requireResponse(t, err, http.StatusBadRequest)
	}
	requireEqual(t, e.getPolicy(t), before, "the stored policy")
}

// Standalone, a policy is stored and shown as sent: nothing is bound, and a
// user ARN is not checked.
func TestBucketPolicyStandaloneIsStoredAsSent(t *testing.T) {
	c := startClient(t, false)
	ctx := t.Context()
	createBucket(t, c, "bkt")
	doc := grantDoc("arn:aws:iam::000000000000:user/nobody")
	_, err := c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String("bkt"), Policy: aws.String(doc),
	})
	requireNoErr(t, err, "put bucket policy")
	out, err := c.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String("bkt")})
	requireNoErr(t, err, "get bucket policy")
	requireEqual(t, aws.ToString(out.Policy), doc, "the policy")
}

// organization is the accounts service with an organization and one
// member account, and identity using it.
type organization struct {
	oc       *awsorg.Client
	id       string
	root     string
	account  string
	identity string
}

func startOrganization(t *testing.T) *organization {
	t.Helper()
	accounts := startSibling(t, "accounts", accountsDirEnv)
	oc := awsorg.New(awsorg.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(accounts.URL),
		Credentials:      credentials.StaticCredentialsProvider{Value: rootCreds()},
		HTTPClient:       &http.Client{Timeout: serviceTimeout},
		RetryMaxAttempts: 1,
	})
	ctx := t.Context()
	org, err := oc.CreateOrganization(ctx, &awsorg.CreateOrganizationInput{})
	requireNoErr(t, err, "create organization")
	roots, err := oc.ListRoots(ctx, &awsorg.ListRootsInput{})
	requireNoErr(t, err, "list roots")
	created, err := oc.CreateAccount(ctx, &awsorg.CreateAccountInput{
		AccountName: aws.String("Member"), Email: aws.String("member@example.com"),
	})
	requireNoErr(t, err, "create account")
	st := created.CreateAccountStatus
	if st == nil || st.State != orgtypes.CreateAccountStateSucceeded {
		t.Fatalf("create account: %+v", st)
	}
	identity := startSibling(t, "identity", identityDirEnv,
		"TULE_Identity_AccountsEndpoint="+accounts.URL)
	return &organization{
		oc: oc, id: aws.ToString(org.Organization.Id), root: aws.ToString(roots.Roots[0].Id),
		account: aws.ToString(st.AccountId), identity: identity.URL,
	}
}

// memberSession returns credentials for a session of the member account's
// OrganizationAccountAccessRole named e2e, assumed with the root key.
func (o *organization) memberSession(t *testing.T) aws.Credentials {
	t.Helper()
	sc := sts.New(sts.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(o.identity),
		Credentials:      credentials.StaticCredentialsProvider{Value: rootCreds()},
		HTTPClient:       &http.Client{Timeout: serviceTimeout},
		RetryMaxAttempts: 1,
	})
	assumed, err := sc.AssumeRole(t.Context(), &sts.AssumeRoleInput{
		RoleArn: aws.String(
			"arn:aws:iam::" + o.account + ":role/OrganizationAccountAccessRole"),
		RoleSessionName: aws.String("e2e"),
	})
	requireNoErr(t, err, "assume role")
	return aws.Credentials{
		AccessKeyID:     aws.ToString(assumed.Credentials.AccessKeyId),
		SecretAccessKey: aws.ToString(assumed.Credentials.SecretAccessKey),
		SessionToken:    aws.ToString(assumed.Credentials.SessionToken),
	}
}

// orgWaitLimit bounds how long the test waits for identity's organization
// keys to follow an account's removal: identity keeps them for 30 seconds.
const orgWaitLimit = 45 * time.Second

// A bucket policy conditioned on aws:PrincipalOrgID and
// aws:PrincipalOrgPaths allows a member account's session and refuses it
// once the account is removed from the organization.
func TestBucketPolicyOrganizationKeys(t *testing.T) {
	o := startOrganization(t)
	ctx := t.Context()
	srv := startWithIdentity(t, o.identity)
	root := s3As(srv, rootCreds())
	createBucket(t, root, "bkt")
	put(t, root, "bkt", "public/a", []byte("a"))
	_, err := root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String("bkt"),
		Policy: aws.String(`{"Version":"2012-10-17","Statement":[` + statement("Allow", `"*"`,
			"s3:GetObject", "arn:aws:s3:::bkt/*",
			`{"StringEquals":{"aws:PrincipalOrgID":"`+o.id+`"},`+
				`"ForAnyValue:StringLike":{"aws:PrincipalOrgPaths":"`+o.id+`/`+o.root+`/*"}}`) +
			`]}`),
	})
	requireNoErr(t, err, "put bucket policy")

	// The session's role has AdministratorAccess, the identity Allow a
	// principal of another account also needs.
	session := s3As(srv, o.memberSession(t))
	requireNoErr(t, getObject(t, session, "public/a"), "a member account's session")

	_, err = o.oc.RemoveAccountFromOrganization(ctx, &awsorg.RemoveAccountFromOrganizationInput{
		AccountId: aws.String(o.account),
	})
	requireNoErr(t, err, "remove the account from the organization")
	deadline := time.Now().Add(orgWaitLimit)
	for {
		err = getObject(t, session, "public/a")
		if err != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	requireDenied(t, err, denied(
		"arn:aws:sts::"+o.account+":assumed-role/OrganizationAccountAccessRole/e2e",
		"s3:GetObject", "arn:aws:s3:::bkt/public/a",
		"because no resource-based policy allows the s3:GetObject action"))
}
