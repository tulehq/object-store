//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The tests here run the live server against the real identity service:
// the lifecycle configuration operations are evaluated as
// s3:GetLifecycleConfiguration and s3:PutLifecycleConfiguration, which
// DeleteBucketLifecycle is evaluated as too (their API references), and
// the deletions S3 Lifecycle makes are evaluated as nothing at all.

const bktARN = "arn:aws:s3:::bkt"

// lifecycleEnv is identity, object-store accepting its keys on a
// fakeClock, the bucket bkt made with the root key, and dev, an IAM user
// whose only inline policy set replaces.
type lifecycleEnv struct {
	root, dev *s3.Client
	clk       *fakeClock
	set       func(doc string)
}

func startLifecycleEnv(t *testing.T) *lifecycleEnv {
	t.Helper()
	identity := startSibling(t, "identity", identityDirEnv)
	clk := newFakeClock(lifecycleEpoch)
	l := startLiveWith(t, identityEnv(identity.URL), liveOptions{clock: clk})
	ic := iamClient(identity.URL)
	root := l.client(rootCreds())
	createBucket(t, root, "bkt")
	key := createUserKey(t, ic, "dev")
	return &lifecycleEnv{
		root: root, dev: l.client(key), clk: clk,
		set: func(doc string) { t.Helper(); putUserPolicy(t, ic, "dev", "p0", doc) },
	}
}

var lifecycleRules = []types.LifecycleRule{
	enabled("logs", prefixFilter("logs/"), days(3)),
}

func putLifecycleAs(t *testing.T, c *s3.Client) error {
	t.Helper()
	_, err := c.PutBucketLifecycleConfiguration(t.Context(),
		&s3.PutBucketLifecycleConfigurationInput{
			Bucket:                 aws.String("bkt"),
			LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: lifecycleRules},
		})
	return err //nolint:wrapcheck // the test inspects the SDK error
}

func getLifecycleAs(t *testing.T, c *s3.Client) error {
	t.Helper()
	_, err := c.GetBucketLifecycleConfiguration(t.Context(),
		&s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("bkt")})
	return err //nolint:wrapcheck // the test inspects the SDK error
}

func deleteLifecycleAs(t *testing.T, c *s3.Client) error {
	t.Helper()
	_, err := c.DeleteBucketLifecycle(t.Context(),
		&s3.DeleteBucketLifecycleInput{Bucket: aws.String("bkt")})
	return err //nolint:wrapcheck // the test inspects the SDK error
}

// s3:GetLifecycleConfiguration allows the Get alone, and
// s3:PutLifecycleConfiguration the Put and the Delete; an explicit Deny of
// either refuses what it allowed.
func TestLifecyclePermissions(t *testing.T) {
	e := startLifecycleEnv(t)
	requireNoErr(t, putLifecycleAs(t, e.root), "root put")

	e.set(allowDoc(`"s3:GetLifecycleConfiguration"`, bktARN))
	requireNoErr(t, getLifecycleAs(t, e.dev), "get with s3:GetLifecycleConfiguration")
	requireDenied(t, putLifecycleAs(t, e.dev),
		notAllowed(devARN, "s3:PutLifecycleConfiguration", bktARN))
	requireDenied(t, deleteLifecycleAs(t, e.dev),
		notAllowed(devARN, "s3:PutLifecycleConfiguration", bktARN))

	e.set(allowDoc(`"s3:PutLifecycleConfiguration"`, bktARN))
	requireNoErr(t, putLifecycleAs(t, e.dev), "put with s3:PutLifecycleConfiguration")
	requireDenied(t, getLifecycleAs(t, e.dev),
		notAllowed(devARN, "s3:GetLifecycleConfiguration", bktARN))
	requireNoErr(t, deleteLifecycleAs(t, e.dev), "delete with s3:PutLifecycleConfiguration")
	requireCode(t, getLifecycleAs(t, e.root), "NoSuchLifecycleConfiguration")

	e.set(`{"Version":"2012-10-17","Statement":[` +
		`{"Effect":"Allow","Action":"s3:*","Resource":"*"},` +
		`{"Effect":"Deny","Action":["s3:GetLifecycleConfiguration",` +
		`"s3:PutLifecycleConfiguration"],"Resource":"` + bktARN + `"}]}`)
	explicit := "with an explicit deny in an identity-based policy"
	requireDenied(t, putLifecycleAs(t, e.dev),
		denied(devARN, "s3:PutLifecycleConfiguration", bktARN, explicit))
	requireDenied(t, getLifecycleAs(t, e.dev),
		denied(devARN, "s3:GetLifecycleConfiguration", bktARN, explicit))
	requireDenied(t, deleteLifecycleAs(t, e.dev),
		denied(devARN, "s3:PutLifecycleConfiguration", bktARN, explicit))
}

// "You can't use a bucket policy to prevent deletions or transitions by
// an S3 Lifecycle rule." (S3 User Guide, "Expiring objects"): a bucket
// policy denying every principal s3:DeleteObject and s3:DeleteObjectVersion
// refuses a client's delete and not the expiration.
func TestLifecycleIgnoresBucketPolicies(t *testing.T) {
	e := startLifecycleEnv(t)
	ctx := t.Context()
	put(t, e.root, "bkt", "logs/a", []byte("a"))
	put(t, e.root, "bkt", "keep/b", []byte("b"))
	_, err := e.root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String("bkt"),
		Policy: aws.String(`{"Version":"2012-10-17","Statement":[` +
			statement("Deny", `"*"`, "s3:DeleteObject", bktARN+"/*", "") + "," +
			statement("Deny", `"*"`, "s3:DeleteObjectVersion", bktARN+"/*", "") + "]}"),
	})
	requireNoErr(t, err, "put the bucket policy")
	e.set(allowDoc(`"s3:*"`, "*"))
	_, err = e.dev.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("logs/a"),
	})
	requireDenied(t, err, denied(devARN, "s3:DeleteObject", bktARN+"/logs/a",
		"with an explicit deny in a resource-based policy"))

	requireNoErr(t, putLifecycleAs(t, e.root), "put the configuration")
	advance(t, e.clk, day(18, 23, 59))
	requireKeys(t, e.root, "bkt", "keep/b", "logs/a")
	advance(t, e.clk, day(19))
	eventuallyKeys(t, e.root, "bkt", "keep/b")
}

// A user of another account with an identity Allow is refused the
// lifecycle operations, with the resource-based message, until the bucket
// policy grants its account s3:GetLifecycleConfiguration, which allows the
// Get alone, or s3:PutLifecycleConfiguration, which allows the Put and the
// Delete.
func TestLifecycleCrossAccount(t *testing.T) {
	e := startCrossAccountEnv(t)
	requireNoErr(t, putLifecycleAs(t, e.root), "root put")
	member := `{"AWS":"` + strings.Split(e.partnerARN, ":")[4] + `"}`
	noGrant := func(action string) string {
		return denied(e.partnerARN, action, bktARN,
			"because no resource-based policy allows the "+action+" action")
	}

	requireDenied(t, getLifecycleAs(t, e.partner), noGrant("s3:GetLifecycleConfiguration"))
	requireDenied(t, putLifecycleAs(t, e.partner), noGrant("s3:PutLifecycleConfiguration"))
	requireDenied(t, deleteLifecycleAs(t, e.partner), noGrant("s3:PutLifecycleConfiguration"))

	e.putBucketPolicy(t, statement("Allow", member, "s3:GetLifecycleConfiguration", bktARN, ""))
	requireNoErr(t, getLifecycleAs(t, e.partner), "partner get, granted")
	requireDenied(t, putLifecycleAs(t, e.partner), noGrant("s3:PutLifecycleConfiguration"))
	requireDenied(t, deleteLifecycleAs(t, e.partner), noGrant("s3:PutLifecycleConfiguration"))

	e.putBucketPolicy(t, statement("Allow", member, "s3:PutLifecycleConfiguration", bktARN, ""))
	requireNoErr(t, putLifecycleAs(t, e.partner), "partner put, granted")
	requireDenied(t, getLifecycleAs(t, e.partner), noGrant("s3:GetLifecycleConfiguration"))
	requireNoErr(t, deleteLifecycleAs(t, e.partner), "partner delete, granted")
	requireCode(t, getLifecycleAs(t, e.root), "NoSuchLifecycleConfiguration")
}
