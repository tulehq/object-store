package bucket

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/gasmod/gas"
	"tule.dev/identity/principals"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/s3api"
)

// putBucketPolicy serves PutBucketPolicy (PUT /<bucket>?policy): the body
// is the policy document, at most authz.MaxBucketPolicySize bytes, which is
// validated with authz.ValidateBucketPolicy and bound (see bind) before it
// is stored. The operation is evaluated before the body is read, so a
// caller it denies never reaches bind or identity. Content-MD5, the
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
	bound, err := m.bind(r.Context(), doc)
	if err != nil {
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
	if err = m.storage.PutBucketPolicy(pinned, bucket, bound); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	return ctx.NoContent()
}

// getBucketPolicy serves GetBucketPolicy (GET /<bucket>?policy): the
// policy as it was stored, as JSON, with its principals shown as S3 shows
// them (each unique ID whose user or role still exists as its current ARN,
// any other as the ID), or 404 NoSuchBucketPolicy. A failure to show them,
// such as identity being unreachable, fails the request with 500.
func (m *Module) getBucketPolicy(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.GetBucketPolicy, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer g.Release()
	_, doc, err := m.storage.GetBucketPolicy(g.Context(), bucket)
	// Shown once read, so the bucket is not held while identity is asked.
	g.Release()
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	shown, err := m.principals.Show(ctx.Request().Context(), string(doc))
	if err != nil {
		return s3api.WriteError(ctx, target, fmt.Errorf(
			"bucket: show the principals of the policy of bucket %s: %w", bucket, err))
	}
	w := ctx.ResponseWriter()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err = w.Write([]byte(shown)); err != nil {
		m.logger.Warn("failed to write the bucket policy").Err("error", err).Send()
	}
	return nil
}

// bind returns doc with its principals bound to unique IDs, as S3 stores a
// bucket policy: each IAM user and role ARN becomes the unique ID of the
// user or role that has it now, so one deleted and created again under the
// same name is not granted what the policy grants. An ARN no user or role
// has is S3's MalformedPolicy, "Invalid principal in policy". A unique ID
// can be longer than the ARN it replaces, so a bound document over
// authz.MaxBucketPolicySize is refused as one sent over it is. Any other
// failure, such as identity being unreachable, is an internal error, and
// nothing is stored.
func (m *Module) bind(ctx context.Context, doc []byte) ([]byte, error) {
	bound, err := m.principals.Bind(ctx, string(doc))
	if errors.Is(err, principals.ErrInvalidPrincipal) {
		return nil, authz.ErrMalformedPolicy("Invalid principal in policy")
	}
	if err != nil {
		return nil, fmt.Errorf("bucket: bind the principals of a bucket policy: %w", err)
	}
	if len(bound) > authz.MaxBucketPolicySize {
		return nil, authz.ErrPolicyTooLarge()
	}
	return []byte(bound), nil
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
