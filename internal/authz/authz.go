// Package authz authorizes S3 requests against the identity-based policies
// of the principal that signed them, as AWS does within one account: the
// root principal is always allowed; any other is allowed an action on a
// resource only when one of its policies allows it and none denies it.
//
// A principal of an account other than the server's is refused even then.
// AWS grants a cross-account request only when a resource-based policy
// allows it as well ("Cross-account policy evaluation logic", IAM User
// Guide), and no resource-based policy is evaluated here, so none can.
package authz

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gasmod/gas/storage"
	"tule.dev/policy"
	"tule.dev/sigv4"

	"tule.dev/object-store/internal/s3api"
)

// partition is the ARN partition of every S3 resource here.
const partition = "aws"

// defaultCacheSize bounds the parsed policy documents an Authorizer keeps.
const defaultCacheSize = 1024

// errNoPrincipal is returned for a request Verify did not pass, which the
// middleware never lets reach a handler.
var errNoPrincipal = errors.New("authz: the request has no verified principal")

// Authorizer evaluates requests against their principal's policies. It
// keeps the documents it parsed, by content hash, so a principal's policies
// are parsed once however many requests it signs. It is safe for concurrent
// use.
type Authorizer struct {
	now     func() time.Time
	cache   map[[sha256.Size]byte]*policy.Policy
	account string
	limit   int
	cacheM  sync.Mutex
}

// New returns an Authorizer for a server whose buckets belong to account.
func New(account string) *Authorizer {
	return &Authorizer{
		now:     time.Now,
		cache:   make(map[[sha256.Size]byte]*policy.Policy),
		account: account,
		limit:   defaultCacheSize,
	}
}

// Caller is the principal of one verified request, ready to be evaluated
// for the operations the request makes.
type Caller struct {
	req      *http.Request
	keys     map[string][]string
	arn      string
	policies []*policy.Policy
	// foreign marks a principal of an account other than the server's.
	foreign bool
	root    bool
}

// Caller returns the caller of r, a request Verify passed. Its policies are
// parsed here; one that does not parse, which identity validated when it was
// stored, is an internal error.
func (a *Authorizer) Caller(r *http.Request) (*Caller, error) {
	p, ok := sigv4.PrincipalFrom(r.Context())
	if !ok {
		return nil, errNoPrincipal
	}
	// A root principal is root only of its own account: the root of any
	// other is refused like any of that account's principals.
	if p.Root && p.AccountID == a.account {
		return &Caller{req: r, arn: p.ARN, root: true}, nil
	}
	policies := make([]*policy.Policy, 0, len(p.Policies))
	for i, doc := range p.Policies {
		pol, err := a.parse(doc)
		if err != nil {
			return nil, fmt.Errorf("authz: parse policy %d of %s: %w", i, p.ARN, err)
		}
		policies = append(policies, pol)
	}
	return &Caller{
		req:      r,
		arn:      p.ARN,
		policies: policies,
		keys:     globalKeys(r, p, a.now()),
		foreign:  p.AccountID != a.account,
	}, nil
}

// Authorize is Caller(r) and its Authorize.
func (a *Authorizer) Authorize(r *http.Request, op Operation, targets ...s3api.Target) error {
	c, err := a.Caller(r)
	if err != nil {
		return err
	}
	return c.Authorize(op, targets...)
}

// parse returns the parsed identity policy doc, from the cache when it was
// parsed before. A full cache drops an arbitrary entry to make room.
func (a *Authorizer) parse(doc []byte) (*policy.Policy, error) {
	sum := sha256.Sum256(doc)
	a.cacheM.Lock()
	p, ok := a.cache[sum]
	a.cacheM.Unlock()
	if ok {
		return p, nil
	}

	p, err := policy.ParseIdentity(doc)
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped by Caller with the principal
	}
	a.cacheM.Lock()
	defer a.cacheM.Unlock()
	if len(a.cache) >= a.limit {
		for k := range a.cache {
			delete(a.cache, k)
			break
		}
	}
	a.cache[sum] = p
	return p, nil
}

// Authorize evaluates op's checks in order, the n-th on targets[n]: the
// root principal is allowed everything; any other principal gets S3's
// AccessDenied for the first check its policies do not allow, or, in
// another account, for the first check at all. An operation with no entry,
// or with a different number of targets than checks, is an internal error,
// so a handler's mistake fails closed.
//
// The targets are the bucket and key the handler acts on, decoded, and
// CopyObject's s3:x-amz-copy-source is its source target's, so a policy is
// never matched against the client's own spelling of either. A bucket name
// or key longer than S3 allows is refused first, whoever the caller, since
// evaluation costs grow with the resource's length.
func (c *Caller) Authorize(op Operation, targets ...s3api.Target) error {
	checks, ok := operations[op]
	if !ok {
		return fmt.Errorf("authz: %s has no authorization entry", op)
	}
	if len(targets) != len(checks) {
		return fmt.Errorf("authz: %s takes %d targets, got %d", op, len(checks), len(targets))
	}
	for _, t := range targets {
		if err := checkLengths(t); err != nil {
			return err
		}
	}
	if c.root {
		return nil
	}
	extra := operationKeys(op, targets)
	for i, ch := range checks {
		if err := c.evaluate(ch, targets[i], extra); err != nil {
			return err
		}
	}
	return nil
}

// S3's limits on the names a resource ARN is built from: a bucket name is
// 3 to 63 characters ("General purpose bucket naming rules", S3 User
// Guide), a key at most 1024 bytes of UTF-8 ("Naming Amazon S3 objects").
const (
	maxBucketName = 63
	maxKey        = 1024
)

// checkLengths refuses a target whose bucket name or key is longer than S3
// allows, with S3's errors for them (the S3 API Reference, Error):
// InvalidBucketName and KeyTooLongError ("Your key is too long.").
func checkLengths(t s3api.Target) error {
	if len(t.Bucket) > maxBucketName {
		return s3api.NewError(http.StatusBadRequest, "InvalidBucketName",
			"The specified bucket is not valid.")
	}
	if len(t.Key) > maxKey {
		return s3api.NewError(http.StatusBadRequest, "KeyTooLongError", "Your key is too long.")
	}
	return nil
}

// The condition keys that come from what an operation acts on rather than
// from the request as sent.
const (
	// copySourceKey is s3:x-amz-copy-source, which the reference lists for
	// s3:PutObject. Its value is CopyObject's source as the handler reads
	// it, <bucket>/<key> decoded, not the header as sent, which may
	// percent-encode it or lead with a slash.
	copySourceKey = "s3:x-amz-copy-source"
	// creationKey is s3:ObjectCreationOperation, which the reference lists
	// for s3:PutObject: true for the operations that create an object
	// (PutObject, CopyObject, CompleteMultipartUpload), false for the
	// multipart steps that do not (CreateMultipartUpload, UploadPart). The
	// S3 User Guide's "Enforce conditional writes" exempts those steps
	// with it, as they take no conditional headers, and notes that its
	// policies refuse CopyObject.
	creationKey = "s3:ObjectCreationOperation"
)

// objectCreation is s3:ObjectCreationOperation by operation.
var objectCreation = map[Operation]string{
	PutObject:               "true",
	CopyObject:              "true",
	CompleteMultipartUpload: "true",
	CreateMultipartUpload:   "false",
	UploadPart:              "false",
}

// operationKeys are the condition keys that come from what op acts on
// rather than from the request as sent, by the action they are supplied
// for.
func operationKeys(op Operation, targets []s3api.Target) map[string]map[string]string {
	put := map[string]string{}
	if v, ok := objectCreation[op]; ok {
		put[creationKey] = v
	}
	if op == CopyObject {
		src := targets[1]
		put[copySourceKey] = src.Bucket + "/" + src.Key
	}
	if len(put) == 0 {
		return nil
	}
	return map[string]map[string]string{"s3:PutObject": put}
}

// NotFound is the error to answer a GetObject, HeadObject or CopyObject
// source read with when it found no object in bucket (err is
// storage.ErrObjectNotFound): S3 answers 404 NoSuchKey only to a caller
// with s3:ListBucket on the bucket, and 403 AccessDenied to any other (the
// GetObject and HeadObject API references, Permissions), so whether a key
// exists is not revealed to a caller who could not list it. Any other err
// is returned as is.
func (a *Authorizer) NotFound(r *http.Request, bucket string, err error) error {
	if !errors.Is(err, storage.ErrObjectNotFound) {
		return err
	}
	c, cErr := a.Caller(r)
	if cErr != nil {
		return cErr
	}
	if c.root {
		return err
	}
	if dErr := c.evaluate(check{action: "s3:ListBucket", kind: bucketResource, implied: true},
		s3api.Target{Bucket: bucket}, nil); dErr != nil {
		return dErr
	}
	return err
}

func (c *Caller) evaluate(ch check, t s3api.Target, extra map[string]map[string]string) error {
	resource := resourceARN(ch.kind, t)
	keys, err := c.context(ch, extra[ch.action])
	if err != nil {
		return err
	}
	res := policy.Evaluate(policy.Request{
		Action:    ch.action,
		Resource:  resource,
		Principal: c.arn,
		Context:   keys,
	}, c.policies...)
	switch res.Decision {
	case policy.Allow:
		if c.foreign {
			return accessDenied(c.arn, ch.action, resource, "because no resource-based "+
				"policy allows the "+ch.action+" action")
		}
		return nil
	case policy.ExplicitDeny:
		return accessDenied(c.arn, ch.action, resource, "with an explicit deny in an "+
			"identity-based policy")
	default:
		return accessDenied(c.arn, ch.action, resource, "because no identity-based policy "+
			"allows the "+ch.action+" action")
	}
}

// context is the request context for ch: the global keys, the service keys
// the reference lists for its action that the request has a value for
// (only those of every action for an implied check), and extra, the
// operation's own keys for the action. A value longer than maxContextValue
// refuses the request.
func (c *Caller) context(ch check, extra map[string]string) (map[string][]string, error) {
	keys := make(map[string][]string, len(c.keys)+len(serviceKeys)+len(extra))
	for k, v := range c.keys {
		keys[k] = v
	}
	set := func(name, v string) error {
		if len(v) > maxContextValue {
			return s3api.ErrInvalidArgument(fmt.Sprintf(
				"The value of the condition key %s is longer than %d bytes.", name,
				maxContextValue))
		}
		keys[name] = []string{v}
		return nil
	}
	for name, v := range c.serviceValues(ch) {
		if err := set(name, v); err != nil {
			return nil, err
		}
	}
	for name, v := range extra {
		if err := set(name, v); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// serviceValues are the values the request carries for the service keys
// the reference lists for ch's action, or, for an implied check, for the
// keys of every action only.
func (c *Caller) serviceValues(ch check) map[string]string {
	values := map[string]string{}
	for _, k := range serviceKeys {
		if !k.appliesTo(ch.action) || ch.implied && k.actions != nil {
			continue
		}
		if v, ok := k.value(c.req); ok {
			values[k.name] = v
		}
	}
	return values
}

// resourceARN is the ARN of t as a resource of kind, in the reference's
// formats.
func resourceARN(kind resourceKind, t s3api.Target) string {
	switch kind {
	case bucketResource:
		return "arn:" + partition + ":s3:::" + t.Bucket
	case objectResource:
		return "arn:" + partition + ":s3:::" + t.Bucket + "/" + t.Key
	default:
		return "*"
	}
}

// accessDenied is S3's AccessDenied, 403, with the message S3 returns for a
// same-account request an identity-based policy denies (the S3 User Guide,
// "Troubleshoot access denied (403 Forbidden) errors in Amazon S3"): "User:
// <arn> is not authorized to perform: <action> on resource: "<resource>"
// <reason>". An action evaluated on "*", which has no resource type, names
// no resource. S3 adds ", with policy ARN: <arn>" to an explicit deny; the
// documents identity hands over carry no ARN, so it is left out.
func accessDenied(principal, action, resource, reason string) *s3api.Error {
	on := ""
	if resource != "*" {
		on = ` on resource: "` + resource + `"`
	}
	return s3api.NewError(http.StatusForbidden, "AccessDenied",
		"User: "+principal+" is not authorized to perform: "+action+on+" "+reason)
}
