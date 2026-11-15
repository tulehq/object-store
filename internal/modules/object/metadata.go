package object

import (
	"net/http"
	"strings"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
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
// headers, its encryption headers (encryptionOf), its user metadata, and its
// content type, ETag, modification time and version ID (setVersionHeader).
func setObjectHeaders(w http.ResponseWriter, info *contracts.ObjectInfo) {
	h := w.Header()
	for name, v := range info.Headers {
		h.Set(name, v)
	}
	setEncryptionHeaders(w, encryptionOf(info.Headers))
	for k, v := range info.Metadata {
		h.Set(metaHeaderPrefix+k, v)
	}

	h.Set("Content-Type", info.ContentType)
	h.Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")
	if info.ETag != "" {
		h.Set("ETag", `"`+info.ETag+`"`)
	}
	setVersionHeader(w, info.VersionID)
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

// putInput is putInput(r) with ws's encryption headers among its headers,
// so they are stored with the object and replayed on reads, and ws's ACL
// and tags.
func (ws *writeSettings) putInput(r *http.Request) contracts.PutObjectInput {
	in := putInput(r)
	for k, v := range ws.encryption {
		in.Headers[k] = v
	}
	in.ACL = ws.acl
	in.Tags = ws.tags
	return in
}

// encryptionOf returns the encryption headers among stored, the headers an
// object or upload was stored with. One stored with neither form, by an
// earlier version, reports SSE-S3, as S3 reports every object it stored
// since "Starting January 5, 2023, all new object uploads to Amazon S3 are
// automatically encrypted" ("Setting default server-side encryption
// behavior for Amazon S3 buckets", S3 User Guide); that is a choice for
// this server's older objects.
func encryptionOf(stored map[string]string) map[string]string {
	headers := map[string]string{}
	for _, k := range s3api.EncryptionHeaderNames {
		if v := stored[k]; v != "" {
			headers[k] = v
		}
	}
	if headers[s3api.HeaderSSE] == "" && headers[s3api.HeaderSSECAlgorithm] == "" {
		headers[s3api.HeaderSSE] = s3api.SSEAES256
	}
	return headers
}
