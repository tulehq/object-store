package object

import (
	"net/http"
	"strings"

	"tule.dev/object-store/internal/contracts"
)

const metaHeaderPrefix = "X-Amz-Meta-"

// storedHeaders are the system headers kept with an object and replayed on
// GET and HEAD.
var storedHeaders = []string{
	"Cache-Control",
	"Content-Disposition",
	"Content-Encoding",
	"Content-Language",
	"Expires",
}

// responseOverrides maps GetObject query parameters to the response header
// each one replaces.
var responseOverrides = map[string]string{
	"response-cache-control":       "Cache-Control",
	"response-content-disposition": "Content-Disposition",
	"response-content-encoding":    "Content-Encoding",
	"response-content-language":    "Content-Language",
	"response-content-type":        "Content-Type",
	"response-expires":             "Expires",
}

// putInput reads the content type, x-amz-meta-* user metadata and stored
// system headers from an upload request.
func putInput(r *http.Request) contracts.PutObjectInput {
	in := contracts.PutObjectInput{
		ContentType: r.Header.Get("Content-Type"),
		Metadata:    make(map[string]string),
		Headers:     make(map[string]string),
	}

	for name, values := range r.Header {
		if k, ok := strings.CutPrefix(name, metaHeaderPrefix); ok && k != "" {
			in.Metadata[strings.ToLower(k)] = strings.Join(values, ",")
		}
	}

	for _, name := range storedHeaders {
		v := r.Header.Get(name)
		if name == "Content-Encoding" {
			v = stripAWSChunked(v)
		}
		if v != "" {
			in.Headers[name] = v
		}
	}

	return in
}

// stripAWSChunked removes the aws-chunked transfer coding from a
// Content-Encoding value; it describes the request body, not the object.
func stripAWSChunked(v string) string {
	var kept []string
	for enc := range strings.SplitSeq(v, ",") {
		if enc = strings.TrimSpace(enc); enc != "" && enc != "aws-chunked" {
			kept = append(kept, enc)
		}
	}
	return strings.Join(kept, ", ")
}

// setObjectHeaders writes the headers GET and HEAD share: the object's system
// headers, its user metadata, and its content type, ETag and modification
// time.
func setObjectHeaders(w http.ResponseWriter, info *contracts.ObjectInfo) {
	h := w.Header()
	for name, v := range info.Headers {
		h.Set(name, v)
	}
	for k, v := range info.Metadata {
		h.Set(metaHeaderPrefix+k, v)
	}

	h.Set("Content-Type", info.ContentType)
	h.Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")
	if info.ETag != "" {
		h.Set("ETag", `"`+info.ETag+`"`)
	}
}

// applyResponseOverrides replaces response headers with the response-*
// query parameters a GetObject request carries.
func applyResponseOverrides(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for param, header := range responseOverrides {
		if v := q.Get(param); v != "" {
			w.Header().Set(header, v)
		}
	}
}
