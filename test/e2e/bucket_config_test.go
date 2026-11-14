//go:build e2e

package e2e

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// The tests here drive the bucket configuration subresources with the SDK
// against the live server: tagging, Block Public Access and policy status,
// Object Ownership and ACLs, and default encryption.

func tags(kv ...string) *types.Tagging {
	var set []types.Tag
	for i := 0; i+1 < len(kv); i += 2 {
		set = append(set, types.Tag{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
	}
	return &types.Tagging{TagSet: set}
}

func TestBucketTagging(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "tagged")
	bucket := aws.String("tagged")

	_, err := c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: bucket})
	requireCode(t, err, "NoSuchTagSet")
	requireResponse(t, err, http.StatusNotFound)

	_, err = c.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: bucket, Tagging: tags("team", "storage", "cost-center", "4 2/x:y=z+@_.-", "e", ""),
	})
	requireNoErr(t, err, "put bucket tagging")
	got, err := c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: bucket})
	requireNoErr(t, err, "get bucket tagging")
	requireEqual(t, len(got.TagSet), 3, "tags")
	requireEqual(t, aws.ToString(got.TagSet[1].Value), "4 2/x:y=z+@_.-", "tag value")
	requireEqual(t, aws.ToString(got.TagSet[2].Value), "", "empty tag value")

	_, err = c.DeleteBucketTagging(ctx, &s3.DeleteBucketTaggingInput{Bucket: bucket})
	requireNoErr(t, err, "delete bucket tagging")
	_, err = c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: bucket})
	requireCode(t, err, "NoSuchTagSet")

	// A bucket's configuration goes with it.
	_, err = c.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: bucket, Tagging: tags("team", "storage"),
	})
	requireNoErr(t, err, "tag again")
	_, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: bucket})
	requireNoErr(t, err, "delete bucket")
	createBucket(t, c, "tagged")
	_, err = c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: bucket})
	requireCode(t, err, "NoSuchTagSet")

	_, err = c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String("missing")})
	requireCode(t, err, "NoSuchBucket")
}

func TestBucketTaggingValidation(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "tagged")
	bucket := aws.String("tagged")

	fifty := make([]string, 0, 102)
	for i := range 51 {
		fifty = append(fifty, "k"+strconv.Itoa(i), "v")
	}
	for name, tagging := range map[string]*types.Tagging{
		"51 tags":                tags(fifty...),
		"a 129-character key":    tags(strings.Repeat("k", 129), "v"),
		"a 257-character value":  tags("k", strings.Repeat("v", 257)),
		"an empty key":           tags("", "v"),
		"a key with a !":         tags("a!", "v"),
		"a value with a ;":       tags("k", "a;b"),
		"the aws: prefix":        tags("aws:createdBy", "me"),
		"the aws: prefix, upper": tags("AWS:createdBy", "me"),
		"a key given twice":      tags("k", "1", "k", "2"),
	} {
		_, err := c.PutBucketTagging(
			ctx,
			&s3.PutBucketTaggingInput{Bucket: bucket, Tagging: tagging},
		)
		requireCode(t, err, "InvalidTag")
		requireResponse(t, err, http.StatusBadRequest)
		t.Logf("%s: refused", name)
	}
	// 50 tags, and 128- and 256-character keys and values in any script,
	// are accepted.
	_, err := c.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: bucket, Tagging: tags(fifty[:100]...),
	})
	requireNoErr(t, err, "50 tags")
	_, err = c.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: bucket, Tagging: tags(strings.Repeat("é", 128), strings.Repeat("ü", 256)),
	})
	requireNoErr(t, err, "the longest key and value")
}

const publicRead = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*",` +
	`"Action":"s3:GetObject","Resource":"arn:aws:s3:::pab/*"}]}`

const accountRead = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
	`"Principal":{"AWS":"111111111111"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::pab/*"}]}`

// A new bucket, a startup bucket too, has all four Block Public Access
// settings on and ACLs disabled.
func TestNewBucketDefaults(t *testing.T) {
	l := startLive(t, map[string]string{"TULE_Storage_Buckets": "uploads"})
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "pab")
	for _, name := range []string{"pab", "uploads"} {
		bucket := aws.String(name)
		block, err := c.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: bucket})
		requireNoErr(t, err, "get public access block")
		cfg := block.PublicAccessBlockConfiguration
		requireEqual(t, aws.ToBool(cfg.BlockPublicAcls) && aws.ToBool(cfg.IgnorePublicAcls) &&
			aws.ToBool(cfg.BlockPublicPolicy) && aws.ToBool(cfg.RestrictPublicBuckets), true,
			name+": all four settings on")
		controls, err := c.GetBucketOwnershipControls(ctx,
			&s3.GetBucketOwnershipControlsInput{Bucket: bucket})
		requireNoErr(t, err, "get ownership controls")
		requireEqual(t, controls.OwnershipControls.Rules[0].ObjectOwnership,
			types.ObjectOwnershipBucketOwnerEnforced, name+": ownership")
	}
}

func TestBlockPublicAccess(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "pab")
	bucket := aws.String("pab")

	_, err := c.GetBucketPolicyStatus(ctx, &s3.GetBucketPolicyStatusInput{Bucket: bucket})
	requireCode(t, err, "NoSuchBucketPolicy")

	// BlockPublicPolicy refuses a public policy, for the root key too, and
	// lets a non-public one through.
	_, err = c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: bucket, Policy: aws.String(publicRead),
	})
	requireAPIError(t, err, "AccessDenied", "Access Denied")
	requireResponse(t, err, http.StatusForbidden)
	_, err = c.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: bucket})
	requireCode(t, err, "NoSuchBucketPolicy")
	_, err = c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: bucket, Policy: aws.String(accountRead),
	})
	requireNoErr(t, err, "put a non-public policy")
	status, err := c.GetBucketPolicyStatus(ctx, &s3.GetBucketPolicyStatusInput{Bucket: bucket})
	requireNoErr(t, err, "policy status")
	requireEqual(t, aws.ToBool(status.PolicyStatus.IsPublic), false, "non-public policy")
}

func TestBlockPublicAccessSettings(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "pab")
	bucket := aws.String("pab")

	// With BlockPublicPolicy off, the public policy is stored.
	_, err := c.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: bucket, PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{
			BlockPublicAcls: aws.Bool(true), RestrictPublicBuckets: aws.Bool(true),
		},
	})
	requireNoErr(t, err, "put public access block")
	block, err := c.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: bucket})
	requireNoErr(t, err, "get public access block again")
	cfg := block.PublicAccessBlockConfiguration
	requireEqual(t, aws.ToBool(cfg.BlockPublicAcls) && !aws.ToBool(cfg.IgnorePublicAcls) &&
		!aws.ToBool(cfg.BlockPublicPolicy) && aws.ToBool(cfg.RestrictPublicBuckets), true,
		"the settings as put")
	_, err = c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: bucket, Policy: aws.String(publicRead),
	})
	requireNoErr(t, err, "put the public policy")
	status, err := c.GetBucketPolicyStatus(ctx, &s3.GetBucketPolicyStatusInput{Bucket: bucket})
	requireNoErr(t, err, "policy status of the public policy")
	requireEqual(t, aws.ToBool(status.PolicyStatus.IsPublic), true, "public policy")

	// A wildcard principal narrowed by a fixed condition is not public.
	narrowed := strings.Replace(publicRead, `"Resource"`,
		`"Condition":{"StringEquals":{"aws:SourceVpce":"vpce-1a2b3c4d"}},"Resource"`, 1)
	_, err = c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: bucket, Policy: aws.String(narrowed),
	})
	requireNoErr(t, err, "put the narrowed policy")
	status, err = c.GetBucketPolicyStatus(ctx, &s3.GetBucketPolicyStatusInput{Bucket: bucket})
	requireNoErr(t, err, "policy status of the narrowed policy")
	requireEqual(t, aws.ToBool(status.PolicyStatus.IsPublic), false, "narrowed policy")

	_, err = c.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{Bucket: bucket})
	requireNoErr(t, err, "delete public access block")
	_, err = c.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: bucket})
	requireCode(t, err, "NoSuchPublicAccessBlockConfiguration")
	requireResponse(t, err, http.StatusNotFound)
}

// ownerGrant reports whether acl is the owner's FULL_CONTROL alone.
func ownerGrant(t *testing.T, owner *types.Owner, grants []types.Grant) {
	t.Helper()
	requireEqual(t, len(grants), 1, "grants")
	requireEqual(t, aws.ToString(grants[0].Grantee.ID), aws.ToString(owner.ID), "grantee")
	requireEqual(t, grants[0].Grantee.Type, types.TypeCanonicalUser, "grantee type")
	requireEqual(t, grants[0].Permission, types.PermissionFullControl, "permission")
}

func TestACLsDisabled(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "enforced")
	bucket := aws.String("enforced")
	put(t, c, "enforced", "k", []byte("k"))

	acl, err := c.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: bucket})
	requireNoErr(t, err, "get bucket acl")
	owner := aws.ToString(acl.Owner.ID)
	requireEqual(t, len(owner), 64, "canonical owner ID")
	ownerGrant(t, acl.Owner, acl.Grants)
	objACL, err := c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: bucket, Key: aws.String("k")})
	requireNoErr(t, err, "get object acl")
	ownerGrant(t, acl.Owner, objACL.Grants)

	requireACLsRefused(t, c)
	requireOwnerACLsAccepted(t, c, acl.Owner)
}

// aclRefused checks that err is S3's refusal of an ACL by a bucket with
// ACLs disabled.
func aclRefused(t *testing.T, what string, err error) {
	t.Helper()
	requireAPIError(t, err, "AccessControlListNotSupported", "The bucket does not allow ACLs")
	requireResponse(t, err, http.StatusBadRequest)
	t.Logf("%s: refused", what)
}

// requireACLsRefused checks that the bucket "enforced" refuses every ACL
// but its owner's full control, and writes nothing it refuses.
func requireACLsRefused(t *testing.T, c *s3.Client) {
	t.Helper()
	ctx := t.Context()
	bucket := aws.String("enforced")
	refused := func(what string, err error) { t.Helper(); aclRefused(t, what, err) }
	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("p"), Body: strings.NewReader("p"),
		ACL: types.ObjectCannedACLPublicRead,
	})
	refused("PutObject public-read", err)
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("p"), Body: strings.NewReader("p"),
		GrantRead: aws.String(`uri="http://acs.amazonaws.com/groups/global/AllUsers"`),
	})
	refused("PutObject granting AllUsers", err)
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("p"), Body: strings.NewReader("p"),
		GrantFullControl: aws.String(`id="` + strings.Repeat("0", 64) + `"`),
	})
	refused("PutObject granting another account", err)
	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: aws.String("p"), CopySource: aws.String("enforced/k"),
		ACL: types.ObjectCannedACLAuthenticatedRead,
	})
	refused("CopyObject authenticated-read", err)
	_, err = c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: bucket, Key: aws.String("p"), ACL: types.ObjectCannedACLPublicReadWrite,
	})
	refused("CreateMultipartUpload public-read-write", err)
	_, err = c.PutObjectAcl(ctx, &s3.PutObjectAclInput{
		Bucket: bucket, Key: aws.String("k"), ACL: types.ObjectCannedACLPublicRead,
	})
	refused("PutObjectAcl public-read", err)
	_, err = c.PutBucketAcl(ctx, &s3.PutBucketAclInput{
		Bucket: bucket, ACL: types.BucketCannedACLPublicRead,
	})
	refused("PutBucketAcl public-read", err)
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String("p")})
	requireResponse(t, err, http.StatusNotFound)
}

// requireOwnerACLsAccepted checks that the bucket "enforced" accepts the
// full control of its owner, as a canned ACL, as private, or as the
// equivalent grant or document.
func requireOwnerACLsAccepted(t *testing.T, c *s3.Client, ownerOf *types.Owner) {
	t.Helper()
	ctx := t.Context()
	bucket := aws.String("enforced")
	owner := aws.ToString(ownerOf.ID)
	for i, in := range []*s3.PutObjectInput{
		{ACL: types.ObjectCannedACLBucketOwnerFullControl},
		{ACL: types.ObjectCannedACLPrivate},
		{GrantFullControl: aws.String(`id="` + owner + `"`)},
	} {
		in.Bucket, in.Key, in.Body = bucket, aws.String(
			"ok"+strconv.Itoa(i),
		), strings.NewReader(
			"ok",
		)
		_, err := c.PutObject(ctx, in)
		requireNoErr(t, err, "an owner full control ACL")
	}
	_, err := c.PutBucketAcl(ctx, &s3.PutBucketAclInput{
		Bucket: bucket, ACL: types.BucketCannedACLPrivate,
	})
	requireNoErr(t, err, "PutBucketAcl private")
	_, err = c.PutBucketAcl(ctx, &s3.PutBucketAclInput{
		Bucket: bucket, AccessControlPolicy: &types.AccessControlPolicy{
			Owner: ownerOf,
			Grants: []types.Grant{{
				Grantee:    &types.Grantee{Type: types.TypeCanonicalUser, ID: aws.String(owner)},
				Permission: types.PermissionFullControl,
			}},
		},
	})
	requireNoErr(t, err, "PutBucketAcl with the owner's full control as XML")
	_, err = c.PutObjectAcl(ctx, &s3.PutObjectAclInput{
		Bucket: bucket, Key: aws.String("k"), ACL: types.ObjectCannedACLBucketOwnerFullControl,
	})
	requireNoErr(t, err, "PutObjectAcl bucket-owner-full-control")
	_, err = c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: bucket, Key: aws.String("none")})
	requireCode(t, err, "NoSuchKey")
}

// CreateBucket refuses an ACL for a bucket that will have ACLs disabled,
// and a public one for any new bucket, which blocks public ACLs.
func TestCreateBucketACLs(t *testing.T) {
	c := startLive(t, nil).testClient()
	ctx := t.Context()
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("with-acl"), ACL: types.BucketCannedACLPublicRead,
	})
	requireCode(t, err, "InvalidBucketAclWithObjectOwnership")
	requireResponse(t, err, http.StatusBadRequest)
	_, err = c.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("with-acl"), ACL: types.BucketCannedACLPublicRead,
		ObjectOwnership: types.ObjectOwnershipObjectWriter,
	})
	requireAPIError(t, err, "InvalidBucketAclWithBlockPublicAccess",
		"Bucket cannot have public ACLs set with BlockPublicAccess enabled")
	_, err = c.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("with-acl"), ACL: types.BucketCannedACLPrivate,
	})
	requireNoErr(t, err, "CreateBucket private")
}

func TestACLsEnabled(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	bucket := aws.String("writer")
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: bucket, ObjectOwnership: types.ObjectOwnershipObjectWriter,
	})
	requireNoErr(t, err, "create an ObjectWriter bucket")
	controls, err := c.GetBucketOwnershipControls(ctx,
		&s3.GetBucketOwnershipControlsInput{Bucket: bucket})
	requireNoErr(t, err, "get ownership controls")
	requireEqual(t, controls.OwnershipControls.Rules[0].ObjectOwnership,
		types.ObjectOwnershipObjectWriter, "ownership")

	// BlockPublicAcls, on for a new bucket, refuses a public ACL.
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("p"), Body: strings.NewReader("p"),
		ACL: types.ObjectCannedACLPublicRead,
	})
	requireCode(t, err, "AccessDenied")
	requireResponse(t, err, http.StatusForbidden)

	_, err = c.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{Bucket: bucket})
	requireNoErr(t, err, "delete public access block")
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("p"), Body: strings.NewReader("p"),
		ACL: types.ObjectCannedACLPublicRead,
	})
	requireNoErr(t, err, "put a public-read object")
	acl, err := c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: bucket, Key: aws.String("p")})
	requireNoErr(t, err, "get object acl")
	requireEqual(t, len(acl.Grants), 2, "public-read grants")
	requireEqual(t, aws.ToString(acl.Grants[1].Grantee.URI),
		"http://acs.amazonaws.com/groups/global/AllUsers", "AllUsers grantee")
	requireEqual(t, acl.Grants[1].Grantee.Type, types.TypeGroup, "group grantee type")
	requireEqual(t, acl.Grants[1].Permission, types.PermissionRead, "AllUsers permission")

	// Writing the object again without an ACL gives it the default.
	put(t, c, "writer", "p", []byte("again"))
	acl, err = c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: bucket, Key: aws.String("p")})
	requireNoErr(t, err, "get object acl after overwrite")
	requireEqual(t, len(acl.Grants), 1, "grants after overwrite")

	_, err = c.PutObjectAcl(ctx, &s3.PutObjectAclInput{
		Bucket: bucket, Key: aws.String("p"),
		GrantRead: aws.String(`uri="http://acs.amazonaws.com/groups/global/AuthenticatedUsers"`),
	})
	requireNoErr(t, err, "PutObjectAcl with a grant header")
	acl, err = c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: bucket, Key: aws.String("p")})
	requireNoErr(t, err, "get object acl after PutObjectAcl")
	requireEqual(t, len(acl.Grants), 1, "grants as granted")
	requireEqual(t, aws.ToString(acl.Grants[0].Grantee.URI),
		"http://acs.amazonaws.com/groups/global/AuthenticatedUsers", "granted group")

	requireBucketACLsEnabled(t, c)
}

// requireBucketACLsEnabled checks the bucket ACL of the ObjectWriter bucket
// "writer", whose object p has an ACL granting AuthenticatedUsers READ:
// IgnorePublicAcls hides public grants, BucketOwnerEnforced needs the
// bucket ACL to be private and hides the object's, and deleting the
// ownership controls restores it.
func requireBucketACLsEnabled(t *testing.T, c *s3.Client) {
	t.Helper()
	ctx := t.Context()
	bucket := aws.String("writer")
	_, err := c.PutBucketAcl(ctx, &s3.PutBucketAclInput{
		Bucket: bucket, ACL: types.BucketCannedACLPublicRead,
	})
	requireNoErr(t, err, "PutBucketAcl public-read")
	bucketACL, err := c.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: bucket})
	requireNoErr(t, err, "get bucket acl")
	requireEqual(t, len(bucketACL.Grants), 2, "bucket public-read grants")

	// IgnorePublicAcls hides the public grants, as S3 shows the effective
	// ACL.
	_, err = c.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: bucket, PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{
			IgnorePublicAcls: aws.Bool(true),
		},
	})
	requireNoErr(t, err, "put IgnorePublicAcls")
	bucketACL, err = c.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: bucket})
	requireNoErr(t, err, "get bucket acl under IgnorePublicAcls")
	ownerGrant(t, bucketACL.Owner, bucketACL.Grants)
	_, err = c.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{Bucket: bucket})
	requireNoErr(t, err, "delete IgnorePublicAcls")

	// BucketOwnerEnforced needs the bucket ACL to grant only the owner.
	enforce := &s3.PutBucketOwnershipControlsInput{
		Bucket: bucket, OwnershipControls: &types.OwnershipControls{
			Rules: []types.OwnershipControlsRule{
				{ObjectOwnership: types.ObjectOwnershipBucketOwnerEnforced},
			},
		},
	}
	_, err = c.PutBucketOwnershipControls(ctx, enforce)
	requireCode(t, err, "InvalidBucketAclWithObjectOwnership")
	_, err = c.PutBucketAcl(ctx, &s3.PutBucketAclInput{
		Bucket: bucket, ACL: types.BucketCannedACLPrivate,
	})
	requireNoErr(t, err, "PutBucketAcl private")
	_, err = c.PutBucketOwnershipControls(ctx, enforce)
	requireNoErr(t, err, "enforce bucket ownership")
	acl, err := c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: bucket, Key: aws.String("p")})
	requireNoErr(t, err, "get object acl once enforced")
	ownerGrant(t, bucketACL.Owner, acl.Grants)

	// Without ownership controls the bucket behaves as ObjectWriter.
	_, err = c.DeleteBucketOwnershipControls(ctx,
		&s3.DeleteBucketOwnershipControlsInput{Bucket: bucket})
	requireNoErr(t, err, "delete ownership controls")
	_, err = c.GetBucketOwnershipControls(ctx,
		&s3.GetBucketOwnershipControlsInput{Bucket: bucket})
	requireCode(t, err, "OwnershipControlsNotFoundError")
	requireResponse(t, err, http.StatusNotFound)
	acl, err = c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: bucket, Key: aws.String("p")})
	requireNoErr(t, err, "get object acl without controls")
	requireEqual(t, aws.ToString(acl.Grants[0].Grantee.URI),
		"http://acs.amazonaws.com/groups/global/AuthenticatedUsers", "the restored object ACL")
}

// keyID is the KMS key the encryption tests name; nothing checks it.
const keyID = "arn:aws:kms:us-east-1:000000000000:key/1234abcd-12ab-34cd-56ef-1234567890ab"

func TestDefaultEncryption(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "enc")
	bucket := aws.String("enc")

	enc, err := c.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: bucket})
	requireNoErr(t, err, "get bucket encryption")
	rule := enc.ServerSideEncryptionConfiguration.Rules[0]
	requireEqual(t, rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm,
		types.ServerSideEncryptionAes256, "default algorithm")
	requireEqual(t, aws.ToBool(rule.BucketKeyEnabled), false, "default bucket key")

	out := put(t, c, "enc", "plain", []byte("plain"))
	requireEqual(t, out.ServerSideEncryption, types.ServerSideEncryptionAes256, "put sse")
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String("plain")})
	requireNoErr(t, err, "head")
	requireEqual(t, head.ServerSideEncryption, types.ServerSideEncryptionAes256, "head sse")
	got, _ := get(t, c, &s3.GetObjectInput{Bucket: bucket, Key: aws.String("plain")})
	requireEqual(t, got.ServerSideEncryption, types.ServerSideEncryptionAes256, "get sse")

	_, err = c.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
		Bucket: bucket,
		ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
			Rules: []types.ServerSideEncryptionRule{{
				ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{
					SSEAlgorithm:   types.ServerSideEncryptionAwsKms,
					KMSMasterKeyID: aws.String(keyID),
				},
				BucketKeyEnabled: aws.Bool(true),
			}},
		},
	})
	requireNoErr(t, err, "put bucket encryption")
	enc, err = c.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: bucket})
	requireNoErr(t, err, "get bucket encryption as put")
	rule = enc.ServerSideEncryptionConfiguration.Rules[0]
	requireEqual(t, aws.ToString(rule.ApplyServerSideEncryptionByDefault.KMSMasterKeyID), keyID,
		"stored key")
	requireEqual(t, aws.ToBool(rule.BucketKeyEnabled), true, "stored bucket key")

	out = put(t, c, "enc", "kms", []byte("kms"))
	requireEqual(t, out.ServerSideEncryption, types.ServerSideEncryptionAwsKms, "put kms")
	requireEqual(t, aws.ToString(out.SSEKMSKeyId), keyID, "put kms key")
	requireEqual(t, aws.ToBool(out.BucketKeyEnabled), true, "put bucket key")
	head, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String("kms")})
	requireNoErr(t, err, "head kms")
	requireEqual(t, head.ServerSideEncryption, types.ServerSideEncryptionAwsKms, "head kms")
	requireEqual(t, aws.ToString(head.SSEKMSKeyId), keyID, "head kms key")

	requireRequestEncryption(t, c)
	requireEncryptionRefusals(t, c)
	requireEncryptionReset(t, c)
}

// requireEncryptionReset checks that deleting the bucket "enc"'s
// encryption reverts it to SSE-S3.
func requireEncryptionReset(t *testing.T, c *s3.Client) {
	t.Helper()
	ctx := t.Context()
	bucket := aws.String("enc")
	_, err := c.DeleteBucketEncryption(ctx, &s3.DeleteBucketEncryptionInput{Bucket: bucket})
	requireNoErr(t, err, "delete bucket encryption")
	enc, err := c.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: bucket})
	requireNoErr(t, err, "get bucket encryption after delete")
	requireEqual(t, enc.ServerSideEncryptionConfiguration.Rules[0].
		ApplyServerSideEncryptionByDefault.SSEAlgorithm, types.ServerSideEncryptionAes256,
		"default again")
	out := put(t, c, "enc", "after", []byte("after"))
	requireEqual(t, out.ServerSideEncryption, types.ServerSideEncryptionAes256, "put after delete")
}

// requireRequestEncryption checks, in the bucket "enc" with an SSE-KMS
// default of keyID, that a write's own header wins over the default, an
// earlier object keeps what it was stored with, a copy is encrypted as its
// request says, and a multipart upload as the default says.
func requireRequestEncryption(t *testing.T, c *s3.Client) {
	t.Helper()
	ctx := t.Context()
	bucket := aws.String("enc")
	// A write's own header wins over the default; the earlier object keeps
	// what it was stored with.
	sse, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("aes"), Body: strings.NewReader("aes"),
		ServerSideEncryption: types.ServerSideEncryptionAes256,
	})
	requireNoErr(t, err, "put with AES256")
	requireEqual(t, sse.ServerSideEncryption, types.ServerSideEncryptionAes256, "explicit AES256")
	requireEqual(t, aws.ToString(sse.SSEKMSKeyId), "", "no kms key for AES256")
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String("plain")})
	requireNoErr(t, err, "head plain again")
	requireEqual(
		t,
		head.ServerSideEncryption,
		types.ServerSideEncryptionAes256,
		"plain keeps AES256",
	)

	// A copy is encrypted as its request says, not as its source was.
	cp, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: aws.String("copy"), CopySource: aws.String("enc/kms"),
		ServerSideEncryption: types.ServerSideEncryptionAes256,
	})
	requireNoErr(t, err, "copy with AES256")
	requireEqual(t, cp.ServerSideEncryption, types.ServerSideEncryptionAes256, "copy sse")
	head, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String("copy")})
	requireNoErr(t, err, "head copy")
	requireEqual(t, head.ServerSideEncryption, types.ServerSideEncryptionAes256, "head copy sse")
	requireEqual(t, aws.ToString(head.SSEKMSKeyId), "", "copy keeps no kms key")

	mp, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: bucket, Key: aws.String("mp"),
	})
	requireNoErr(t, err, "create multipart upload")
	requireEqual(t, mp.ServerSideEncryption, types.ServerSideEncryptionAwsKms, "upload sse")
	part, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: bucket, Key: aws.String("mp"), UploadId: mp.UploadId, PartNumber: aws.Int32(1),
		Body: strings.NewReader("part"),
	})
	requireNoErr(t, err, "upload part")
	requireEqual(t, part.ServerSideEncryption, types.ServerSideEncryptionAwsKms, "part sse")
	requireEqual(t, aws.ToString(part.SSEKMSKeyId), keyID, "part kms key")
	done, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: aws.String("mp"), UploadId: mp.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: part.ETag},
		}},
	})
	requireNoErr(t, err, "complete multipart upload")
	requireEqual(t, done.ServerSideEncryption, types.ServerSideEncryptionAwsKms, "completed sse")
	requireEqual(t, aws.ToString(done.SSEKMSKeyId), keyID, "completed kms key")
}

// requireEncryptionRefusals checks the encryption headers and
// configurations the bucket "enc" refuses.
func requireEncryptionRefusals(t *testing.T, c *s3.Client) {
	t.Helper()
	ctx := t.Context()
	bucket := aws.String("enc")
	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("bad"), Body: strings.NewReader("bad"),
		ServerSideEncryption: types.ServerSideEncryption("aws:fsx"),
	})
	requireCode(t, err, "InvalidArgument")
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("bad"), Body: strings.NewReader("bad"),
		ServerSideEncryption: types.ServerSideEncryptionAes256, SSEKMSKeyId: aws.String(keyID),
	})
	requireCode(t, err, "InvalidArgument")
	_, err = c.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
		Bucket: bucket,
		ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
			Rules: []types.ServerSideEncryptionRule{{
				ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{
					SSEAlgorithm:   types.ServerSideEncryptionAes256,
					KMSMasterKeyID: aws.String(keyID),
				},
			}},
		},
	})
	requireCode(t, err, "InvalidArgument")
	_, err = c.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
		Bucket: bucket,
		ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
			Rules: []types.ServerSideEncryptionRule{{
				ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{
					SSEAlgorithm: types.ServerSideEncryption("DES"),
				},
			}},
		},
	})
	requireCode(t, err, "MalformedXML")
}

// A write with a customer-provided key (SSE-C) is reported with the
// customer algorithm and key MD5, not x-amz-server-side-encryption, on the
// write and on reads; nothing is encrypted, and reads do not need the key.
func TestCustomerProvidedKeys(t *testing.T) {
	c := startLive(t, nil).testClient()
	ctx := t.Context()
	createBucket(t, c, "ssec")
	key := aws.String(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	out, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("ssec"), Key: aws.String("k"), Body: strings.NewReader("k"),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: key,
	})
	requireNoErr(t, err, "put with SSE-C")
	requireEqual(t, aws.ToString(out.SSECustomerAlgorithm), "AES256", "put customer algorithm")
	requireEqual(t, aws.ToString(out.SSECustomerKeyMD5) != "", true, "put customer key MD5")
	requireEqual(t, out.ServerSideEncryption, types.ServerSideEncryption(""), "put sse")
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("ssec"), Key: aws.String("k"),
	})
	requireNoErr(t, err, "head without the key")
	requireEqual(t, aws.ToString(head.SSECustomerKeyMD5), aws.ToString(out.SSECustomerKeyMD5),
		"head customer key MD5")
	requireEqual(t, head.ServerSideEncryption, types.ServerSideEncryption(""), "head sse")
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("ssec"), Key: aws.String("bad"), Body: strings.NewReader("k"),
		SSECustomerAlgorithm: aws.String("DES"), SSECustomerKey: key,
	})
	requireCode(t, err, "InvalidEncryptionAlgorithmError")
}

// A GET or HEAD with x-amz-server-side-encryption of an SSE-S3 or SSE-KMS
// object is refused with 400, as the GetObject and HeadObject API
// references say; without it, the read is served.
func TestReadWithEncryptionHeader(t *testing.T) {
	c := startLive(t, nil).testClient()
	ctx := t.Context()
	createBucket(t, c, "enc")
	put(t, c, "enc", "k", []byte("k"))
	withSSE := editRequest(func(r *smithyhttp.Request) {
		r.Header.Set("X-Amz-Server-Side-Encryption", "AES256")
	})
	_, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("enc"),
		Key:    aws.String("k"),
	}, withSSE)
	requireCode(t, err, "InvalidArgument")
	requireResponse(t, err, http.StatusBadRequest)
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("enc"),
		Key:    aws.String("k"),
	}, withSSE)
	requireResponse(t, err, http.StatusBadRequest)
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("enc"), Key: aws.String("k")})
	requireNoErr(t, err, "head without the header")
}

// An SSE-C write needs a 256-bit key with its algorithm.
func TestCustomerKeyRules(t *testing.T) {
	c := startLive(t, nil).testClient()
	ctx := t.Context()
	createBucket(t, c, "ssec")
	short := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 16))
	for name, edit := range map[string]func(*smithyhttp.Request){
		"no key": func(r *smithyhttp.Request) {
			r.Header.Set("X-Amz-Server-Side-Encryption-Customer-Algorithm", "AES256")
		},
		"a 128-bit key": func(r *smithyhttp.Request) {
			r.Header.Set("X-Amz-Server-Side-Encryption-Customer-Algorithm", "AES256")
			r.Header.Set("X-Amz-Server-Side-Encryption-Customer-Key", short)
		},
		"no algorithm": func(r *smithyhttp.Request) {
			r.Header.Set("X-Amz-Server-Side-Encryption-Customer-Key", short)
		},
	} {
		_, err := c.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("ssec"), Key: aws.String("k"), Body: strings.NewReader("k"),
		}, editRequest(edit))
		requireCode(t, err, "InvalidArgument")
		requireResponse(t, err, http.StatusBadRequest)
		t.Logf("%s: refused", name)
	}
}
