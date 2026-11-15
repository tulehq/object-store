package bucket

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/lifecycle"
	"tule.dev/object-store/internal/s3api"
)

// The lifecycle configuration is served apart from configKinds: it is
// stored as a lifecycle.Stored envelope, which keeps the bucket generation
// it was put in and the x-amz-transition-default-minimum-object-size it
// was put with, and both requests answer that header. The deprecated
// PutBucketLifecycle and GetBucketLifecycle share the request lines of
// PutBucketLifecycleConfiguration and GetBucketLifecycleConfiguration, and
// are served as they are: a rule with a top-level Prefix is read and
// answered in that form.

// putBucketLifecycle serves PutBucketLifecycleConfiguration (PUT
// /<bucket>?lifecycle): the document is read, at most
// lifecycle.MaxDocument bytes, validated (lifecycle.Parse), and stored with
// the bucket's generation and the request's
// x-amz-transition-default-minimum-object-size, replacing any
// configuration the bucket had ("this will overwrite an existing lifecycle
// configuration", its API reference). It answers 200 with no body and the
// header, all_storage_classes_128K for a request without one. The
// operation is evaluated before the body is read, and the bucket pinned
// after, as putConfig does. Content-MD5 and the checksum headers are
// accepted and ignored.
func (m *Module) putBucketLifecycle(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}
	r := ctx.Request()

	caller, err := m.authz.Authorize(r, authz.PutBucketLifecycleConfiguration, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	doc, err := s3api.ReadDocument(r, lifecycle.MaxDocument, m.verifier.ReadBody)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	transitionDefault, err := lifecycle.ParseTransitionDefault(
		r.Header.Get(lifecycle.HeaderTransitionDefault))
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	cfg, err := lifecycle.Parse(doc)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	// The generation stored is the one evaluated, which Pin then holds,
	// so a bucket deleted and created again while the body was read is
	// NoSuchBucket rather than given this configuration.
	ref, err := caller.Ref(bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	stored, err := lifecycle.Encode(ref.Generation, cfg, transitionDefault)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	pinned, release, err := caller.Pin(ctx, bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer release()
	if err = m.storage.PutBucketConfig(pinned, bucket, contracts.ConfigLifecycle,
		stored); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	ctx.SetHeader(lifecycle.HeaderTransitionDefault, transitionDefault)
	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

// getBucketLifecycle serves GetBucketLifecycleConfiguration (GET
// /<bucket>?lifecycle): the stored document, with the
// x-amz-transition-default-minimum-object-size it was put with, or 404
// NoSuchLifecycleConfiguration for a bucket without one. A configuration
// stored in another generation of the bucket, which a server that did
// not know lifecycle configurations can leave when it deletes the bucket
// and creates it again, is none.
func (m *Module) getBucketLifecycle(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(),
		authz.GetBucketLifecycleConfiguration, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer g.Release()
	ref, err := g.Caller().Ref(bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	raw, err := m.storage.GetBucketConfig(g.Context(), bucket, contracts.ConfigLifecycle)
	g.Release()
	if errors.Is(err, contracts.ErrNoSuchBucketConfig) {
		return s3api.WriteError(ctx, target, s3api.ErrNoSuchLifecycleConfiguration)
	}
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	stored, _, err := lifecycle.Decode(raw)
	if err != nil {
		return s3api.WriteError(ctx, target, fmt.Errorf(
			"bucket: the stored lifecycle configuration of bucket %s: %w", bucket, err))
	}
	if stored.Generation != ref.Generation {
		return s3api.WriteError(ctx, target, s3api.ErrNoSuchLifecycleConfiguration)
	}
	ctx.SetHeader(lifecycle.HeaderTransitionDefault, stored.TransitionDefaultMinimumObjectSize)
	return m.writeDocument(ctx, []byte(stored.Document))
}

// deleteBucketLifecycle serves DeleteBucketLifecycle (DELETE
// /<bucket>?lifecycle), answering 204 whether or not the bucket had a
// lifecycle configuration, as the other configuration deletes do.
func (m *Module) deleteBucketLifecycle(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.DeleteBucketLifecycle, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer g.Release()
	if err = m.storage.DeleteBucketConfig(g.Context(), bucket,
		contracts.ConfigLifecycle); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	return ctx.NoContent()
}
