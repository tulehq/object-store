package bucket

import (
	"errors"
	"net/http"

	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/s3api"
)

// putBucketPolicy serves PutBucketPolicy (PUT /<bucket>?policy): the body
// is the policy document, at most authz.MaxBucketPolicySize bytes, which is
// validated with authz.ValidateBucketPolicy before it is stored. The
// operation is evaluated before the body is read. Content-MD5, the
// checksum headers and x-amz-confirm-remove-self-bucket-access are
// accepted and ignored. S3's example answers 204 No Content, while its
// response syntax says 200; this answers 204, as the example does.
func (m *Module) putBucketPolicy(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}
	r := ctx.Request()

	caller, err := m.authz.Authorize(r, authz.PutBucketPolicy, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	doc, err := s3api.ReadDocument(r, authz.MaxBucketPolicySize, m.verifier.ReadBody)
	if err != nil {
		var s3Err *s3api.Error
		if errors.As(err, &s3Err) && s3Err.Code == "MaxMessageLengthExceeded" {
			err = authz.ErrPolicyTooLarge()
		}
		return s3api.WriteError(ctx, target, err)
	}
	if len(doc) == 0 {
		// The error S3 lists for an empty request document.
		return s3api.WriteError(ctx, target, s3api.NewError(http.StatusBadRequest,
			"MissingRequestBodyError", "Request body is empty."))
	}
	if err = authz.ValidateBucketPolicy(bucket, doc); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	// Pinned after the body is read, so a slow body holds no lock, and to
	// the generation evaluated, so a bucket deleted and created again while
	// the body was read is NoSuchBucket, not given this policy.
	pinned, release, err := caller.Pin(ctx, bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer release()
	if err = m.storage.PutBucketPolicy(pinned, bucket, doc); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	return ctx.NoContent()
}

// getBucketPolicy serves GetBucketPolicy (GET /<bucket>?policy): the
// policy as it was stored, as JSON, or 404 NoSuchBucketPolicy.
func (m *Module) getBucketPolicy(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.GetBucketPolicy, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer g.Release()
	_, doc, err := m.storage.GetBucketPolicy(g.Context(), bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	w := ctx.ResponseWriter()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err = w.Write(doc); err != nil {
		m.logger.Warn("failed to write the bucket policy").Err("error", err).Send()
	}
	return nil
}

// deleteBucketPolicy serves DeleteBucketPolicy (DELETE /<bucket>?policy),
// answering 204 whether or not the bucket had a policy; S3 does not
// document the second case, so that is this module's choice.
func (m *Module) deleteBucketPolicy(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.DeleteBucketPolicy, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer g.Release()
	if err = m.storage.DeleteBucketPolicy(g.Context(), bucket); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	return ctx.NoContent()
}
