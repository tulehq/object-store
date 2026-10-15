package dirimpl

import (
	"crypto/md5" //nolint:gosec // S3 ETags are MD5 digests, not a security control
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"strconv"
	"strings"
)

// etagMetaKey is the metadata key the object's ETag is stored under, next to
// the user metadata in the backend's sidecar file.
const etagMetaKey = "__TULE_ETAG__"

// etagReader hashes everything read through it and records the hex digest in
// meta once the underlying reader reports EOF.
//
// The dir backend reads the body to EOF before it copies the metadata map it
// was given, which is what lets the digest land in the sidecar written for
// this same upload. TestUploadRecordsETag guards that ordering.
type etagReader struct {
	r    io.Reader
	h    hash.Hash
	meta map[string]string
}

func newETagReader(r io.Reader, userMeta map[string]string) *etagReader {
	meta := make(map[string]string, len(userMeta)+1)
	maps.Copy(meta, userMeta)
	return &etagReader{r: r, h: md5.New(), meta: meta} //nolint:gosec // see import
}

func (e *etagReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.h.Write(p[:n])
	if errors.Is(err, io.EOF) {
		e.meta[etagMetaKey] = hex.EncodeToString(e.h.Sum(nil))
	}
	return n, err //nolint:wrapcheck // io.Reader contract: EOF must pass through unwrapped
}

// splitETag removes the ETag entry from meta and returns it.
func splitETag(meta map[string]string) string {
	etag := meta[etagMetaKey]
	delete(meta, etagMetaKey)
	return etag
}

// headerMetaPrefix marks stored system headers in the backend metadata map.
// User metadata keys are lowercase, so they cannot collide with it.
const headerMetaPrefix = "__TULE_HEADER__"

// joinMetadata flattens user metadata and system headers into one backend
// metadata map.
func joinMetadata(meta, headers map[string]string) map[string]string {
	out := make(map[string]string, len(meta)+len(headers))
	maps.Copy(out, meta)
	for k, v := range headers {
		out[headerMetaPrefix+k] = v
	}
	return out
}

// splitHeaders removes the stored system headers from meta and returns them.
func splitHeaders(meta map[string]string) map[string]string {
	headers := make(map[string]string)
	for k, v := range meta {
		if name, ok := strings.CutPrefix(k, headerMetaPrefix); ok {
			headers[name] = v
			delete(meta, k)
		}
	}
	return headers
}

// partsMetaKey records the part sizes of an object assembled by a multipart
// upload, as comma-separated decimal byte counts.
const partsMetaKey = "__TULE_PARTS__"

func joinParts(meta map[string]string, sizes []int64) {
	strs := make([]string, len(sizes))
	for i, n := range sizes {
		strs[i] = strconv.FormatInt(n, 10)
	}
	meta[partsMetaKey] = strings.Join(strs, ",")
}

// splitParts removes the part sizes entry from meta and returns the sizes.
func splitParts(meta map[string]string) ([]int64, error) {
	v, ok := meta[partsMetaKey]
	delete(meta, partsMetaKey)
	if !ok {
		return nil, nil
	}

	var sizes []int64
	for f := range strings.SplitSeq(v, ",") {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid part sizes %q: %w", v, err)
		}
		sizes = append(sizes, n)
	}
	return sizes, nil
}
