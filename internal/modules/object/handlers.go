package object

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// target returns the bucket and key a request addresses. The key is taken from
// the decoded URL path rather than the route wildcard, which chi leaves
// percent-encoded whenever the client escaped characters Go would not.
func target(ctx gas.Context) s3api.Target {
	bucket := ctx.Param("bucket")
	key := strings.TrimPrefix(ctx.Request().URL.Path, "/"+bucket+"/")
	return s3api.Target{Bucket: bucket, Key: key}
}

// getObject streams an object. Range requests and the If-* conditionals are
// handled by http.ServeContent.
func (m *Module) getObject(ctx gas.Context) error {
	t := target(ctx)
	r := ctx.Request()
	w := ctx.ResponseWriter()

	if r.URL.Query().Has("uploadId") {
		if err := s3api.CheckSubresource(r, "uploadId"); err != nil {
			return s3api.WriteError(ctx, t, err)
		}
		return m.listParts(ctx, t)
	}

	if err := s3api.CheckSubresource(r, "partNumber"); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	g, err := m.authz.AuthorizeAndPin(ctx, r, authz.GetObject, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()

	obj, err := m.storage.GetObject(g.Context(), t.Bucket, t.Key)
	// The object is open, and its body is the backend's open data file,
	// which "keeps reading the contents it opened when the object is
	// replaced or deleted" (gas storage/dir), so streaming it needs no pin
	// and a slow reader does not hold DeleteBucket. Nothing below writes,
	// and NotFound uses the policy already read.
	g.Release()
	if err != nil {
		return s3api.WriteError(ctx, t, g.NotFound(t.Bucket, err))
	}
	defer func() {
		if cErr := obj.Body.Close(); cErr != nil {
			m.logger.Warn("failed to close object body").Err("error", cErr).Send()
		}
	}()
	info := &obj.ObjectInfo

	serve, err := partRequest(w, r, info)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	// The storage service's body seeks within the object bytes, which
	// http.ServeContent needs for ranges.
	rs, ok := obj.Body.(io.ReadSeeker)
	if !ok {
		return s3api.WriteError(ctx, t, fmt.Errorf("object body %T is not seekable", obj.Body))
	}

	setObjectHeaders(w, info)
	applyResponseOverrides(w, r)
	http.ServeContent(w, serve, "", info.LastModified, rs)
	return nil
}

func (m *Module) headObject(ctx gas.Context) error {
	t := target(ctx)
	r := ctx.Request()

	if err := s3api.CheckSubresource(r, "partNumber"); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	g, err := m.authz.AuthorizeAndPin(ctx, r, authz.HeadObject, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()

	info, err := m.storage.StatObject(g.Context(), t.Bucket, t.Key)
	if err != nil {
		// A HEAD's error has no body; the status alone tells 403 from 404.
		return s3api.WriteError(ctx, t, g.NotFound(t.Bucket, err))
	}

	w := ctx.ResponseWriter()
	setObjectHeaders(w, info)

	if v := r.URL.Query().Get("partNumber"); v != "" {
		return m.headPart(ctx, t, info, v)
	}

	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
	return nil
}

// headPart answers a HEAD with ?partNumber. A plain object's part 1 is the
// whole object.
func (m *Module) headPart(
	ctx gas.Context,
	t s3api.Target,
	info *contracts.ObjectInfo,
	v string,
) error {
	span, err := locatePart(info, v)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	h := ctx.ResponseWriter().Header()
	if !span.ok {
		h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
		ctx.ResponseWriter().WriteHeader(http.StatusOK)
		return nil
	}

	h.Set("X-Amz-Mp-Parts-Count", strconv.Itoa(span.count))
	h.Set("Content-Length", strconv.FormatInt(span.length, 10))
	if cr := span.contentRange(info.Size); cr != "" {
		h.Set("Content-Range", cr)
	}
	ctx.ResponseWriter().WriteHeader(http.StatusPartialContent)
	return nil
}

// putObject serves PutObject, CopyObject when x-amz-copy-source is set, and
// UploadPart when the query names an upload and part.
func (m *Module) putObject(ctx gas.Context) error {
	t := target(ctx)
	r := ctx.Request()

	if q := r.URL.Query(); q.Has("uploadId") || q.Has("partNumber") {
		if err := s3api.CheckSubresource(r, "uploadId", "partNumber"); err != nil {
			return s3api.WriteError(ctx, t, err)
		}
		return m.uploadPart(ctx, t)
	}

	if err := s3api.CheckSubresource(r); err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	if r.Header.Get("X-Amz-Copy-Source") != "" {
		return m.copyObject(ctx, t)
	}
	return m.uploadObject(ctx, t)
}

// uploadObject serves PutObject.
func (m *Module) uploadObject(ctx gas.Context, t s3api.Target) error {
	r := ctx.Request()
	g, err := m.authz.AuthorizeAndPin(ctx, r, authz.PutObject, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()

	cond, err := precondition(r)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	body, err := requestBody(r)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	info, err := m.storage.PutObject(g.Context(), t.Bucket, t.Key, body, putInput(r), cond)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	if etag := s3api.ETag(info.ETag); etag != "" {
		ctx.SetHeader("ETag", etag)
	}
	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

func (m *Module) copyObject(ctx gas.Context, t s3api.Target) error {
	in, err := copyInput(ctx.Request(), t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	// s3:PutObject on the destination, then s3:GetObject on the source.
	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.CopyObject, t,
		s3api.Target{Bucket: in.SrcBucket, Key: in.SrcKey})
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()

	info, err := m.storage.CopyObject(g.Context(), in)
	if err != nil {
		return s3api.WriteError(ctx, t, m.copyNotFound(g, in, err))
	}

	return ctx.XML(http.StatusOK, s3api.NewCopyObjectResult(info.ETag, info.LastModified))
}

// copyNotFound is the error for a CopyObject that failed with err. A copy
// finds no object either reading its source or, under If-Match, at its
// destination; the source is checked again to tell which bucket's
// s3:ListBucket decides between NoSuchKey and AccessDenied.
func (m *Module) copyNotFound(g *authz.Grant, in contracts.CopyObjectInput, err error) error {
	if !errors.Is(err, storage.ErrObjectNotFound) {
		return err
	}
	bucket := in.DstBucket
	if _, sErr := m.storage.StatObject(g.Context(), in.SrcBucket, in.SrcKey); errors.Is(sErr,
		storage.ErrObjectNotFound) {
		bucket = in.SrcBucket
	}
	return g.NotFound(bucket, err)
}

// precondition reads the If-Match and If-None-Match headers of a write. S3
// only accepts "*" for If-None-Match.
func precondition(r *http.Request) (contracts.Precondition, error) {
	var cond contracts.Precondition

	switch v := strings.TrimSpace(r.Header.Get("If-None-Match")); v {
	case "":
	case "*":
		cond.IfNoneMatch = true
	default:
		return cond, s3api.ErrNotImplemented(
			"If-None-Match only supports the * value on writes.")
	}

	cond.IfMatch = strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	return cond, nil
}

// requestBody returns the object bytes of an upload, decoding aws-chunked
// bodies. Either way the reader reaches EOF only once the whole body is read,
// so the body is checked against its payload hash before the upload ends.
func requestBody(r *http.Request) (io.Reader, error) {
	chunked, err := s3api.IsAWSChunked(r)
	if err != nil {
		return nil, err //nolint:wrapcheck // an S3 error, answered as is
	}
	if !chunked {
		return r.Body, nil
	}

	decodedLen := int64(-1)
	if v := r.Header.Get("X-Amz-Decoded-Content-Length"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return nil, s3api.ErrInvalidArgument("invalid x-amz-decoded-content-length")
		}
		decodedLen = n
	}

	return s3api.NewChunkedReader(r.Body, decodedLen), nil
}

func (m *Module) deleteObject(ctx gas.Context) error {
	t := target(ctx)
	r := ctx.Request()

	if r.URL.Query().Has("uploadId") {
		if err := s3api.CheckSubresource(r, "uploadId"); err != nil {
			return s3api.WriteError(ctx, t, err)
		}
		return m.abortMultipartUpload(ctx, t)
	}

	if err := s3api.CheckSubresource(r); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	g, err := m.authz.AuthorizeAndPin(ctx, r, authz.DeleteObject, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()

	if err := m.storage.DeleteObject(g.Context(), t.Bucket, t.Key); err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	return ctx.NoContent()
}
