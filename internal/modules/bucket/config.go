package bucket

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// maxConfigDocument bounds a bucket configuration document. A CORS
// configuration is "limited to 64 KB in size" (PutBucketCors API
// reference); the other configurations are much smaller, and applying the
// same bound to them is a choice.
const maxConfigDocument = s3api.MaxCORSDocument

// configKind is one bucket configuration subresource: the operations that
// put, get and delete it, the error a bucket without it is answered with,
// and how a request document is validated into the document stored.
// fallback, when set, is the document a bucket without it reports instead.
type configKind struct {
	parse    func(doc []byte) (any, error)
	fallback func() any
	notFound *s3api.Error
	kind     contracts.BucketConfig
	put      authz.Operation
	get      authz.Operation
	del      authz.Operation
}

// configKinds are the configuration subresources served by putConfig,
// getConfig and deleteConfig, by subresource.
var configKinds = map[string]configKind{
	"tagging": {
		kind: contracts.ConfigTagging, notFound: s3api.ErrNoSuchTagSet,
		put: authz.PutBucketTagging, get: authz.GetBucketTagging, del: authz.DeleteBucketTagging,
		parse: func(doc []byte) (any, error) { return s3api.ParseTagging(doc) },
	},
	"cors": {
		kind: contracts.ConfigCORS, notFound: s3api.ErrNoSuchCORSConfiguration,
		put: authz.PutBucketCors, get: authz.GetBucketCors, del: authz.DeleteBucketCors,
		parse: func(doc []byte) (any, error) { return s3api.ParseCORSConfiguration(doc) },
	},
	"publicAccessBlock": {
		kind:     contracts.ConfigPublicAccessBlock,
		notFound: s3api.ErrNoSuchPublicAccessBlockConfiguration,
		put:      authz.PutPublicAccessBlock, get: authz.GetPublicAccessBlock,
		del:   authz.DeletePublicAccessBlock,
		parse: func(doc []byte) (any, error) { return s3api.ParsePublicAccessBlock(doc) },
	},
	"encryption": {
		kind: contracts.ConfigEncryption,
		put:  authz.PutBucketEncryption, get: authz.GetBucketEncryption,
		del:      authz.DeleteBucketEncryption,
		parse:    func(doc []byte) (any, error) { return s3api.ParseEncryption(doc) },
		fallback: func() any { return s3api.NewDefaultEncryption() },
	},
}

// putConfig serves PutBucketTagging, PutBucketCors, PutPublicAccessBlock
// and PutBucketEncryption: the document is read, at most
// maxConfigDocument bytes, validated, and stored as the bucket's
// configuration of its kind, replacing any it had, answering 200 with no
// body, as each API reference says. The operation is evaluated before the
// body is read, and the bucket pinned after, as PutBucketPolicy does.
// Content-MD5 and the checksum headers are accepted and ignored.
func (m *Module) putConfig(ctx gas.Context, ck configKind) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}
	r := ctx.Request()

	caller, err := m.authz.Authorize(r, ck.put, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	doc, err := s3api.ReadDocument(r, maxConfigDocument, m.verifier.ReadBody)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	parsed, err := ck.parse(doc)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	stored, err := s3api.Encode(parsed)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	pinned, release, err := caller.Pin(ctx, bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer release()
	if err = m.storage.PutBucketConfig(pinned, bucket, ck.kind, stored); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

// getConfig serves GetBucketTagging, GetBucketCors, GetPublicAccessBlock
// and GetBucketEncryption: the stored document, or, for a bucket without
// one, ck's fallback or its notFound error.
func (m *Module) getConfig(ctx gas.Context, ck configKind) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), ck.get, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer g.Release()
	doc, err := m.storage.GetBucketConfig(g.Context(), bucket, ck.kind)
	g.Release()
	if errors.Is(err, contracts.ErrNoSuchBucketConfig) {
		if ck.fallback == nil {
			return s3api.WriteError(ctx, target, ck.notFound)
		}
		return ctx.XML(http.StatusOK, ck.fallback())
	}
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	return m.writeDocument(ctx, doc)
}

// deleteConfig serves DeleteBucketTagging, DeleteBucketCors,
// DeletePublicAccessBlock and DeleteBucketEncryption, answering 204
// whether or not the bucket had the configuration, as DeleteBucketPolicy
// does. A bucket whose encryption is deleted reports the default again:
// "This implementation of the DELETE action resets the default encryption
// for the bucket as server-side encryption with Amazon S3 managed keys
// (SSE-S3)." (DeleteBucketEncryption API reference).
func (m *Module) deleteConfig(ctx gas.Context, ck configKind) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), ck.del, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer g.Release()
	if err = m.storage.DeleteBucketConfig(g.Context(), bucket, ck.kind); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	return ctx.NoContent()
}

// writeDocument answers 200 with doc, a stored XML document.
func (m *Module) writeDocument(ctx gas.Context, doc []byte) error {
	w := ctx.ResponseWriter()
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(doc); err != nil {
		m.logger.Warn("failed to write a bucket configuration").Err("error", err).Send()
	}
	return nil
}

// getBucketPolicyStatus serves GetBucketPolicyStatus (GET
// /<bucket>?policyStatus): IsPublic is whether the bucket's policy is
// public by the rule of "The meaning of 'public'" (authz.PublicPolicy). A
// bucket without a policy is answered with 404 NoSuchBucketPolicy, which
// the API reference does not document; that is a choice.
func (m *Module) getBucketPolicyStatus(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.GetBucketPolicyStatus, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer g.Release()
	_, doc, err := m.storage.GetBucketPolicy(g.Context(), bucket)
	g.Release()
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	public, err := authz.PublicPolicy(doc)
	if err != nil {
		return s3api.WriteError(ctx, target, fmt.Errorf(
			"bucket: the stored policy of bucket %s: %w", bucket, err))
	}
	return ctx.XML(http.StatusOK, s3api.NewPolicyStatus(public))
}

// DefaultConfigs returns the configuration a new bucket is created with,
// as S3 has created buckets since April 2023: all four Block Public Access
// settings on ("All four of the bucket-level settings described in this
// post will be enabled for newly created buckets", AWS News Blog, "Heads-Up:
// Amazon S3 Security Changes Are Coming in April of 2023", in effect from
// 2023-04-27; "By default, all four Block Public Access settings are
// enabled.", "Setting Object Ownership when you create a bucket", S3 User
// Guide), and Object Ownership ownership, BucketOwnerEnforced unless the
// request named another ("By default, ObjectOwnership is set to
// BucketOwnerEnforced and ACLs are disabled.", CreateBucket API
// reference). acl, when set, is the bucket's ACL document.
func DefaultConfigs(ownership string, acl []byte) (map[contracts.BucketConfig][]byte, error) {
	block, err := s3api.Encode(s3api.NewPublicAccessBlockAll())
	if err != nil {
		return nil, err //nolint:wrapcheck // s3api's own error
	}
	controls, err := s3api.Encode(s3api.NewOwnershipControls(ownership))
	if err != nil {
		return nil, err //nolint:wrapcheck // s3api's own error
	}
	configs := map[contracts.BucketConfig][]byte{
		contracts.ConfigPublicAccessBlock: block,
		contracts.ConfigOwnership:         controls,
	}
	if acl != nil {
		configs[contracts.ConfigACL] = acl
	}
	return configs, nil
}
