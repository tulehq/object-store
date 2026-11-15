//go:build e2e

package e2e

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The tests here run the live server against the real identity service:
// the notification configuration operations are evaluated as
// s3:PutBucketNotification and s3:GetBucketNotification on the bucket, and
// a bucket policy may grant them to other principals
// (PutBucketNotificationConfiguration API reference). The configurations
// put name only EventBridge, which needs no destination service.

// eventBridgeOnly is a configuration with no destination to validate.
var eventBridgeOnly = &types.NotificationConfiguration{
	EventBridgeConfiguration: &types.EventBridgeConfiguration{},
}

func putNotificationAs(t *testing.T, c *s3.Client) error {
	t.Helper()
	return putNotification(t, c, "bkt", eventBridgeOnly, false)
}

func getNotificationAs(t *testing.T, c *s3.Client) error {
	t.Helper()
	_, err := getNotification(t, c, "bkt")
	return err
}

// s3:GetBucketNotification allows the Get alone, and
// s3:PutBucketNotification the Put; an explicit Deny of either refuses
// what it allowed.
func TestNotificationPermissions(t *testing.T) {
	e := startLifecycleEnv(t)
	requireNoErr(t, putNotificationAs(t, e.root), "root put")

	e.set(allowDoc(`"s3:GetBucketNotification"`, bktARN))
	requireNoErr(t, getNotificationAs(t, e.dev), "get with s3:GetBucketNotification")
	requireDenied(t, putNotificationAs(t, e.dev),
		notAllowed(devARN, "s3:PutBucketNotification", bktARN))

	e.set(allowDoc(`"s3:PutBucketNotification"`, bktARN))
	requireNoErr(t, putNotificationAs(t, e.dev), "put with s3:PutBucketNotification")
	requireDenied(t, getNotificationAs(t, e.dev),
		notAllowed(devARN, "s3:GetBucketNotification", bktARN))

	e.set(`{"Version":"2012-10-17","Statement":[` +
		`{"Effect":"Allow","Action":"s3:*","Resource":"*"},` +
		`{"Effect":"Deny","Action":["s3:GetBucketNotification",` +
		`"s3:PutBucketNotification"],"Resource":"` + bktARN + `"}]}`)
	explicit := "with an explicit deny in an identity-based policy"
	requireDenied(t, putNotificationAs(t, e.dev),
		denied(devARN, "s3:PutBucketNotification", bktARN, explicit))
	requireDenied(t, getNotificationAs(t, e.dev),
		denied(devARN, "s3:GetBucketNotification", bktARN, explicit))
}

// A user with no identity policy is allowed what the bucket policy grants
// it, and a Deny in the bucket policy refuses a user its identity policy
// allows.
func TestNotificationBucketPolicy(t *testing.T) {
	e := startPolicyEnv(t)
	const otherARN = "arn:aws:iam::000000000000:user/other"
	other := s3As(e.srv, createUserKey(t, e.ic, "other"))
	dev := e.user(t, allowDoc(`"s3:*"`, "*"))

	requireDenied(t, getNotificationAs(t, other),
		notAllowed(otherARN, "s3:GetBucketNotification", bktARN))
	e.putBucketPolicy(t,
		statement("Allow", `{"AWS":"`+otherARN+`"}`, "s3:GetBucketNotification", bktARN, ""),
		statement("Allow", `{"AWS":"`+otherARN+`"}`, "s3:PutBucketNotification", bktARN, ""),
		statement("Deny", `{"AWS":"`+devARN+`"}`, "s3:PutBucketNotification", bktARN, ""))
	requireNoErr(t, getNotificationAs(t, other), "get granted by the bucket policy")
	requireNoErr(t, putNotificationAs(t, other), "put granted by the bucket policy")

	requireNoErr(t, getNotificationAs(t, dev), "dev's get, allowed by its identity policy")
	requireDenied(t, putNotificationAs(t, dev), denied(devARN, "s3:PutBucketNotification",
		bktARN, "with an explicit deny in a resource-based policy"))
}
