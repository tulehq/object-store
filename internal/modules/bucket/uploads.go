package bucket

import (
	"net/http"
	"strings"

	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// listMultipartUploads serves ListMultipartUploads. Uploads come back sorted by
// key and then by initiation time, so a key-marker and upload-id-marker pair
// resumes right after the upload they name.
func (m *Module) listMultipartUploads(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	q, err := parseUploadsQuery(ctx)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}

	uploads, err := m.storage.ListMultipartUploads(ctx, bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}

	res := q.result(bucket)
	p := uploadsPage{query: q, res: res}
	for _, u := range uploads {
		if !p.add(u) {
			break
		}
	}
	if !res.IsTruncated {
		res.NextKeyMarker, res.NextUploadIDMarker = "", ""
	}

	return ctx.XML(http.StatusOK, res)
}

// uploadsQuery is a parsed ListMultipartUploads request.
type uploadsQuery struct {
	encode       func(string) string
	prefix       string
	delimiter    string
	keyMarker    string
	idMarker     string
	encodingType string
	maxUploads   int
}

func parseUploadsQuery(ctx gas.Context) (*uploadsQuery, error) {
	encodingType, err := parseEncodingType(ctx)
	if err != nil {
		return nil, err
	}

	maxUploads, err := nonNegative(ctx.Query("max-uploads"), "max-uploads", defaultMaxKeys)
	if err != nil {
		return nil, err
	}

	q := &uploadsQuery{
		encode:       keyEncoder(encodingType),
		prefix:       ctx.Query("prefix"),
		delimiter:    ctx.Query("delimiter"),
		keyMarker:    ctx.Query("key-marker"),
		encodingType: encodingType,
		maxUploads:   min(maxUploads, defaultMaxKeys),
	}
	// S3 ignores upload-id-marker without a key-marker.
	if q.keyMarker != "" {
		q.idMarker = ctx.Query("upload-id-marker")
	}
	return q, nil
}

// result returns the response document with the request fields echoed.
func (q *uploadsQuery) result(bucket string) *s3api.ListMultipartUploadsResult {
	res := s3api.NewListMultipartUploadsResult()
	res.Bucket = bucket
	res.KeyMarker = q.encode(q.keyMarker)
	res.UploadIDMarker = q.idMarker
	res.Prefix = q.encode(q.prefix)
	res.Delimiter = q.encode(q.delimiter)
	res.EncodingType = q.encodingType
	res.MaxUploads = q.maxUploads
	return res
}

// uploadsPage fills one ListMultipartUploads response from uploads fed to it
// in listing order.
type uploadsPage struct {
	query      *uploadsQuery
	res        *s3api.ListMultipartUploadsResult
	lastPrefix string
	count      int
	// pastIDMarker turns true once the upload named by upload-id-marker has
	// gone by; uploads of the key-marker key after it are listed.
	pastIDMarker bool
}

// add lists u if it belongs on the page, and returns false once the page is
// full.
func (p *uploadsPage) add(u contracts.UploadInfo) bool {
	q := p.query
	if p.beforeMarker(u) {
		return true
	}

	cp := commonPrefix(u.Key, q.prefix, q.delimiter)
	if cp != "" && (cp == p.lastPrefix || cp <= q.keyMarker) {
		return true
	}
	if p.count == q.maxUploads {
		p.res.IsTruncated = true
		return false
	}
	p.count++

	if cp != "" {
		p.res.CommonPrefixes = append(
			p.res.CommonPrefixes,
			s3api.CommonPrefix{Prefix: q.encode(cp)},
		)
		p.res.NextKeyMarker, p.res.NextUploadIDMarker = q.encode(cp), ""
		p.lastPrefix = cp
		return true
	}

	p.res.Uploads = append(p.res.Uploads, s3api.Upload{
		Key:          q.encode(u.Key),
		UploadID:     u.UploadID,
		StorageClass: storageClass,
		Initiated:    s3api.Timestamp(u.Initiated),
	})
	p.res.NextKeyMarker, p.res.NextUploadIDMarker = q.encode(u.Key), u.UploadID
	return true
}

// beforeMarker reports whether u is outside the prefix or at or before the
// key-marker and upload-id-marker the listing resumes after.
func (p *uploadsPage) beforeMarker(u contracts.UploadInfo) bool {
	q := p.query
	if !strings.HasPrefix(u.Key, q.prefix) || u.Key < q.keyMarker {
		return true
	}
	if u.Key != q.keyMarker || (q.idMarker != "" && p.pastIDMarker) {
		return false
	}
	p.pastIDMarker = p.pastIDMarker || u.UploadID == q.idMarker
	return true
}

// commonPrefix returns the prefix key rolls up into, or "" when key is listed
// on its own.
func commonPrefix(key, prefix, delimiter string) string {
	if delimiter == "" {
		return ""
	}
	i := strings.Index(key[len(prefix):], delimiter)
	if i < 0 {
		return ""
	}
	return key[:len(prefix)+i+len(delimiter)]
}
