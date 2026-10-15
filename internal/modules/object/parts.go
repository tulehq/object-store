package object

import (
	"fmt"
	"net/http"
	"strconv"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// partSpan locates part n of an object. For an object that was not uploaded
// in parts, part 1 is the whole object and ok is false, meaning the request
// should be served as a plain GET or HEAD.
type partSpan struct {
	start, length int64
	count         int
	ok            bool
}

func locatePart(info *contracts.ObjectInfo, v string) (partSpan, error) {
	n, err := parsePartNumber(v)
	if err != nil {
		return partSpan{}, err
	}

	if len(info.PartSizes) == 0 {
		if n != 1 {
			return partSpan{}, s3api.ErrInvalidPartNumber()
		}
		return partSpan{}, nil
	}

	if n > len(info.PartSizes) {
		return partSpan{}, s3api.ErrInvalidPartNumber()
	}

	var start int64
	for _, size := range info.PartSizes[:n-1] {
		start += size
	}

	return partSpan{
		start:  start,
		length: info.PartSizes[n-1],
		count:  len(info.PartSizes),
		ok:     true,
	}, nil
}

// contentRange is the Content-Range header value for the span, or "" for an
// empty part, which no byte range can express.
func (p partSpan) contentRange(total int64) string {
	if p.length == 0 {
		return ""
	}
	return fmt.Sprintf("bytes %d-%d/%d", p.start, p.start+p.length-1, total)
}

// rangeHeader is the Range request header selecting the span.
func (p partSpan) rangeHeader() string {
	return fmt.Sprintf("bytes=%d-%d", p.start, p.start+p.length-1)
}

// partRequest resolves a GET's ?partNumber into the request to serve: r
// itself for a plain object, or a copy of r restricted to the part's bytes.
func partRequest(
	w http.ResponseWriter,
	r *http.Request,
	info *contracts.ObjectInfo,
) (*http.Request, error) {
	v := r.URL.Query().Get("partNumber")
	if v == "" {
		return r, nil
	}
	if r.Header.Get("Range") != "" {
		return nil, s3api.ErrInvalidRequest(
			"Cannot specify both Range header and partNumber query parameter")
	}

	span, err := locatePart(info, v)
	if err != nil || !span.ok {
		return r, err
	}

	w.Header().Set("X-Amz-Mp-Parts-Count", strconv.Itoa(span.count))
	if span.length == 0 {
		return r, s3api.ErrInvalidPartNumber()
	}

	pr := r.Clone(r.Context())
	pr.Header.Set("Range", span.rangeHeader())
	return pr, nil
}
