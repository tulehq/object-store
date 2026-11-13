package object

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// maxCompleteBody bounds a CompleteMultipartUpload request document: 10000
// parts at around 100 bytes each, with room to spare.
const maxCompleteBody = 2 << 20

// maxListParts is the most parts one ListParts response holds, and the
// default page size.
const maxListParts = 1000

// postObject serves the POST object operations: CreateMultipartUpload
// (?uploads) and CompleteMultipartUpload (?uploadId).
func (m *Module) postObject(ctx gas.Context) error {
	t := target(ctx)
	r := ctx.Request()
	q := r.URL.Query()

	switch {
	case q.Has("uploads"):
		if err := s3api.CheckSubresource(r, "uploads"); err != nil {
			return s3api.WriteError(ctx, t, err)
		}
		return m.createMultipartUpload(ctx, t)
	case q.Has("uploadId"):
		if err := s3api.CheckSubresource(r, "uploadId"); err != nil {
			return s3api.WriteError(ctx, t, err)
		}
		return m.completeMultipartUpload(ctx, t, q.Get("uploadId"))
	default:
		return s3api.WriteError(ctx, t,
			s3api.ErrNotImplemented("This POST object operation is not supported."))
	}
}

func (m *Module) createMultipartUpload(ctx gas.Context, t s3api.Target) error {
	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.CreateMultipartUpload, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()

	id, err := m.storage.CreateMultipartUpload(
		g.Context(),
		t.Bucket,
		t.Key,
		putInput(ctx.Request()),
	)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	return ctx.XML(http.StatusOK, s3api.NewInitiateMultipartUploadResult(t.Bucket, t.Key, id))
}

func (m *Module) uploadPart(ctx gas.Context, t s3api.Target) error {
	r := ctx.Request()

	if r.Header.Get("X-Amz-Copy-Source") != "" {
		return s3api.WriteError(ctx, t, s3api.ErrNotImplemented("UploadPartCopy is not supported."))
	}
	g, err := m.authz.AuthorizeAndPin(ctx, r, authz.UploadPart, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()

	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		return s3api.WriteError(
			ctx,
			t,
			s3api.ErrInvalidArgument("uploadId is required with partNumber"),
		)
	}

	n, err := parsePartNumber(r.URL.Query().Get("partNumber"))
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	body, err := requestBody(r)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	etag, err := m.storage.UploadPart(g.Context(), t.Bucket, t.Key, uploadID, n, body)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	ctx.SetHeader("ETag", s3api.ETag(etag))
	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

func (m *Module) completeMultipartUpload(ctx gas.Context, t s3api.Target, uploadID string) error {
	r := ctx.Request()
	caller, err := m.authz.Authorize(r, authz.CompleteMultipartUpload, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	cond, err := precondition(r)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	// The whole body is read before it is decoded, and verified against the
	// signature when it is signed.
	body, err := s3api.ReadDocument(r, maxCompleteBody, m.verifier.ReadBody)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	var req s3api.CompleteMultipartUpload
	if err = xml.Unmarshal(body, &req); err != nil || len(req.Parts) == 0 {
		return s3api.WriteError(ctx, t, s3api.ErrMalformedXML())
	}

	parts := make([]contracts.CompletedPart, len(req.Parts))
	for i, p := range req.Parts {
		parts[i] = contracts.CompletedPart{
			PartNumber: p.PartNumber,
			ETag:       strings.Trim(strings.TrimSpace(p.ETag), `"`),
		}
	}

	// Pinned after the document is read, so a slow body holds no lock, and
	// to the generation evaluated; the upload must be of that generation too.
	pinned, release, err := caller.Pin(ctx, t.Bucket)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer release()
	info, err := m.storage.CompleteMultipartUpload(pinned, t.Bucket, t.Key, uploadID, parts, cond)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	return ctx.XML(http.StatusOK, s3api.NewCompleteMultipartUploadResult(
		objectURL(r, t), t.Bucket, t.Key, info.ETag))
}

func (m *Module) abortMultipartUpload(ctx gas.Context, t s3api.Target) error {
	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.AbortMultipartUpload, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()
	uploadID := ctx.Request().URL.Query().Get("uploadId")
	if err := m.storage.AbortMultipartUpload(g.Context(), t.Bucket, t.Key, uploadID); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	return ctx.NoContent()
}

// listParts serves ListParts, paging by part-number-marker and max-parts.
func (m *Module) listParts(ctx gas.Context, t s3api.Target) error {
	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.ListParts, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()
	q := ctx.Request().URL.Query()
	uploadID := q.Get("uploadId")

	maxParts, err := nonNegativeParam(q.Get("max-parts"), "max-parts", maxListParts)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	maxParts = min(maxParts, maxListParts)
	marker, err := nonNegativeParam(q.Get("part-number-marker"), "part-number-marker", 0)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	parts, err := m.storage.ListParts(g.Context(), t.Bucket, t.Key, uploadID)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	res := s3api.NewListPartsResult()
	res.Bucket, res.Key, res.UploadID = t.Bucket, t.Key, uploadID
	res.StorageClass = "STANDARD"
	res.PartNumberMarker = marker
	res.MaxParts = maxParts

	for _, p := range parts {
		if p.PartNumber <= marker {
			continue
		}
		if len(res.Parts) == maxParts {
			res.IsTruncated = true
			break
		}
		res.Parts = append(res.Parts, s3api.Part{
			PartNumber:   p.PartNumber,
			ETag:         s3api.ETag(p.ETag),
			Size:         p.Size,
			LastModified: s3api.Timestamp(p.LastModified),
		})
	}
	if res.IsTruncated {
		res.NextPartNumberMarker = res.Parts[len(res.Parts)-1].PartNumber
	}

	return ctx.XML(http.StatusOK, res)
}

// nonNegativeParam parses the query parameter name, valued v, as a
// non-negative integer, returning def when it is absent.
func nonNegativeParam(v, name string, def int) (int, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, s3api.ErrInvalidArgument(name + " must be a non-negative integer")
	}
	return n, nil
}

func parsePartNumber(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > contracts.MaxPartNumber {
		return 0, s3api.ErrInvalidArgument(
			"Part number must be an integer between 1 and " + strconv.Itoa(contracts.MaxPartNumber))
	}
	return n, nil
}

// objectURL is the path-style URL of the object t on the host r was sent to.
func objectURL(r *http.Request, t s3api.Target) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}

	segments := strings.Split(t.Key, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}

	return scheme + "://" + r.Host + "/" + t.Bucket + "/" + strings.Join(segments, "/")
}
