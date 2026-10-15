package bucket

import (
	"encoding/xml"
	"fmt"
	"net/http"

	"github.com/gasmod/gas"

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

	req, err := readDeleteRequest(ctx.ResponseWriter(), r)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}

	if _, err = m.storage.HeadBucket(ctx, bucket); err != nil {
		return s3api.WriteError(ctx, target, err)
	}

	res := s3api.NewDeleteResult()
	for _, obj := range req.Objects {
		dErr := m.deleteOne(ctx, bucket, obj)
		if dErr == nil {
			if !req.Quiet {
				res.Deleted = append(res.Deleted, s3api.DeletedObject{Key: obj.Key})
			}
			continue
		}
		s3Err := s3api.ToError(ctx, dErr)
		res.Errors = append(res.Errors, s3api.DeleteError{
			Key:     obj.Key,
			Code:    s3Err.Code,
			Message: s3Err.Message,
		})
	}

	return ctx.XML(http.StatusOK, res)
}

func (m *Module) deleteOne(ctx gas.Context, bucket string, obj s3api.ObjectIdentifier) error {
	if obj.VersionID != "" {
		return s3api.ErrNotImplemented("Deleting a specific object version is not supported.")
	}
	if err := m.storage.Delete(ctx, obj.Key, gas.InBucket(bucket)); err != nil {
		return fmt.Errorf("delete %q: %w", obj.Key, err)
	}
	return nil
}

func readDeleteRequest(w http.ResponseWriter, r *http.Request) (*s3api.Delete, error) {
	var req s3api.Delete
	dec := xml.NewDecoder(http.MaxBytesReader(w, r.Body, maxDeleteBody))
	// An oversized body surfaces as a decode error too.
	if err := dec.Decode(&req); err != nil {
		return nil, s3api.ErrMalformedXML()
	}
	if len(req.Objects) == 0 || len(req.Objects) > s3api.MaxDeleteObjects {
		return nil, s3api.ErrMalformedXML()
	}
	return &req, nil
}
