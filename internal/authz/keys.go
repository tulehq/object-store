package authz

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/gasmod/gas/storage"
	"tule.dev/sigv4"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

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

// listActions are the listing actions s3:prefix, s3:delimiter and
// s3:max-keys are listed for.
var listActions = []string{"s3:ListBucket", "s3:ListBucketVersions"}

// serviceKeys are the S3 condition keys supplied, each for the actions the
// Service Authorization Reference lists it for. A key the request carries no
// value for is left out of the context, as S3 leaves it out.
var serviceKeys = []serviceKey{
	{name: "s3:prefix", actions: listActions, value: query("prefix")},
	{name: "s3:delimiter", actions: listActions, value: query("delimiter")},
	{name: "s3:max-keys", actions: listActions, value: query("max-keys")},
	{
		name: "s3:x-amz-acl",
		actions: []string{
			"s3:CreateBucket", "s3:PutObject", "s3:PutBucketAcl", "s3:PutObjectAcl",
			"s3:PutObjectVersionAcl",
		},
		value: header("X-Amz-Acl"),
	},
	grantKey("s3:x-amz-grant-full-control", "X-Amz-Grant-Full-Control"),
	grantKey("s3:x-amz-grant-read", "X-Amz-Grant-Read"),
	grantKey("s3:x-amz-grant-read-acp", "X-Amz-Grant-Read-Acp"),
	grantKey("s3:x-amz-grant-write", "X-Amz-Grant-Write"),
	grantKey("s3:x-amz-grant-write-acp", "X-Amz-Grant-Write-Acp"),
	{
		name:    "s3:x-amz-object-ownership",
		actions: []string{"s3:CreateBucket"},
		value:   header("X-Amz-Object-Ownership"),
	},
	{
		name:    "s3:x-amz-server-side-encryption-aws-kms-key-id",
		actions: []string{"s3:PutObject"},
		value:   header("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"),
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

// grantKey is the condition key name for the grant header h, which the
// reference lists for the actions that set ACLs.
func grantKey(name, h string) serviceKey {
	return serviceKey{
		name: name,
		actions: []string{
			"s3:CreateBucket", "s3:PutObject", "s3:PutBucketAcl", "s3:PutObjectAcl",
			"s3:PutObjectVersionAcl",
		},
		value: header(h),
	}
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
// reports them: an IAM user's, or a session's role's. aws:PrincipalOrgID
// and aws:PrincipalOrgPaths are the organization identity reports the
// principal's account in, and are left out for one in no organization, as
// AWS leaves them out; when identity could not tell, they are unknown
// instead (see unknownKeys).
func globalKeys(r *http.Request, p sigv4.Principal, now time.Time) map[string][]string {
	keys := map[string][]string{
		"aws:CurrentTime":     {now.UTC().Format(time.RFC3339)},
		"aws:EpochTime":       {strconv.FormatInt(now.Unix(), 10)},
		"aws:SecureTransport": {strconv.FormatBool(r.TLS != nil)},
		"aws:RequestedRegion": {contracts.Region},
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		keys["aws:SourceIp"] = []string{host}
	}
	for k, v := range p.Tags {
		keys[principalTagPrefix+k] = []string{v}
	}
	if p.OrgID != "" {
		keys[orgIDKey] = []string{p.OrgID}
		keys[orgPathsKey] = slices.Clone(p.OrgPaths)
	}
	return keys
}

// The organization condition keys.
const (
	orgIDKey    = "aws:PrincipalOrgID"
	orgPathsKey = "aws:PrincipalOrgPaths"
)

// unknownKeys are the condition keys whose values identity could not tell
// for p: both organization keys when it could not ask the accounts service
// (p.OrgUnknown). They are passed to policy.Authorize as unknown, never left
// out: an absent key satisfies a negated operator, so StringNotEquals on
// aws:PrincipalOrgID would grant, or let a Deny lapse, for a principal whose
// organization is not known. An unknown key never helps an Allow apply and
// always lets a Deny apply.
func unknownKeys(p sigv4.Principal) []string {
	if p.OrgUnknown {
		return []string{orgIDKey, orgPathsKey}
	}
	return nil
}

// versionIDKey is s3:versionid, the version a request names, as given
// ("null" for the null version). It comes from the target rather than the
// request's query, since a copy names its source's version in its
// x-amz-copy-source header, so it is no serviceKey.
const versionIDKey = "s3:versionid"

// versionIDActions are the actions evaluated here that the reference lists
// s3:versionid for.
var versionIDActions = []string{
	"s3:GetObjectVersion", "s3:DeleteObjectVersion", "s3:GetObjectVersionTagging",
	"s3:PutObjectVersionTagging", "s3:DeleteObjectVersionTagging", "s3:GetObjectVersionAcl",
	"s3:PutObjectVersionAcl", "s3:GetObjectVersionAttributes",
}

// actionKeys are the operation's own keys for ch's action on t, extra's,
// and s3:versionid when t names a version and the reference lists the key
// for the action. extra is not modified.
func actionKeys(ch check, t s3api.Target, extra map[string]string) map[string]string {
	if t.VersionID == "" || !slices.Contains(versionIDActions, ch.action) {
		return extra
	}
	keys := make(map[string]string, len(extra)+1)
	maps.Copy(keys, extra)
	keys[versionIDKey] = t.VersionID
	return keys
}

// principalTagPrefix is the prefix of the aws:PrincipalTag/<key> keys.
const principalTagPrefix = "aws:PrincipalTag/"

// The object tag condition keys ("Tagging and access control policies", S3
// User Guide): s3:ExistingObjectTag/<key>, each tag of the object acted
// on, and s3:RequestObjectTag/<key> and s3:RequestObjectTagKeys, each tag
// and the keys of the tag set the request sets.
const (
	existingTagPrefix = "s3:ExistingObjectTag/"
	requestTagPrefix  = "s3:RequestObjectTag/"
	requestTagKeysKey = "s3:RequestObjectTagKeys"
)

// existingTagActions are the actions evaluated here that the reference
// lists s3:ExistingObjectTag/<key> for, and requestTagActions those it
// lists s3:RequestObjectTag/<key> and s3:RequestObjectTagKeys for.
var (
	existingTagActions = []string{
		"s3:GetObject", "s3:GetObjectAcl", "s3:PutObjectAcl", "s3:GetObjectAttributes",
		"s3:GetObjectTagging", "s3:PutObjectTagging", "s3:DeleteObjectTagging",
		"s3:GetObjectVersion", "s3:GetObjectVersionAcl", "s3:PutObjectVersionAcl",
		"s3:GetObjectVersionAttributes", "s3:GetObjectVersionTagging",
		"s3:PutObjectVersionTagging", "s3:DeleteObjectVersionTagging",
	}
	requestTagActions = []string{
		"s3:PutObject", "s3:PutObjectTagging", "s3:PutObjectVersionTagging",
	}
)

// SetRequestTags gives the tags of the request's document, which
// PutObjectTagging sets, as its s3:RequestObjectTag and
// s3:RequestObjectTagKeys. It must be called before Authorize, so the
// document is read and parsed before the request is evaluated.
func (c *Caller) SetRequestTags(tags []contracts.Tag) {
	c.bodyTags, c.bodyTagsSet = tags, true
}

// requestTagsOf returns the tags op's request on targets sets: the
// document's, given by SetRequestTags, or the x-amz-tagging header's for
// PutObject, CreateMultipartUpload and a CopyObject under the REPLACE
// tagging directive (s3api.RequestTags), whose error, a header that does
// not parse, refuses the request. A CopyObject under the COPY tagging
// directive, the default, sets its source's tags, targets[1]'s as stored
// (see existing), so a policy's conditions on the tags a write sets apply
// to the tags a copy copies; AWS does not document which keys copied tags
// are evaluated as, and this is a choice, the one that lets no tag reach
// an object unevaluated. The copy is bound to the source they were read
// from (see CheckObject). Any other operation sets none.
func (c *Caller) requestTagsOf(op Operation, targets []s3api.Target) ([]contracts.Tag, error) {
	if c.bodyTagsSet {
		return c.bodyTags, nil
	}
	switch op {
	case PutObject, CreateMultipartUpload:
	case CopyObject:
		if !replacesTags(c.req) {
			return c.existing(targets[1])
		}
	default:
		return nil, nil
	}
	tags, err := s3api.RequestTags(c.req)
	if err != nil {
		return nil, err //nolint:wrapcheck // an S3 error, answered as is
	}
	return tags, nil
}

// addTagKeys adds to keys the object tag keys of ch on t: the request's
// tags for the actions requestTagActions lists, and t's tags as stored for
// those existingTagActions lists, except for an implied check and one a
// write's headers add (onWrite). An object that does not exist has no
// tags. Values are bounded as every other value is (maxContextValue).
func (c *Caller) addTagKeys(keys map[string][]string, ch check, t s3api.Target) error {
	if ch.implied || ch.kind != objectResource {
		return nil
	}
	if slices.Contains(requestTagActions, ch.action) {
		if err := c.addRequestTagKeys(keys); err != nil {
			return err
		}
	}
	if ch.onWrite || !slices.Contains(existingTagActions, ch.action) {
		return nil
	}
	tags, err := c.existing(t)
	if err != nil {
		return err
	}
	for _, tag := range tags {
		if err = setTagKey(keys, existingTagPrefix+tag.Key, tag.Value); err != nil {
			return err
		}
	}
	return nil
}

// addRequestTagKeys adds the request's tags to keys, and their keys as
// s3:RequestObjectTagKeys, when it sets any.
func (c *Caller) addRequestTagKeys(keys map[string][]string) error {
	if len(c.requestTags) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.requestTags))
	for _, tag := range c.requestTags {
		if err := setTagKey(keys, requestTagPrefix+tag.Key, tag.Value); err != nil {
			return err
		}
		names = append(names, tag.Key)
	}
	keys[requestTagKeysKey] = names
	return nil
}

// setTagKey sets the tag key name to v, refusing a value longer than
// maxContextValue, as Caller.context does.
func setTagKey(keys map[string][]string, name, v string) error {
	if len(name) > maxContextValue || len(v) > maxContextValue {
		return s3api.ErrInvalidArgument(fmt.Sprintf(
			"The value of the condition key %s is longer than %d bytes.", name, maxContextValue))
	}
	keys[name] = []string{v}
	return nil
}

// evaluatedObject is the object a request's s3:ExistingObjectTag keys were
// read from: whether there was one, and its ETag, modification time, tags
// and version ID, the ID of the version read, as one read of it returned
// them (contracts.StorageProvider's StatObjectTagging).
type evaluatedObject struct {
	lastModified time.Time
	etag         string
	versionID    string
	tags         []contracts.Tag
	found        bool
}

// existing returns the tags stored for the object t, the version it names
// or else the current one, read once per request, and records which
// object version they are the tags of, which the operation must then act
// on (see CheckObject): none when there is no such object, version or
// bucket, when the version read is a delete marker, or when the key or
// version ID is not one an object can have; the handler answers the
// error for those. A read that fails otherwise fails the request; the
// tags are never treated as absent.
func (c *Caller) existing(t s3api.Target) ([]contracts.Tag, error) {
	if o, ok := c.evaluated[t]; ok {
		return o.tags, nil
	}
	var o evaluatedObject
	var marker *contracts.DeleteMarkerError
	info, err := c.a.policies.StatObjectTagging(c.req.Context(),
		contracts.ObjectRef{Bucket: t.Bucket, Key: t.Key, Version: t.VersionID})
	switch {
	case errors.Is(err, storage.ErrObjectNotFound), errors.Is(err, storage.ErrBucketNotFound),
		errors.Is(err, storage.ErrInvalidKey), errors.Is(err, storage.ErrVersionNotFound),
		errors.Is(err, contracts.ErrInvalidVersionID), errors.As(err, &marker):
	case err != nil:
		return nil, fmt.Errorf("authz: read the tags of %s/%s: %w", t.Bucket, t.Key, err)
	default:
		o = evaluatedObject{
			lastModified: info.LastModified, etag: info.ETag, versionID: info.VersionID,
			tags: info.Tags, found: true,
		}
	}
	if c.evaluated == nil {
		c.evaluated = map[s3api.Target]evaluatedObject{}
	}
	c.evaluated[t] = o
	return o.tags, nil
}

// CheckObject checks that info, the object t as the operation opened it,
// with its Tags and VersionID, is the object version whose tags the
// request was evaluated with: the same version, ETag, modification time
// and tags, so a decision made on the tags of one object is never applied
// to another written at its key meanwhile, even one with the same ETag
// and modification time that became the current version, or to the same
// version retagged meanwhile. The tags are compared too, as a set
// (sameTagSet), since a retagging changes none of the other three. The
// version IDs are compared as sameVersion does. When
// it is not, the operation fails with contracts.ErrOperationAborted,
// S3's 409 OperationAborted, which asks the client to try again; it is
// not evaluated again against the object opened, a choice, as simpler and
// as safe. A target no s3:ExistingObjectTag key was read for, which
// includes every target of the root principal, which is not evaluated, is
// not checked.
func (c *Caller) CheckObject(t s3api.Target, info *contracts.ObjectInfo) error {
	o, ok := c.evaluated[t]
	if !ok {
		return nil
	}
	if o.found && sameVersion(o.versionID, info.VersionID) && o.etag == info.ETag &&
		o.lastModified.Equal(info.LastModified) && sameTagSet(o.tags, info.Tags) {
		return nil
	}
	return fmt.Errorf("authz: %s/%s changed after the request was evaluated: %w",
		t.Bucket, t.Key, contracts.ErrOperationAborted)
}

// sameVersion reports whether a and b, the version IDs of two reads, name
// the same version: "", which a read of an Unversioned bucket's object
// reports, is its null version, so a bucket whose versioning was enabled
// between the two reads still binds its object.
func sameVersion(a, b string) bool {
	canon := func(id string) string {
		if id == "" {
			return contracts.VersionNull
		}
		return id
	}
	return canon(a) == canon(b)
}

// sameTagSet reports whether a and b are the same tag set, in any order:
// the same keys, each with the same value. A tag set's keys are unique
// (s3api's tag rules), so a retagging that only reorders the set is no
// change.
func sameTagSet(a, b []contracts.Tag) bool {
	return len(a) == len(b) && maps.Equal(tagValues(a), tagValues(b))
}

// tagValues maps each key of tags to its value.
func tagValues(tags []contracts.Tag) map[string]string {
	values := make(map[string]string, len(tags))
	for _, tag := range tags {
		values[tag.Key] = tag.Value
	}
	return values
}

// ObjectCheck returns CheckObject for t, for a storage operation that
// opens t itself.
func (c *Caller) ObjectCheck(t s3api.Target) contracts.ObjectCheck {
	return func(info *contracts.ObjectInfo) error { return c.CheckObject(t, info) }
}
