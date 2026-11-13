package authz

import (
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"

	"tule.dev/sigv4"
)

// region is the region every bucket is in.
const region = "us-east-1"

// maxContextValue bounds the length of a condition key's value taken from
// the request. tule.dev/policy's cost grows with the values' length times
// the policies' patterns, so callers must cap what they pass. S3's longest
// such value, an s3:prefix or s3:x-amz-copy-source of up to a 63-byte
// bucket and a 1024-byte key, stays well under it.
const maxContextValue = 4096

// serviceKey is an S3 condition key the server can supply cheaply: the
// actions the reference lists it for (nil for every S3 action this server
// evaluates), and its value in the request, when the request has one.
type serviceKey struct {
	value   func(r *http.Request) (string, bool)
	name    string
	actions []string
}

// serviceKeys are the S3 condition keys supplied, each for the actions the
// Service Authorization Reference lists it for. A key the request carries no
// value for is left out of the context, as S3 leaves it out.
var serviceKeys = []serviceKey{
	{name: "s3:prefix", actions: []string{"s3:ListBucket"}, value: query("prefix")},
	{name: "s3:delimiter", actions: []string{"s3:ListBucket"}, value: query("delimiter")},
	{name: "s3:max-keys", actions: []string{"s3:ListBucket"}, value: query("max-keys")},
	{
		name:    "s3:x-amz-acl",
		actions: []string{"s3:CreateBucket", "s3:PutObject"},
		value:   header("X-Amz-Acl"),
	},
	{
		name:    "s3:x-amz-server-side-encryption",
		actions: []string{"s3:PutObject"},
		value:   header("X-Amz-Server-Side-Encryption"),
	},
	{
		name:    "s3:x-amz-storage-class",
		actions: []string{"s3:PutObject"},
		value:   header("X-Amz-Storage-Class"),
	},
	{
		name:    "s3:x-amz-metadata-directive",
		actions: []string{"s3:PutObject"},
		value:   header("X-Amz-Metadata-Directive"),
	},
	{name: "s3:if-match", actions: []string{"s3:PutObject"}, value: header("If-Match")},
	{name: "s3:if-none-match", actions: []string{"s3:PutObject"}, value: header("If-None-Match")},
	{name: "s3:x-amz-content-sha256", value: header("X-Amz-Content-Sha256")},
	{name: "s3:authType", value: authType},
	// Verify accepts only SigV4.
	{name: "s3:signatureversion", value: func(*http.Request) (string, bool) {
		return "AWS4-HMAC-SHA256", true
	}},
}

func query(name string) func(*http.Request) (string, bool) {
	return func(r *http.Request) (string, bool) {
		q := r.URL.Query()
		if !q.Has(name) {
			return "", false
		}
		return q.Get(name), true
	}
}

func header(name string) func(*http.Request) (string, bool) {
	return func(r *http.Request) (string, bool) {
		vs := r.Header.Values(name)
		if len(vs) == 0 {
			return "", false
		}
		return vs[0], true
	}
}

// authType is s3:authType: REST-QUERY-STRING for a presigned URL,
// REST-HEADER for an Authorization header. It tells them apart with the
// test Verify makes (a non-empty X-Amz-Algorithm query parameter, with
// presigned URLs accepted), since sigv4 does not report which it verified.
func authType(r *http.Request) (string, bool) {
	if r.URL.Query().Get("X-Amz-Algorithm") != "" {
		return "REST-QUERY-STRING", true
	}
	return "REST-HEADER", true
}

// appliesTo reports whether k is supplied for action.
func (k serviceKey) appliesTo(action string) bool {
	return k.actions == nil || slices.Contains(k.actions, action)
}

// globalKeys are the global condition keys (IAM User Guide, "AWS global
// condition context keys") the server supplies for p's request, besides the
// principal's own (aws:PrincipalArn, aws:userid, aws:username,
// aws:PrincipalType and aws:PrincipalAccount), which policy.Authorize
// supplies from the Caller. They also fill policy variables such as
// ${aws:username}.
//
// aws:RequestedRegion is the server's region, us-east-1, which every bucket
// reports, not the region the request was signed for: the server does not
// check that, so a caller could pick it. aws:SourceIp is the TCP peer's
// address, never X-Forwarded-For, which any client can set.
// aws:PrincipalTag/<key> is each of the principal's tags, as identity
// reports them: an IAM user's, or a session's role's.
func globalKeys(r *http.Request, p sigv4.Principal, now time.Time) map[string][]string {
	keys := map[string][]string{
		"aws:CurrentTime":     {now.UTC().Format(time.RFC3339)},
		"aws:EpochTime":       {strconv.FormatInt(now.Unix(), 10)},
		"aws:SecureTransport": {strconv.FormatBool(r.TLS != nil)},
		"aws:RequestedRegion": {region},
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		keys["aws:SourceIp"] = []string{host}
	}
	for k, v := range p.Tags {
		keys[principalTagPrefix+k] = []string{v}
	}
	return keys
}

// principalTagPrefix is the prefix of the aws:PrincipalTag/<key> keys.
const principalTagPrefix = "aws:PrincipalTag/"
