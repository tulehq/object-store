package authz

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"tule.dev/policy"

	"tule.dev/object-store/internal/s3api"
)

// MaxBucketPolicySize is the largest bucket policy PutBucketPolicy stores,
// in bytes: "Bucket policies are limited to 20 KB in size." (S3 User Guide,
// "Adding a bucket policy by using the Amazon S3 console"). It also bounds
// what evaluating a bucket's policy costs. Reading 20 KB as 20 * 1024
// bytes of the document as sent is this package's choice.
const MaxBucketPolicySize = 20 * 1024

// ErrMalformedPolicy is S3's 400 MalformedPolicy with message, the error
// PutBucketPolicy answers a policy it refuses with.
func ErrMalformedPolicy(message string) *s3api.Error {
	return s3api.NewError(http.StatusBadRequest, "MalformedPolicy", message)
}

// ErrPolicyTooLarge is the error for a bucket policy over
// MaxBucketPolicySize. S3 documents the limit but not its error; the code
// and message are this package's choice.
func ErrPolicyTooLarge() *s3api.Error {
	return ErrMalformedPolicy("Policy exceeds the maximum allowed document size.")
}

// ValidateBucketPolicy checks doc as a policy for bucket and returns S3's
// error for one PutBucketPolicy refuses:
//
//   - over MaxBucketPolicySize, ErrPolicyTooLarge;
//   - anything tule.dev/policy's ParseResource refuses, such as a statement
//     without a Principal or a principal IAM does not accept ("Invalid
//     principal in policy"), MalformedPolicy with ParseResource's message;
//   - a Resource or NotResource entry that is neither the bucket's ARN,
//     arn:aws:s3:::<bucket>, nor an object ARN in it,
//     arn:aws:s3:::<bucket>/<key pattern>, MalformedPolicy "Policy has
//     invalid resource", the message S3 is reported to give for a resource
//     naming another bucket (a secondary source:
//     github.com/aws-samples/aws-serverless-appsync-app issue 4). That
//     "*" and wildcards in the bucket name are refused too is this
//     package's choice;
//   - an Action or NotAction entry that is neither "*" nor an s3 action,
//     MalformedPolicy "Policy has invalid action" (the message is this
//     package's choice; S3 does not document it).
//
// A Sid may be any string unique in the policy, as ParseResource accepts
// it: the S3 documentation gives no rule of its own for a bucket policy's
// Sid (IAM's alphanumeric rule, "IAM JSON policy elements: Sid", is for IAM
// policies). Actions are not checked against S3's action list, nor against
// the resources they apply to.
func ValidateBucketPolicy(bucket string, doc []byte) error {
	if len(doc) > MaxBucketPolicySize {
		return ErrPolicyTooLarge()
	}
	if _, err := policy.ParseResource(doc); err != nil {
		var pe *policy.ParseError
		if errors.As(err, &pe) {
			return ErrMalformedPolicy(pe.Message)
		}
		return err //nolint:wrapcheck // ParseResource returns only *ParseError
	}
	statements, err := decodeStatements(doc)
	if err != nil {
		// ParseResource accepted the document, so it decodes; a failure
		// here is the two decoders disagreeing, which is refused.
		return ErrMalformedPolicy("Syntax errors in policy.")
	}
	bucketARN := "arn:" + partition + ":s3:::" + bucket
	for _, st := range statements {
		if err = checkStatement(bucketARN, st); err != nil {
			return err
		}
	}
	return nil
}

// checkStatement applies S3's own checks to one statement's resources and
// actions, for the bucket whose ARN is bucketARN.
func checkStatement(bucketARN string, st map[string][]string) error {
	for _, r := range append(st["Resource"], st["NotResource"]...) {
		if r != bucketARN && !strings.HasPrefix(r, bucketARN+"/") {
			return ErrMalformedPolicy("Policy has invalid resource")
		}
	}
	for _, a := range append(st["Action"], st["NotAction"]...) {
		if a != "*" && !strings.HasPrefix(strings.ToLower(a), "s3:") {
			return ErrMalformedPolicy("Policy has invalid action")
		}
	}
	return nil
}

// decodeStatements returns the Resource, NotResource, Action and NotAction
// entries of each statement of doc, a document ParseResource accepted:
// Statement is one object or an array of them, and each element a string
// or an array of strings. Element names are matched exactly, as
// ParseResource matches them, and it has refused repeated names.
func decodeStatements(doc []byte) ([]map[string][]string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		return nil, err //nolint:wrapcheck // the caller reports its own error
	}
	raw := bytes.TrimSpace(top["Statement"])
	var objs []map[string]json.RawMessage
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &objs); err != nil {
			return nil, err //nolint:wrapcheck // the caller reports its own error
		}
	} else {
		var one map[string]json.RawMessage
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, err //nolint:wrapcheck // the caller reports its own error
		}
		objs = []map[string]json.RawMessage{one}
	}
	out := make([]map[string][]string, 0, len(objs))
	for _, obj := range objs {
		st := map[string][]string{}
		for _, name := range []string{"Resource", "NotResource", "Action", "NotAction"} {
			v, ok := obj[name]
			if !ok {
				continue
			}
			values, err := stringOrList(v)
			if err != nil {
				return nil, err
			}
			st[name] = values
		}
		out = append(out, st)
	}
	return out, nil
}

// stringOrList decodes raw, a string or an array of strings.
func stringOrList(raw json.RawMessage) ([]string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err //nolint:wrapcheck // the caller reports its own error
		}
		return []string{s}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err //nolint:wrapcheck // the caller reports its own error
	}
	return list, nil
}
