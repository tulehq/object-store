//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The tests here run the live server against the real identity service
// and check that object tagging is evaluated as S3 evaluates it: the
// s3:PutObjectTagging a tagged write also needs, the tag count only a
// caller allowed s3:GetObjectTagging is answered, and the object tag
// condition keys.

// tagEnv is the live server with identity and the bucket "bkt": the root
// client, the IAM user dev's client, and set, which sets dev's policy.
type tagEnv struct {
	root *s3.Client
	dev  *s3.Client
	set  func(doc string)
}

func startTagEnv(t *testing.T) *tagEnv {
	t.Helper()
	identity := startSibling(t, "identity", identityDirEnv)
	l := startLiveWithIdentity(t, identity.URL)
	ic := iamClient(identity.URL)
	root := l.client(rootCreds())
	createBucket(t, root, "bkt")
	key := createUserKey(t, ic, "dev")
	return &tagEnv{
		root: root,
		dev:  l.client(key),
		set:  func(doc string) { t.Helper(); putUserPolicy(t, ic, "dev", "p0", doc) },
	}
}

// conditionDoc is a policy allowing actions on every object of bkt under
// cond, a JSON object, and also allowing always.
func conditionDoc(actions, cond, always string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":` + actions +
		`,"Resource":"arn:aws:s3:::bkt/*","Condition":` + cond + `},` +
		`{"Effect":"Allow","Action":` + always + `,"Resource":"*"}]}`
}

// A write carrying x-amz-tagging also needs s3:PutObjectTagging, as a
// copy under REPLACE does; a copy under COPY of a tagged source needs it
// and s3:GetObjectTagging (see TestACopyOfTagsNeedsTagPermissions).
func TestTaggedWritesNeedPutObjectTagging(t *testing.T) {
	e := startTagEnv(t)
	ctx := t.Context()
	e.set(allowDoc(`["s3:PutObject","s3:GetObject"]`, "*"))
	put(t, e.root, "bkt", "src", []byte("src"))
	_, err := e.root.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String("bkt"), Key: aws.String("src"), Tagging: tags("team", "a"),
	})
	requireNoErr(t, err, "tag src")

	_, err = e.dev.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("k"),
		Tagging: aws.String("team=a"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutObjectTagging", "arn:aws:s3:::bkt/k"))
	_, err = e.dev.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("bkt"), Key: aws.String("m"), Tagging: aws.String("team=a"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutObjectTagging", "arn:aws:s3:::bkt/m"))
	_, err = e.dev.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("c"), CopySource: copySource("bkt", "src"),
		TaggingDirective: types.TaggingDirectiveReplace, Tagging: aws.String("team=b"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutObjectTagging", "arn:aws:s3:::bkt/c"))
	_, err = e.dev.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("c"), CopySource: copySource("bkt", "src"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:GetObjectTagging", "arn:aws:s3:::bkt/src"))

	e.set(allowDoc(`["s3:PutObject","s3:PutObjectTagging"]`, "*"))
	_, err = e.dev.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("k"),
		Tagging: aws.String("team=a"),
	})
	requireNoErr(t, err, "a tagged put with s3:PutObjectTagging")
}

// x-amz-tagging-count is answered only to a caller allowed
// s3:GetObjectTagging, and the tagging operations are evaluated as their
// actions.
func TestTagCountNeedsGetObjectTagging(t *testing.T) {
	e := startTagEnv(t)
	ctx := t.Context()
	_, err := e.root.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("k"),
		Tagging: aws.String("a=1&b=2"),
	})
	requireNoErr(t, err, "put a tagged object")
	head := func() *int32 {
		t.Helper()
		out, hErr := e.dev.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String("bkt"), Key: aws.String("k"),
		})
		requireNoErr(t, hErr, "head")
		getOut, gErr := e.dev.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String("bkt"), Key: aws.String("k"),
		})
		requireNoErr(t, gErr, "get")
		requireNoErr(t, getOut.Body.Close(), "close")
		requireEqual(t, aws.ToInt32(getOut.TagCount), aws.ToInt32(out.TagCount), "GET and HEAD")
		return out.TagCount
	}

	e.set(allowDoc(`["s3:GetObject"]`, "*"))
	requireEqual(t, head() == nil, true, "count without s3:GetObjectTagging")
	_, err = e.dev.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:GetObjectTagging", "arn:aws:s3:::bkt/k"))
	_, err = e.dev.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), Tagging: tags("a", "1"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutObjectTagging", "arn:aws:s3:::bkt/k"))
	_, err = e.dev.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:DeleteObjectTagging", "arn:aws:s3:::bkt/k"))

	e.set(allowDoc(`["s3:GetObject","s3:GetObjectTagging"]`, "*"))
	requireEqual(t, aws.ToInt32(head()), 2, "count with s3:GetObjectTagging")
}

// s3:ExistingObjectTag/<key> is the object's stored tag, for a read, its
// ACL and its tagging, and a copy's source.
func TestExistingObjectTagCondition(t *testing.T) {
	e := startTagEnv(t)
	ctx := t.Context()
	for key, tagging := range map[string]string{"prod": "env=prod", "dev": "env=dev", "none": ""} {
		_, err := e.root.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("bkt"), Key: aws.String(key), Body: strings.NewReader(key),
			Tagging: aws.String(tagging),
		})
		requireNoErr(t, err, "put "+key)
	}
	e.set(conditionDoc(`["s3:GetObject","s3:GetObjectTagging","s3:GetObjectAcl"]`,
		`{"StringEquals":{"s3:ExistingObjectTag/env":"prod"}}`,
		`["s3:PutObject","s3:PutObjectTagging"]`))

	_, err := e.dev.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: aws.String("bkt"), Key: aws.String("prod"),
	})
	requireNoErr(t, err, "the tags of a prod object")
	_, err = e.dev.GetObjectAcl(ctx, &s3.GetObjectAclInput{
		Bucket: aws.String("bkt"), Key: aws.String("prod"),
	})
	requireNoErr(t, err, "the ACL of a prod object")
	get(t, e.dev, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("prod")})
	_, err = e.dev.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("copy"), CopySource: copySource("bkt", "prod"),
	})
	requireNoErr(t, err, "copy a prod object")
	for _, key := range []string{"dev", "none"} {
		requireDenied(t, getObject(t, e.dev, key),
			notAllowed(devARN, "s3:GetObject", "arn:aws:s3:::bkt/"+key))
		_, err = e.dev.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket: aws.String("bkt"), Key: aws.String("copy"), CopySource: copySource("bkt", key),
		})
		requireDenied(t, err, notAllowed(devARN, "s3:GetObject", "arn:aws:s3:::bkt/"+key))
	}

	// A write is never evaluated with the tags of the object it replaces.
	e.set(conditionDoc(`"s3:PutObject"`, `{"StringEquals":{"s3:ExistingObjectTag/env":"prod"}}`,
		`"s3:GetObject"`))
	_, err = e.dev.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("prod"), Body: strings.NewReader("x"),
	})
	requireDenied(t, err, notAllowed(devARN, "s3:PutObject", "arn:aws:s3:::bkt/prod"))
}

// s3:RequestObjectTag/<key> and s3:RequestObjectTagKeys are the tags a
// request sets, in PutObjectTagging's document or a write's x-amz-tagging.
func TestRequestObjectTagConditions(t *testing.T) {
	e := startTagEnv(t)
	ctx := t.Context()
	put(t, e.root, "bkt", "k", []byte("k"))
	e.set(conditionDoc(`["s3:PutObjectTagging","s3:PutObject"]`,
		`{"StringEquals":{"s3:RequestObjectTag/team":"a"},`+
			`"ForAllValues:StringEquals":{"s3:RequestObjectTagKeys":["team","env"]}}`,
		`"s3:GetObject"`))

	for _, tt := range []struct {
		tagging *types.Tagging
		header  string
		ok      bool
	}{
		{tagging: tags("team", "a", "env", "dev"), header: "team=a&env=dev", ok: true},
		{tagging: tags("team", "b"), header: "team=b"},
		{tagging: tags("team", "a", "owner", "me"), header: "team=a&owner=me"},
		{tagging: tags("env", "dev"), header: "env=dev"},
	} {
		_, err := e.dev.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
			Bucket: aws.String("bkt"), Key: aws.String("k"), Tagging: tt.tagging,
		})
		_, pErr := e.dev.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("bkt"), Key: aws.String("w"), Body: strings.NewReader("w"),
			Tagging: aws.String(tt.header),
		})
		if tt.ok {
			requireNoErr(t, err, "PutObjectTagging "+tt.header)
			requireNoErr(t, pErr, "PutObject "+tt.header)
			continue
		}
		requireDenied(t, err, notAllowed(devARN, "s3:PutObjectTagging", "arn:aws:s3:::bkt/k"))
		requireDenied(t, pErr, notAllowed(devARN, "s3:PutObject", "arn:aws:s3:::bkt/w"))
	}
}

// A bucket policy denying s3:PutObjectAcl on an existing tag does not
// deny a write with an ACL over the tagged object: "you cannot create a
// policy to grant or deny a user permissions to delete or overwrite an
// object based on its existing tags" ("Tagging and access control
// policies", S3 User Guide). The write is allowed over it as to a new key;
// PutObjectAcl of it is denied.
func TestAWritesACLIgnoresExistingTags(t *testing.T) {
	e := startTagEnv(t)
	ctx := t.Context()
	_, err := e.root.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("acls"), ObjectOwnership: types.ObjectOwnershipObjectWriter,
	})
	requireNoErr(t, err, "create an ObjectWriter bucket")
	_, err = e.root.DeletePublicAccessBlock(ctx,
		&s3.DeletePublicAccessBlockInput{Bucket: aws.String("acls")})
	requireNoErr(t, err, "allow public ACLs")
	_, err = e.root.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("acls"), Key: aws.String("locked"), Body: strings.NewReader("x"),
		Tagging: aws.String("locked=true"),
	})
	requireNoErr(t, err, "put a locked object")
	_, err = e.root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String("acls"),
		Policy: aws.String(`{"Version":"2012-10-17","Statement":[` + statement("Deny", `"*"`,
			"s3:PutObjectAcl", "arn:aws:s3:::acls/*",
			`{"StringEquals":{"s3:ExistingObjectTag/locked":"true"}}`) + `]}`),
	})
	requireNoErr(t, err, "put the deny policy")
	e.set(allowDoc(`["s3:PutObject","s3:PutObjectAcl"]`, "*"))

	for _, key := range []string{"locked", "new"} {
		_, err = e.dev.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("acls"), Key: aws.String(key), Body: strings.NewReader("y"),
			ACL: types.ObjectCannedACLPublicRead,
		})
		requireNoErr(t, err, "a public-read write to "+key)
	}
	_, err = e.root.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("acls"), Key: aws.String("locked"), Body: strings.NewReader("x"),
		Tagging: aws.String("locked=true"),
	})
	requireNoErr(t, err, "lock the object again")
	_, err = e.dev.PutObjectAcl(ctx, &s3.PutObjectAclInput{
		Bucket: aws.String("acls"), Key: aws.String("locked"), ACL: types.ObjectCannedACLPrivate,
	})
	requireDenied(t, err, denied(devARN, "s3:PutObjectAcl", "arn:aws:s3:::acls/locked",
		"with an explicit deny in a resource-based policy"))
}

// A copy under COPY, the default, of a source with tags needs
// s3:GetObjectTagging on the source and s3:PutObjectTagging on the
// destination, and the source's tags are the destination's
// s3:RequestObjectTag keys, so a Deny on them applies to the copy. A
// copy of a source without tags needs neither.
func TestACopyOfTagsNeedsTagPermissions(t *testing.T) {
	e := startTagEnv(t)
	ctx := t.Context()
	_, err := e.root.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("src"), Body: strings.NewReader("src"),
		Tagging: aws.String("team=a"),
	})
	requireNoErr(t, err, "put a tagged source")
	put(t, e.root, "bkt", "plain", []byte("plain"))
	copyOf := func(src, dst string) error {
		t.Helper()
		_, cErr := e.dev.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket: aws.String("bkt"), Key: aws.String(dst), CopySource: copySource("bkt", src),
		})
		return cErr //nolint:wrapcheck // the test inspects the SDK error
	}

	e.set(allowDoc(`["s3:GetObject","s3:PutObject","s3:PutObjectTagging"]`, "*"))
	requireDenied(t, copyOf("src", "c"),
		notAllowed(devARN, "s3:GetObjectTagging", "arn:aws:s3:::bkt/src"))
	e.set(allowDoc(`["s3:GetObject","s3:PutObject","s3:GetObjectTagging"]`, "*"))
	requireDenied(t, copyOf("src", "c"),
		notAllowed(devARN, "s3:PutObjectTagging", "arn:aws:s3:::bkt/c"))

	e.set(allowDoc(`["s3:GetObject","s3:PutObject"]`, "*"))
	requireNoErr(t, copyOf("plain", "p"), "copy an untagged source")
	out, err := e.root.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: aws.String("bkt"), Key: aws.String("p"),
	})
	requireNoErr(t, err, "the tags of the untagged copy")
	requireEqual(t, len(out.TagSet), 0, "the untagged copy's tags")

	all := `["s3:GetObject","s3:PutObject","s3:GetObjectTagging","s3:PutObjectTagging"]`
	e.set(allowDoc(all, "*"))
	requireNoErr(t, copyOf("src", "c"), "copy a tagged source with both permissions")
	out, err = e.root.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: aws.String("bkt"), Key: aws.String("c"),
	})
	requireNoErr(t, err, "the tags of the copy")
	requireEqual(t, len(out.TagSet) == 1 && aws.ToString(out.TagSet[0].Key) == "team" &&
		aws.ToString(out.TagSet[0].Value) == "a", true, "the copy has its source's tags")

	e.set(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":` + all +
		`,"Resource":"*"},{"Effect":"Deny","Action":"s3:PutObject",` +
		`"Resource":"arn:aws:s3:::bkt/*","Condition":` +
		`{"StringEquals":{"s3:RequestObjectTag/team":"a"}}}]}`)
	requireDenied(t, copyOf("src", "d"), denied(devARN, "s3:PutObject", "arn:aws:s3:::bkt/d",
		"with an explicit deny in an identity-based policy"))
	requireNoErr(t, copyOf("plain", "d"), "copy an untagged source past the Deny")
}
