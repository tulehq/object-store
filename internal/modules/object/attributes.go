package object

import (
	"net/http"
	"slices"
	"strings"

	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// The attributes GetObjectAttributes answers ("Valid Values: ETag |
// Checksum | ObjectParts | StorageClass | ObjectSize", its API reference).
const (
	attrETag         = "ETag"
	attrChecksum     = "Checksum"
	attrObjectParts  = "ObjectParts"
	attrStorageClass = "StorageClass"
	attrObjectSize   = "ObjectSize"
)

var objectAttributes = []string{
	attrETag, attrChecksum, attrObjectParts, attrStorageClass, attrObjectSize,
}

// getObjectAttributes serves GetObjectAttributes (GET
// /<bucket>/<key>?attributes): the attributes x-amz-object-attributes
// names, "Fields that you do not specify are not returned.", with the
// object's Last-Modified. Checksum is never answered, since no checksum is
// stored; the element is not required. StorageClass is STANDARD and the
// ETag unquoted, as the AWS CLI's get-object-attributes example shows
// them. ObjectParts is answered for an object completed from a multipart
// upload, one page of its parts as ListParts pages them, by
// x-amz-max-parts and x-amz-part-number-marker, each with its number and
// size. A missing object is NoSuchKey or AccessDenied as for GetObject.
func (m *Module) getObjectAttributes(ctx gas.Context, t s3api.Target) error {
	r := ctx.Request()
	// s3:GetObject, then s3:GetObjectAttributes, both on the object.
	g, err := m.authz.AuthorizeAndPin(ctx, r, authz.GetObjectAttributes, t, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()

	q, err := attributesQuery(r)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	info, err := m.storage.StatObject(g.Context(), t.Bucket, t.Key)
	if err != nil {
		return s3api.WriteError(ctx, t, g.NotFound(t.Bucket, err))
	}
	if err = checkReadEncryption(r, info); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	ctx.SetHeader("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	return ctx.XML(http.StatusOK, q.response(info))
}

// attributesRequest is what a GetObjectAttributes asks for: its
// attributes, and the page of parts.
type attributesRequest struct {
	attrs            []string
	maxParts, marker int
}

// attributesQuery reads r's x-amz-object-attributes, x-amz-max-parts
// (1000 when absent, and at most 1000, as for ListParts) and
// x-amz-part-number-marker.
func attributesQuery(r *http.Request) (*attributesRequest, error) {
	attrs, err := requestedAttributes(r)
	if err != nil {
		return nil, err
	}
	maxParts, err := nonNegativeParam(r.Header.Get("X-Amz-Max-Parts"), "x-amz-max-parts",
		maxListParts)
	if err != nil {
		return nil, err
	}
	marker, err := nonNegativeParam(r.Header.Get("X-Amz-Part-Number-Marker"),
		"x-amz-part-number-marker", 0)
	if err != nil {
		return nil, err
	}
	return &attributesRequest{
		attrs: attrs, maxParts: min(maxParts, maxListParts), marker: marker,
	}, nil
}

// response is the GetObjectAttributes response of q for the object info
// describes.
func (q *attributesRequest) response(
	info *contracts.ObjectInfo,
) *s3api.GetObjectAttributesResponse {
	res := s3api.NewGetObjectAttributesResponse()
	if slices.Contains(q.attrs, attrETag) {
		res.ETag = info.ETag
	}
	if slices.Contains(q.attrs, attrStorageClass) {
		res.StorageClass = "STANDARD"
	}
	if slices.Contains(q.attrs, attrObjectSize) {
		res.ObjectSize = &info.Size
	}
	if slices.Contains(q.attrs, attrObjectParts) && len(info.PartSizes) > 0 {
		res.ObjectParts = objectParts(info, q.marker, q.maxParts)
	}
	return res
}

// requestedAttributes returns the attributes r's x-amz-object-attributes
// names, each value of the header a comma-separated list. A request
// naming none, or one GetObjectAttributes does not answer, is 400
// InvalidArgument, a choice: S3 documents neither.
func requestedAttributes(r *http.Request) ([]string, error) {
	var attrs []string
	for _, v := range r.Header.Values("X-Amz-Object-Attributes") {
		for a := range strings.SplitSeq(v, ",") {
			a = strings.TrimSpace(a)
			if !slices.Contains(objectAttributes, a) {
				return nil, s3api.ErrInvalidArgument("Invalid attribute name specified.")
			}
			attrs = append(attrs, a)
		}
	}
	if len(attrs) == 0 {
		return nil, s3api.ErrInvalidArgument(
			"The x-amz-object-attributes header specifying the attributes to be retrieved " +
				"is either missing or empty")
	}
	return attrs, nil
}

// objectParts is the page of info's parts after part marker, at most
// maxParts of them.
func objectParts(info *contracts.ObjectInfo, marker, maxParts int) *s3api.ObjectParts {
	op := &s3api.ObjectParts{
		PartNumberMarker: marker, MaxParts: maxParts, PartsCount: len(info.PartSizes),
	}
	for i, size := range info.PartSizes {
		n := i + 1
		if n <= marker {
			continue
		}
		if len(op.Parts) == maxParts {
			op.IsTruncated = true
			break
		}
		op.Parts = append(op.Parts, s3api.ObjectPart{PartNumber: n, Size: size})
	}
	if op.IsTruncated && len(op.Parts) > 0 {
		op.NextPartNumberMarker = op.Parts[len(op.Parts)-1].PartNumber
	}
	return op
}
