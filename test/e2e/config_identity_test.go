//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// The tests here run the live server against the real identity service
// and check that the bucket configuration operations, and the actions
// ACL and Object Ownership headers add, are evaluated as S3 evaluates
// them, and that RestrictPublicBuckets takes a public policy's grants
// away from other accounts.

func TestBucketConfigAuthorization(t *testing.T) {
	identity := startSibling(t, "identity", identityDirEnv)
	l := startLiveWithIdentity(t, identity.URL)
	ic := iamClient(identity.URL)
	ctx := t.Context()
	root := l.client(rootCreds())
	createBucket(t, root, "bkt")

	key := createUserKey(t, ic, "dev")
	putUserPolicy(t, ic, "dev", "p0", allowDoc(`["s3:GetBucketTagging","s3:GetBucketAcl"]`, "*"))
	dev := l.client(key)

	_, err := dev.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: aws.String("bkt"), Tagging: tags("team", "storage"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutBucketTagging", "arn:aws:s3:::bkt"))
	// DeleteBucketTagging is evaluated as s3:PutBucketTagging.
	_, err = dev.DeleteBucketTagging(ctx, &s3.DeleteBucketTaggingInput{Bucket: aws.String("bkt")})
	requireDenied(t, err, notAllowed(devARN, "s3:PutBucketTagging", "arn:aws:s3:::bkt"))
	_, err = dev.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String("bkt")})
	requireCode(t, err, "NoSuchTagSet")
	for name, call := range map[string]func() error{
		"s3:PutBucketCORS": func() error {
			_, e := dev.DeleteBucketCors(ctx, &s3.DeleteBucketCorsInput{Bucket: aws.String("bkt")})
			return e //nolint:wrapcheck // the test inspects the SDK error
		},
		"s3:PutBucketPublicAccessBlock": func() error {
			_, e := dev.DeletePublicAccessBlock(ctx,
				&s3.DeletePublicAccessBlockInput{Bucket: aws.String("bkt")})
			return e //nolint:wrapcheck // the test inspects the SDK error
		},
		"s3:GetBucketPolicyStatus": func() error {
			_, e := dev.GetBucketPolicyStatus(ctx,
				&s3.GetBucketPolicyStatusInput{Bucket: aws.String("bkt")})
			return e //nolint:wrapcheck // the test inspects the SDK error
		},
		"s3:PutEncryptionConfiguration": func() error {
			_, e := dev.DeleteBucketEncryption(ctx,
				&s3.DeleteBucketEncryptionInput{Bucket: aws.String("bkt")})
			return e //nolint:wrapcheck // the test inspects the SDK error
		},
		"s3:GetEncryptionConfiguration": func() error {
			_, e := dev.GetBucketEncryption(ctx,
				&s3.GetBucketEncryptionInput{Bucket: aws.String("bkt")})
			return e //nolint:wrapcheck // the test inspects the SDK error
		},
		"s3:PutBucketOwnershipControls": func() error {
			_, e := dev.DeleteBucketOwnershipControls(ctx,
				&s3.DeleteBucketOwnershipControlsInput{Bucket: aws.String("bkt")})
			return e //nolint:wrapcheck // the test inspects the SDK error
		},
	} {
		requireDenied(t, call(), notAllowed(devARN, name, "arn:aws:s3:::bkt"))
	}

	// The bucket policy can grant them within the account.
	_, err = root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String("bkt"),
		Policy: aws.String(`{"Version":"2012-10-17","Statement":[` + statement("Allow",
			`{"AWS":"`+devARN+`"}`, "s3:PutBucketTagging", "arn:aws:s3:::bkt", "") + `]}`),
	})
	requireNoErr(t, err, "grant dev s3:PutBucketTagging")
	_, err = dev.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: aws.String("bkt"), Tagging: tags("team", "storage"),
	})
	requireNoErr(t, err, "dev puts tags, granted by the bucket policy")
	acl, err := dev.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: aws.String("bkt")})
	requireNoErr(t, err, "dev gets the bucket acl")
	requireEqual(t, len(acl.Grants), 1, "bucket acl grants")
}

// A write with an ACL also needs s3:PutObjectAcl, and CreateBucket with an
// Object Ownership s3:PutBucketOwnershipControls and with an ACL other than
// private s3:PutBucketAcl.
func TestACLHeaderAuthorization(t *testing.T) {
	identity := startSibling(t, "identity", identityDirEnv)
	l := startLiveWithIdentity(t, identity.URL)
	ic := iamClient(identity.URL)
	ctx := t.Context()
	createBucket(t, l.client(rootCreds()), "bkt")
	key := createUserKey(t, ic, "dev")
	putUserPolicy(t, ic, "dev", "p0", allowDoc(
		`["s3:PutObject","s3:GetObject","s3:CreateBucket"]`, "*"))
	dev := l.client(key)

	// The canned ACLs the bucket owner's full control comes as need only
	// s3:PutObject; another ACL, and any grant header, s3:PutObjectAcl.
	for _, acl := range []types.ObjectCannedACL{
		types.ObjectCannedACLBucketOwnerFullControl, types.ObjectCannedACLPrivate,
	} {
		_, err := dev.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("k"), ACL: acl,
		})
		requireNoErr(t, err, "PutObject "+string(acl))
	}
	_, err := dev.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("k"),
		ACL: types.ObjectCannedACLAuthenticatedRead,
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutObjectAcl", "arn:aws:s3:::bkt/k"))
	_, err = dev.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("k"),
		GrantFullControl: aws.String(`id="` + strings.Repeat("0", 64) + `"`),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutObjectAcl", "arn:aws:s3:::bkt/k"))
	put(t, dev, "bkt", "k", []byte("k"))
	_, err = dev.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("c"), CopySource: aws.String("bkt/k"),
		ACL: types.ObjectCannedACLPublicRead,
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutObjectAcl", "arn:aws:s3:::bkt/c"))
	_, err = dev.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("bkt"), Key: aws.String("m"),
		GrantRead: aws.String(`uri="http://acs.amazonaws.com/groups/global/AllUsers"`),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutObjectAcl", "arn:aws:s3:::bkt/m"))
	_, err = dev.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("c"), CopySource: aws.String("bkt/k"),
		ACL: types.ObjectCannedACLBucketOwnerFullControl,
	})
	requireNoErr(t, err, "CopyObject bucket-owner-full-control")

	// CreateBucket with an Object Ownership needs
	// s3:PutBucketOwnershipControls, and with a non-private ACL
	// s3:PutBucketAcl; private needs neither.
	_, err = dev.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("owned"), ObjectOwnership: types.ObjectOwnershipObjectWriter,
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutBucketOwnershipControls",
		"arn:aws:s3:::owned"))
	_, err = dev.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("owned"), ACL: types.BucketCannedACLAuthenticatedRead,
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutBucketAcl", "arn:aws:s3:::owned"))
	_, err = dev.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("owned"), ACL: types.BucketCannedACLPrivate,
	})
	requireNoErr(t, err, "CreateBucket private")
}

// A public policy under RestrictPublicBuckets grants nothing to another
// account, not even what a non-public statement delegates to it, while
// the owner's account keeps what it grants. Without the public statement,
// or without the setting, the other account's grant applies again ("How
// Amazon S3 evaluates a bucket policy that contains both public and
// non-public access grants", S3 User Guide).
func TestRestrictPublicBuckets(t *testing.T) {
	account, identityURL := startMemberAccount(t)
	l := startLiveWithIdentity(t, identityURL)
	ctx := t.Context()
	root := l.client(rootCreds())
	createBucket(t, root, "bkt")
	put(t, root, "bkt", "public/a", []byte("a"))

	// The session's role has AdministratorAccess, the identity Allow a
	// principal of another account also needs.
	member := l.client(assumeMemberRole(t, identityURL, account))
	sessionARN := "arn:aws:sts::" + account + ":assumed-role/OrganizationAccountAccessRole/e2e"
	ic := iamClient(identityURL)
	dev := l.client(createUserKey(t, ic, "dev"))

	toAccount := statement("Allow", `{"AWS":"`+account+`"}`, "s3:GetObject",
		"arn:aws:s3:::bkt/public/*", "")
	toAll := statement("Allow", `"*"`, "s3:GetObject", "arn:aws:s3:::bkt/public/*", "")
	putPolicy := func(statements ...string) {
		t.Helper()
		_, pErr := root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
			Bucket: aws.String("bkt"),
			Policy: aws.String(`{"Version":"2012-10-17","Statement":[` +
				strings.Join(statements, ",") + `]}`),
		})
		requireNoErr(t, pErr, "put bucket policy")
	}
	restrict := func(on bool) {
		t.Helper()
		_, pErr := root.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
			Bucket: aws.String("bkt"),
			PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{
				RestrictPublicBuckets: aws.Bool(on),
			},
		})
		requireNoErr(t, pErr, "put public access block")
	}
	restrictedDenial := denied(sessionARN, "s3:GetObject", "arn:aws:s3:::bkt/public/a",
		"because no resource-based policy allows the s3:GetObject action")

	// The new bucket's BlockPublicPolicy is lifted, its
	// RestrictPublicBuckets kept.
	restrict(true)
	putPolicy(toAccount, toAll)
	requireDenied(t, getObject(t, member, "public/a"), restrictedDenial)
	requireNoErr(t, getObject(t, dev, "public/a"),
		"the owner's account, granted by the public statement")

	restrict(false)
	requireNoErr(t, getObject(t, member, "public/a"), "another account, unrestricted")

	restrict(true)
	requireDenied(t, getObject(t, member, "public/a"), restrictedDenial)
	putPolicy(toAccount)
	requireNoErr(t, getObject(t, member, "public/a"),
		"another account, granted by a non-public policy")
}

// Another account uploads with the bucket-owner-full-control canned ACL
// under the bucket policy of the S3 User Guide's "Grant cross-account
// permissions to upload objects while ensuring that the bucket owner has
// full control" ("Examples of Amazon S3 bucket policies"), which allows
// s3:PutObject only with that ACL.
func TestCrossAccountUploadWithBucketOwnerFullControl(t *testing.T) {
	account, identityURL := startMemberAccount(t)
	l := startLiveWithIdentity(t, identityURL)
	ctx := t.Context()
	root := l.client(rootCreds())
	createBucket(t, root, "bkt")
	_, err := root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String("bkt"),
		Policy: aws.String(`{"Version":"2012-10-17","Statement":[` + statement("Allow",
			`{"AWS":"`+account+`"}`, "s3:PutObject", "arn:aws:s3:::bkt/*",
			`{"StringEquals":{"s3:x-amz-acl":"bucket-owner-full-control"}}`) + `]}`),
	})
	requireNoErr(t, err, "put the example policy")
	member := l.client(assumeMemberRole(t, identityURL, account))

	_, err = member.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("up"), Body: strings.NewReader("up"),
		ACL: types.ObjectCannedACLBucketOwnerFullControl,
	})
	requireNoErr(t, err, "upload with bucket-owner-full-control")
	_, err = member.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("up"), Body: strings.NewReader("up"),
	})
	requireDenied(t, err, denied(
		"arn:aws:sts::"+account+":assumed-role/OrganizationAccountAccessRole/e2e",
		"s3:PutObject", "arn:aws:s3:::bkt/up",
		"because no resource-based policy allows the s3:PutObject action"))
	acl, err := root.GetObjectAcl(ctx, &s3.GetObjectAclInput{
		Bucket: aws.String("bkt"), Key: aws.String("up"),
	})
	requireNoErr(t, err, "get the uploaded object's acl")
	requireEqual(t, len(acl.Grants), 1, "the bucket owner's full control")
}

// assumeMemberRole returns credentials for a session named e2e of
// account's OrganizationAccountAccessRole, assumed with the root key.
func assumeMemberRole(t *testing.T, identityURL, account string) aws.Credentials {
	t.Helper()
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
	return aws.Credentials{
		AccessKeyID:     aws.ToString(assumed.Credentials.AccessKeyId),
		SecretAccessKey: aws.ToString(assumed.Credentials.SecretAccessKey),
		SessionToken:    aws.ToString(assumed.Credentials.SessionToken),
	}
}
