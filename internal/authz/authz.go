// Package authz authorizes S3 requests against the identity-based policies
// of the principal that signed them and the bucket policy of the bucket
// they act on, as AWS evaluates the two together (tule.dev/policy's
// Authorize): the root principal of the server's account is always
// allowed; any other principal of that account needs an Allow in either
// kind of policy and no Deny in either, and a principal of another account
// needs an Allow in both.
//
// Every bucket belongs to the server's account.
package authz

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/gasmod/gas/storage"
	"tule.dev/policy"
	"tule.dev/sigv4"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// partition is the ARN partition of every S3 resource here.
const partition = "aws"

// defaultCacheSize bounds the parsed policy documents an Authorizer keeps.
const defaultCacheSize = 1024

// errNoPrincipal is returned for a request Verify did not pass, which the
// middleware never lets reach a handler.
var errNoPrincipal = errors.New("authz: the request has no verified principal")

// PolicySource reads the bucket policies requests are evaluated with, and
// pins the buckets an operation acts on to the generation evaluated:
// contracts.StorageProvider's GetBucketPolicy and Pin.
type PolicySource interface {
	GetBucketPolicy(ctx context.Context, bucket string) (string, []byte, error)
	Pin(ctx context.Context, refs ...contracts.BucketRef) (context.Context, func(), error)
}

// Authorizer evaluates requests against their principal's policies and
// their bucket's. It keeps the documents it parsed, by kind and content
// hash, so a policy is parsed once however many requests it is evaluated
// for. It is safe for concurrent use.
type Authorizer struct {
	now      func() time.Time
	policies PolicySource
	cache    map[cacheKey]*policy.Policy
	account  string
	limit    int
	cacheM   sync.Mutex
}

// cacheKey names a parsed document: its hash, and whether it was parsed as
// a resource-based policy rather than an identity-based one.
type cacheKey struct {
	sum      [sha256.Size]byte
	resource bool
}

// New returns an Authorizer for a server whose buckets belong to account,
// reading their policies from policies.
func New(account string, policies PolicySource) *Authorizer {
	return &Authorizer{
		now:      time.Now,
		policies: policies,
		cache:    make(map[cacheKey]*policy.Policy),
		account:  account,
		limit:    defaultCacheSize,
	}
}

// Caller is the principal of one verified request, ready to be evaluated
// for the operations the request makes. It is used by one goroutine.
type Caller struct {
	a    *Authorizer
	req  *http.Request
	keys map[string][]string
	// buckets are the bucket policies read for this request, by bucket,
	// nil for a bucket that has none, so each is read once per request.
	buckets map[string]*policy.Policy
	// generations are the generations of the buckets read, by bucket, ""
	// for a bucket that did not exist: the operation acts on these only.
	generations map[string]string
	principal   policy.Caller
	policies    []*policy.Policy
	root        bool
}

// Caller returns the caller of r, a request Verify passed. Its policies are
// parsed here; one that does not parse, which identity validated when it was
// stored, is an internal error.
func (a *Authorizer) Caller(r *http.Request) (*Caller, error) {
	p, ok := sigv4.PrincipalFrom(r.Context())
	if !ok {
		return nil, errNoPrincipal
	}
	// Evaluation is skipped for Root, while policy.Authorize grants full
	// identity access for the Account type, so the two must agree.
	if p.Root != (p.Type == sigv4.PrincipalTypeAccount) {
		return nil, fmt.Errorf("authz: principal %s has Root %t but type %q", p.ARN, p.Root, p.Type)
	}
	// A root principal is root only of its own account: the root of any
	// other is evaluated like any of that account's principals.
	if p.Root && p.AccountID == a.account {
		return &Caller{
			a: a, req: r, principal: policy.Caller{AccountID: p.AccountID, ARN: p.ARN}, root: true,
			buckets: map[string]*policy.Policy{}, generations: map[string]string{},
		}, nil
	}
	policies := make([]*policy.Policy, 0, len(p.Policies))
	for i, doc := range p.Policies {
		pol, err := a.parse(doc, false)
		if err != nil {
			return nil, fmt.Errorf("authz: parse policy %d of %s: %w", i, p.ARN, err)
		}
		policies = append(policies, pol)
	}
	return &Caller{
		a:   a,
		req: r,
		// policy.Authorize supplies aws:PrincipalArn, aws:userid,
		// aws:username, aws:PrincipalType and aws:PrincipalAccount from
		// these, and refuses a caller whose fields disagree, such as a
		// session whose role was deleted (an empty PrincipalARN).
		principal: policy.Caller{
			AccountID:    p.AccountID,
			ARN:          p.ARN,
			PrincipalARN: p.PrincipalARN,
			UserID:       p.UserID,
			Type:         policy.PrincipalType(p.Type),
		},
		policies:    policies,
		keys:        globalKeys(r, p, a.now()),
		buckets:     map[string]*policy.Policy{},
		generations: map[string]string{},
	}, nil
}

// InAccount reports whether the caller is a principal of the server's
// account, which owns every bucket.
func (c *Caller) InAccount() bool {
	return c.principal.AccountID == c.a.account
}

// Authorize is Caller(r) and its Authorize.
func (a *Authorizer) Authorize(
	r *http.Request,
	op Operation,
	targets ...s3api.Target,
) (*Caller, error) {
	c, err := a.Caller(r)
	if err != nil {
		return nil, err
	}
	if err = c.Authorize(op, targets...); err != nil {
		return nil, err
	}
	return c, nil
}

// parse returns doc parsed as an identity-based policy, or a resource-based
// one when resource is set, from the cache when it was parsed before. A
// full cache drops an arbitrary entry to make room.
func (a *Authorizer) parse(doc []byte, resource bool) (*policy.Policy, error) {
	key := cacheKey{sum: sha256.Sum256(doc), resource: resource}
	a.cacheM.Lock()
	p, ok := a.cache[key]
	a.cacheM.Unlock()
	if ok {
		return p, nil
	}

	parse := policy.ParseIdentity
	if resource {
		parse = policy.ParseResource
	}
	p, err := parse(doc)
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped by the caller with the document's owner
	}
	a.cacheM.Lock()
	defer a.cacheM.Unlock()
	if len(a.cache) >= a.limit {
		for k := range a.cache {
			delete(a.cache, k)
			break
		}
	}
	a.cache[key] = p
	return p, nil
}

// Authorize evaluates op's checks in order, the n-th on targets[n]: the
// root principal is allowed everything; any other principal gets S3's
// AccessDenied for the first check its policies and the target bucket's
// policy do not allow together. An operation with no entry, or with a
// different number of targets than checks, is an internal error, so a
// handler's mistake fails closed.
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
	if err := c.CheckExpectedOwner(op); err != nil {
		return err
	}
	if c.root {
		return nil
	}
	extra := operationKeys(op, targets)
	for i, t := range targets {
		if err := c.evaluate(checks[i], t, extra); err != nil {
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
// is returned as is. It is evaluated with the bucket policy the operation
// was, without reading it again.
func (c *Caller) NotFound(bucket string, err error) error {
	if !errors.Is(err, storage.ErrObjectNotFound) {
		return err
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

// evaluate evaluates ch on t with the caller's policies and, for a bucket
// or object, the bucket's policy. The bucket's owner is the server's
// account; an action on "*", ListBuckets', has no resource policy and is
// evaluated in the caller's own account, as AWS lists the caller's own
// buckets. The deny's message ends with the reason tule.dev/policy gives.
// An Allow of an owner-only check for a principal of another account is
// S3's 405 MethodNotAllowed.
func (c *Caller) evaluate(ch check, t s3api.Target, extra map[string]map[string]string) error {
	resource := resourceARN(ch.kind, t)
	keys, err := c.context(ch, extra[ch.action])
	if err != nil {
		return err
	}
	owner := c.a.account
	var bucketPolicy *policy.Policy
	if ch.kind == anyResource {
		owner = c.principal.AccountID
	} else if bucketPolicy, err = c.bucketPolicy(t.Bucket); err != nil {
		return err
	}
	keys[resourceAccountKey] = []string{owner}
	az := policy.Authorize(policy.AuthorizeRequest{
		Action:          ch.action,
		Resource:        resource,
		ResourceAccount: owner,
		Caller:          c.principal,
		Context:         keys,
	}, bucketPolicy, c.policies...)
	if az.Decision != policy.Allow {
		return accessDenied(c.principal.ARN, ch.action, resource, az.Reason.Message(ch.action))
	}
	if ch.ownerOnly && !c.InAccount() {
		return errMethodNotAllowed()
	}
	return nil
}

// resourceAccountKey is s3:ResourceAccount, the account that owns the
// resource, which the reference lists for every action evaluated here.
const resourceAccountKey = "s3:ResourceAccount"

// bucketPolicy returns bucket's policy, parsed, or nil when the bucket has
// none or does not exist. It is read once per request, with the bucket's
// existence (contracts.StorageProvider's GetBucketPolicy), so the policy
// evaluated is the one stored for the bucket the operation acts on. A
// stored policy that no longer parses, or that cannot be read, fails the
// request with an internal error: it is never evaluated as absent.
//
// The bucket's generation is recorded with it, and Pin pins the bucket to
// it, so the operation acts on the bucket whose policy was evaluated, not
// one deleted and created again under its name since. For the root
// principal, which is not evaluated, only the generation is recorded.
func (c *Caller) bucketPolicy(bucket string) (*policy.Policy, error) {
	if p, ok := c.buckets[bucket]; ok {
		return p, nil
	}
	gen, doc, err := c.a.policies.GetBucketPolicy(c.req.Context(), bucket)
	switch {
	case errors.Is(err, storage.ErrBucketNotFound), errors.Is(err, contracts.ErrNoSuchBucketPolicy):
		c.buckets[bucket], c.generations[bucket] = nil, gen
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("authz: read the policy of bucket %s: %w", bucket, err)
	}
	if c.root {
		c.buckets[bucket], c.generations[bucket] = nil, gen
		return nil, nil
	}
	p, err := c.a.parse(doc, true)
	if err != nil {
		return nil, fmt.Errorf(
			"authz: the stored policy of bucket %s does not parse: %w",
			bucket,
			err,
		)
	}
	c.buckets[bucket], c.generations[bucket] = p, gen
	return p, nil
}

// Ref returns bucket as of the generation this request read it in, reading
// it now when it has not.
func (c *Caller) Ref(bucket string) (contracts.BucketRef, error) {
	if _, err := c.bucketPolicy(bucket); err != nil {
		return contracts.BucketRef{}, err
	}
	return contracts.BucketRef{Name: bucket, Generation: c.generations[bucket]}, nil
}

// Pin pins buckets to the generations this request evaluated them in (see
// contracts.StorageProvider's Pin), reading any it has not yet. The
// returned context is the one to act on them with, and release must be
// called when the operation is done. A bucket deleted, or deleted and
// created again, since it was evaluated fails with
// storage.ErrBucketNotFound, which S3 answers as NoSuchBucket.
func (c *Caller) Pin(
	ctx context.Context,
	buckets ...string,
) (pinned context.Context, release func(), err error) {
	refs := make([]contracts.BucketRef, 0, len(buckets))
	for _, b := range buckets {
		ref, rErr := c.Ref(b)
		if rErr != nil {
			return nil, nil, rErr
		}
		refs = append(refs, ref)
	}
	pinned, release, err = c.a.policies.Pin(ctx, refs...)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // storage.ErrBucketNotFound, answered as NoSuchBucket
	}
	return pinned, release, nil
}

// The headers that name the account a request expects to own its bucket,
// and, for CopyObject, its source bucket (the S3 API Reference, each
// operation's ExpectedBucketOwner and ExpectedSourceBucketOwner).
const (
	expectedOwnerHeader       = "X-Amz-Expected-Bucket-Owner"
	expectedSourceOwnerHeader = "X-Amz-Source-Expected-Bucket-Owner"
)

// CheckExpectedOwner refuses a request that expects its bucket, or
// CopyObject's source bucket, to belong to an account other than the
// server's, which owns every bucket: "If the account ID that you provide
// does not match the actual owner of the bucket, the request fails with the
// HTTP status code 403 Forbidden (access denied)." It applies to every
// caller, root included, and to every value of a repeated header.
// ListBuckets and CreateBucket take no such header.
func (c *Caller) CheckExpectedOwner(op Operation) error {
	if op == ListBuckets || op == CreateBucket {
		return nil
	}
	headers := []string{expectedOwnerHeader}
	if op == CopyObject {
		headers = append(headers, expectedSourceOwnerHeader)
	}
	for _, h := range headers {
		for _, v := range c.req.Header.Values(h) {
			if v != c.a.account {
				return s3api.NewError(http.StatusForbidden, "AccessDenied", "Access Denied")
			}
		}
	}
	return nil
}

// errMethodNotAllowed is S3's 405 MethodNotAllowed, which the bucket policy
// operations answer an allowed principal of another account than the
// bucket owner's with ("If you have the correct permissions, but you're not
// using an identity that belongs to the bucket owner's account, Amazon S3
// returns a 405 Method Not Allowed error."; PutBucketPolicy,
// GetBucketPolicy and DeleteBucketPolicy API references, Permissions).
func errMethodNotAllowed() *s3api.Error {
	return s3api.NewError(http.StatusMethodNotAllowed, "MethodNotAllowed",
		"The specified method is not allowed against this resource.")
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
// request a policy denies (the S3 User Guide, "Troubleshoot access denied
// (403 Forbidden) errors in Amazon S3"): "User: <arn> is not authorized to
// perform: <action> on resource: "<resource>" <reason>", where reason is
// policy.Reason's Message. An action evaluated on "*", which has no
// resource type, names no resource. S3 adds ", with policy ARN: <arn>" to
// an identity-based explicit deny; the documents identity hands over carry
// no ARN, so it is left out.
func accessDenied(principal, action, resource, reason string) *s3api.Error {
	on := ""
	if resource != "*" {
		on = ` on resource: "` + resource + `"`
	}
	return s3api.NewError(http.StatusForbidden, "AccessDenied",
		"User: "+principal+" is not authorized to perform: "+action+on+" "+reason)
}

// Grant is an authorized operation whose buckets are pinned to the
// generations it was evaluated in. Act on them with Context, and call
// Release when the operation is done, its response body included.
type Grant struct {
	caller  *Caller
	ctx     context.Context
	release func()
	once    sync.Once
}

// Context is the context pinned to the operation's buckets, which the
// storage operations on them require.
func (g *Grant) Context() context.Context { return g.ctx }

// Release releases the operation's buckets. Calls after the first do
// nothing.
func (g *Grant) Release() { g.once.Do(g.release) }

// NotFound is the caller's NotFound.
func (g *Grant) NotFound(bucket string, err error) error { return g.caller.NotFound(bucket, err) }

// AuthorizeAndPin authorizes op on targets for r's caller, as Authorize
// does, and pins the targets' buckets to the generations evaluated (see
// Caller.Pin), so the operation acts on the buckets whose policies allowed
// it. ctx is the context to derive the pinned one from.
func (a *Authorizer) AuthorizeAndPin(
	ctx context.Context,
	r *http.Request,
	op Operation,
	targets ...s3api.Target,
) (*Grant, error) {
	//nolint:contextcheck // evaluation reads bucket policies with the request's context
	c, err := a.Authorize(r, op, targets...)
	if err != nil {
		return nil, err
	}
	var buckets []string
	for _, t := range targets {
		if t.Bucket != "" && !slices.Contains(buckets, t.Bucket) {
			buckets = append(buckets, t.Bucket)
		}
	}
	pinned, release, err := c.Pin(ctx, buckets...)
	if err != nil {
		return nil, err
	}
	return &Grant{caller: c, ctx: pinned, release: release}, nil
}
