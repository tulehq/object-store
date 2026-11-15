package bucket

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"

	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// maxDeleteBody bounds a DeleteObjects request document. 1000 keys of up to
// 1024 bytes each, plus markup, stays well under it.
const maxDeleteBody = 2 << 20

// postBucket serves the POST bucket operations. Only DeleteObjects (?delete)
// is supported.
func (m *Module) postBucket(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}
	r := ctx.Request()

	if !r.URL.Query().Has("delete") {
		return s3api.WriteError(ctx, target,
			s3api.ErrNotImplemented("This POST bucket operation is not supported."))
	}
	if err := s3api.CheckSubresource(r, "delete"); err != nil {
		return s3api.WriteError(ctx, target, err)
	}

	caller, err := m.authz.Caller(r)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	// A mismatched expected owner fails the whole request with 403, not
	// each key (the DeleteObjects API reference, ExpectedBucketOwner).
	if err = caller.CheckExpectedOwner(authz.DeleteObjects); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	req, err := m.readDeleteRequest(r)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}

	// Pinning the bucket checks that it exists; each key is then evaluated
	// with the policy of the generation pinned, and deleted from it.
	pinned, release, err := caller.Pin(ctx, bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer release()

	res := s3api.NewDeleteResult()
	for _, obj := range req.Objects {
		out, dErr := m.deleteOne(pinned, caller, bucket, obj)
		if dErr == nil {
			if !req.Quiet {
				res.Deleted = append(res.Deleted, deleted(obj, out))
			}
			continue
		}
		s3Err := s3api.ToError(ctx, dErr)
		res.Errors = append(res.Errors, s3api.DeleteError{
			Key:       obj.Key,
			VersionID: obj.VersionID,
			Code:      s3Err.Code,
			Message:   s3Err.Message,
		})
	}

	return ctx.XML(http.StatusOK, res)
}

// deleted is the Deleted entry of obj, which DeleteObject answered with
// out, in the shapes of the DeleteObjects API reference's examples: a
// delete without a version that created a delete marker gives
// DeleteMarker and DeleteMarkerVersionId; one of a version gives its
// VersionId, and, when that version was a delete marker, DeleteMarker and
// DeleteMarkerVersionId, the same ID, too. An Unversioned bucket's gives
// the key alone.
func deleted(obj s3api.ObjectIdentifier, out *contracts.DeleteObjectOutput) s3api.DeletedObject {
	d := s3api.DeletedObject{Key: obj.Key}
	if obj.VersionID != "" {
		d.VersionID = out.VersionID
	}
	if out.DeleteMarker {
		d.DeleteMarker, d.DeleteMarkerVersionID = true, out.VersionID
	}
	return d
}

// deleteOne deletes one key of a DeleteObjects request, or the version of
// it obj names, which is evaluated as DeleteObject is, s3:DeleteObject or
// s3:DeleteObjectVersion, on that key alone: a key the caller may not
// delete is answered with its own entry, AccessDenied with the message
// "Access Denied", as the DeleteObjects API reference's example shows it.
func (m *Module) deleteOne(
	ctx context.Context,
	caller *authz.Caller,
	bucket string,
	obj s3api.ObjectIdentifier,
) (*contracts.DeleteObjectOutput, error) {
	t := s3api.Target{Bucket: bucket, Key: obj.Key, VersionID: obj.VersionID}
	//nolint:contextcheck // the policy was read when the bucket was pinned
	if err := caller.Authorize(authz.DeleteObjects, t); err != nil {
		var s3Err *s3api.Error
		if errors.As(err, &s3Err) && s3Err.Code == "AccessDenied" {
			return nil, s3api.NewError(http.StatusForbidden, "AccessDenied", "Access Denied")
		}
		return nil, err
	}
	out, err := m.storage.DeleteObject(
		ctx,
		contracts.ObjectRef{Bucket: bucket, Key: obj.Key, Version: obj.VersionID},
	)
	if err != nil {
		return nil, fmt.Errorf("delete %q: %w", obj.Key, err)
	}
	return out, nil
}

// readDeleteRequest reads and decodes a DeleteObjects document. The whole
// body is read first, and verified against the signature when it is signed.
func (m *Module) readDeleteRequest(r *http.Request) (*s3api.Delete, error) {
	body, err := s3api.ReadDocument(r, maxDeleteBody, m.verifier.ReadBody)
	if err != nil {
		return nil, fmt.Errorf("read delete request: %w", err)
	}
	var req s3api.Delete
	if err = xml.Unmarshal(body, &req); err != nil {
		return nil, s3api.ErrMalformedXML()
	}
	if len(req.Objects) == 0 || len(req.Objects) > s3api.MaxDeleteObjects {
		return nil, s3api.ErrMalformedXML()
	}
	return &req, nil
}
