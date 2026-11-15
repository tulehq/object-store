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

	// A request names one operation: any other subresource with it, such
	// as ?versionId, is one not served with it.
	sub := s3api.Subresource(r)
	if err := s3api.CheckSubresource(r, sub); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	switch sub {
	case "uploadId":
		return m.listParts(ctx, t)
	case "acl":
		return m.getObjectACL(ctx, t)
	case "tagging":
		return m.getObjectTagging(ctx, t)
	case "attributes":
		return m.getObjectAttributes(ctx, t)
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
	return m.serveObject(ctx, g.Caller(), t, obj)
}

// serveObject answers caller's GetObject of t with obj, open, if it is
// the object the request was evaluated for (authz.CheckObject).
func (m *Module) serveObject(
	ctx gas.Context,
	caller *authz.Caller,
	t s3api.Target,
	obj *contracts.Object,
) error {
	r := ctx.Request()
	w := ctx.ResponseWriter()
	info := &obj.ObjectInfo
	if err := caller.CheckObject(t, info); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	if err := checkReadEncryption(r, info); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	if err := setTagCount(w, caller, t, info.Tags); err != nil {
		return s3api.WriteError(ctx, t, err)
	}

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

	// The object is opened, rather than stated, to read its tags with it.
	obj, err := m.storage.GetObject(g.Context(), t.Bucket, t.Key)
	if err != nil {
		// A HEAD's error has no body; the status alone tells 403 from 404.
		return s3api.WriteError(ctx, t, g.NotFound(t.Bucket, err))
	}
	if err = obj.Body.Close(); err != nil {
		return s3api.WriteError(ctx, t, fmt.Errorf("close object %q: %w", t.Key, err))
	}
	info := &obj.ObjectInfo
	if err = g.CheckObject(t, info); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	if err = checkReadEncryption(r, info); err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	w := ctx.ResponseWriter()
	if err = setTagCount(w, g.Caller(), t, info.Tags); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	setObjectHeaders(w, info)

	if v := r.URL.Query().Get("partNumber"); v != "" {
		return m.headPart(ctx, t, info, v)
	}

	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
	return nil
}

// checkReadEncryption refuses a GET or HEAD carrying
// x-amz-server-side-encryption for an object not stored with a
// customer-provided key: "Encryption request headers, like
// x-amz-server-side-encryption, should not be sent for the GetObject
// requests, if your object uses" SSE-S3, SSE-KMS or DSSE-KMS; "you'll get
// an HTTP 400 Bad Request error" (GetObject API reference, and HeadObject's
// likewise). The reference gives no code; InvalidArgument is a choice. An
// SSE-C object's read with the header, which S3 does not document, is
// served.
func checkReadEncryption(r *http.Request, info *contracts.ObjectInfo) error {
	if r.Header.Get(s3api.HeaderSSE) == "" || info.Headers[s3api.HeaderSSECAlgorithm] != "" {
		return nil
	}
	return s3api.ErrInvalidArgument("x-amz-server-side-encryption is not accepted on a read " +
		"of an object stored with server-side encryption.")
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

	sub := s3api.Subresource(r)
	if err := s3api.CheckSubresource(r, sub); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	switch sub {
	case "acl":
		return m.putObjectACL(ctx, t)
	case "tagging":
		return m.putObjectTagging(ctx, t)
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

	ws, err := m.settings(g.Context(), r, g.Caller(), t.Bucket, true)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	body, err := requestBody(r)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	info, err := m.storage.PutObject(g.Context(), t.Bucket, t.Key, body, ws.putInput(r), cond)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	if etag := s3api.ETag(info.ETag); etag != "" {
		ctx.SetHeader("ETag", etag)
	}
	setEncryptionHeaders(ctx.ResponseWriter(), ws.encryption)
	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

// copyObject serves CopyObject. Under the COPY tagging directive a source
// with tags also needs s3:GetObjectTagging on it and s3:PutObjectTagging
// on the destination, and its tags are the destination's
// s3:RequestObjectTag keys (see authz). The copy is made only of the
// source the request was evaluated for, with the tags it was evaluated
// with (authz.CheckObject).
func (m *Module) copyObject(ctx gas.Context, t s3api.Target) error {
	in, err := copyInput(ctx.Request(), t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	src := s3api.Target{Bucket: in.SrcBucket, Key: in.SrcKey}
	// s3:PutObject on the destination, then s3:GetObject on the source.
	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.CopyObject, t, src)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()

	// The copy is encrypted and given an ACL as its own request says, or
	// its bucket's default, not as its source was.
	ws, err := m.settings(g.Context(), ctx.Request(), g.Caller(), t.Bucket,
		replacesTags(ctx.Request()))
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	// Every encryption header is named, so one the source had and the copy
	// does not is removed.
	in.Headers = map[string]string{}
	for _, k := range s3api.EncryptionHeaderNames {
		in.Headers[k] = ""
	}
	for k, v := range ws.encryption {
		in.Headers[k] = v
	}
	in.ACL = ws.acl
	// Under COPY, the default, the copy takes its source's tags and the
	// request's x-amz-tagging is not read.
	in.CopyTags, in.Tags = !replacesTags(ctx.Request()), ws.tags
	in.SourceCheck = g.ObjectCheck(src)

	info, err := m.storage.CopyObject(g.Context(), in)
	if err != nil {
		return s3api.WriteError(ctx, t, m.copyNotFound(g, in, err))
	}

	setEncryptionHeaders(ctx.ResponseWriter(), ws.encryption)
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

// replacesTags reports whether r, a CopyObject, gives the copy its own
// tags, x-amz-tagging-directive REPLACE, rather than its source's, COPY,
// the default (CopyObject API reference). copyInput refuses any other
// directive.
func replacesTags(r *http.Request) bool {
	return r.Header.Get(s3api.HeaderTaggingDirective) == "REPLACE"
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

	if s3api.Subresource(r) == "tagging" {
		if err := s3api.CheckSubresource(r, "tagging"); err != nil {
			return s3api.WriteError(ctx, t, err)
		}
		return m.deleteObjectTagging(ctx, t)
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
