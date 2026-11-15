package s3api

import "net/http"

// The headers that report what a bucket's lifecycle configuration will do
// to an object or an upload: x-amz-expiration on PutObject, CopyObject,
// CompleteMultipartUpload, GetObject and HeadObject, and x-amz-abort-date
// and x-amz-abort-rule-id on CreateMultipartUpload and ListParts (their
// API references).
const (
	HeaderExpiration  = "X-Amz-Expiration"
	HeaderAbortDate   = "X-Amz-Abort-Date"
	HeaderAbortRuleID = "X-Amz-Abort-Rule-Id"
)

// ErrNoSuchLifecycleConfiguration is GetBucketLifecycleConfiguration's
// answer for a bucket without a lifecycle configuration; the message is
// the description its API reference gives the code.
var ErrNoSuchLifecycleConfiguration = NewError(http.StatusNotFound,
	"NoSuchLifecycleConfiguration", "The lifecycle configuration does not exist")

// CheckTag applies the tag rules of a tag set (see ParseTagging) to one
// tag, as a lifecycle rule's filter names it, with their InvalidTag
// errors. An empty value is valid.
func CheckTag(tag Tag) error {
	return checkTag(tag)
}
