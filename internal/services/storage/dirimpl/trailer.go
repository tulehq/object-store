package dirimpl

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// The backend's own metadata keeps S3's limits, 2 KB of printable ASCII,
// which the ETag, system headers and part sizes stored next to the user
// metadata do not fit. So what this package stores with an object is kept
// in a trailer after the object's bytes, in the same backend object, and is
// written and read in the same backend call as them:
//
//	<object bytes> <trailer JSON> <JSON length, 8 bytes big-endian> <trailerMagic>
//
// The backend object's own content type and metadata are left unset.

// trailerMagic ends every backend object this package writes.
const trailerMagic = "TULEOBJ1"

// trailerFooterSize is the length of the JSON length and trailerMagic.
const trailerFooterSize = 8 + len(trailerMagic)

// sniffSize is how many leading bytes of a body a missing content type is
// detected from, as http.DetectContentType reads at most.
const sniffSize = 512

// errBadTrailer is returned for a backend object that does not end in a
// trailer this package wrote.
var errBadTrailer = errors.New("object has no valid trailer")

// trailer is what is stored after an object's bytes. Meta is the flattened
// metadata map described in etag.go.
type trailer struct {
	LastModified time.Time         `json:"lastModified"`
	Meta         map[string]string `json:"meta"`
	ContentType  string            `json:"contentType"`
}

// withTrailer returns a reader of body followed by the trailer of tr. The
// trailer is encoded once body is exhausted, so it records what reading
// body filled in, such as the ETag an etagReader adds to tr.Meta, and the
// time the body was read. An empty tr.ContentType is detected from the
// first bytes of body.
func withTrailer(body io.Reader, tr *trailer) io.Reader {
	head := &headReader{r: body}
	return io.MultiReader(head, &lazyReader{build: func() ([]byte, error) {
		if tr.ContentType == "" {
			tr.ContentType = http.DetectContentType(head.head)
		}
		tr.LastModified = time.Now()
		return encodeTrailer(tr)
	}})
}

func encodeTrailer(tr *trailer) ([]byte, error) {
	b, err := json.Marshal(tr)
	if err != nil {
		return nil, fmt.Errorf("encode trailer: %w", err)
	}
	b = binary.BigEndian.AppendUint64(b, uint64(len(b)))
	return append(b, trailerMagic...), nil
}

// readTrailer decodes the trailer at the end of the size bytes of r, and
// returns it with the length of the object bytes before it. It leaves r at
// the start of the object bytes.
func readTrailer(r io.ReadSeeker, size int64) (*trailer, int64, error) {
	if size < int64(trailerFooterSize) {
		return nil, 0, errBadTrailer
	}

	footer := make([]byte, trailerFooterSize)
	if err := readAt(r, footer, size-int64(trailerFooterSize)); err != nil {
		return nil, 0, err
	}
	if string(footer[8:]) != trailerMagic {
		return nil, 0, errBadTrailer
	}

	n := binary.BigEndian.Uint64(footer[:8])
	if n > uint64(size-int64(trailerFooterSize)) {
		return nil, 0, errBadTrailer
	}
	// n is at most size - trailerFooterSize, checked above.
	bodyLen := size - int64(trailerFooterSize) - int64(n) //nolint:gosec // see above

	b := make([]byte, n)
	if err := readAt(r, b, bodyLen); err != nil {
		return nil, 0, err
	}

	var tr trailer
	if err := json.Unmarshal(b, &tr); err != nil {
		return nil, 0, fmt.Errorf("%w: %w", errBadTrailer, err)
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, 0, fmt.Errorf("read trailer: %w", err)
	}
	return &tr, bodyLen, nil
}

// readAt fills b from r at offset off. A short read is an error.
func readAt(r io.ReadSeeker, b []byte, off int64) error {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return fmt.Errorf("read trailer: %w", err)
	}
	if _, err := io.ReadFull(r, b); err != nil {
		return fmt.Errorf("read trailer: %w", err)
	}
	return nil
}

// objectBody reads the first size bytes of a backend object, its object
// bytes without the trailer, as an io.ReadSeeker. Closing it closes the
// backend object. Like the backend body, it is for one goroutine at a time.
type objectBody struct {
	rs   io.ReadSeekCloser
	size int64
	pos  int64
}

func (b *objectBody) Read(p []byte) (int, error) {
	if b.pos >= b.size {
		return 0, io.EOF
	}
	if rest := b.size - b.pos; int64(len(p)) > rest {
		p = p[:rest]
	}

	n, err := b.rs.Read(p)
	b.pos += int64(n)
	if errors.Is(err, io.EOF) && b.pos < b.size {
		// The backend object ended before its trailer: it was truncated.
		return n, io.ErrUnexpectedEOF
	}
	return n, err //nolint:wrapcheck // io.Reader contract: EOF must pass through unwrapped
}

func (b *objectBody) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = b.pos + offset
	case io.SeekEnd:
		abs = b.size + offset
	default:
		return 0, errors.New("objectBody.Seek: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("objectBody.Seek: negative position")
	}

	if _, err := b.rs.Seek(abs, io.SeekStart); err != nil {
		return 0, err //nolint:wrapcheck // the backend body's own Seek
	}
	b.pos = abs
	return abs, nil
}

func (b *objectBody) Close() error {
	return b.rs.Close() //nolint:wrapcheck // the backend body's own Close
}

// headReader passes r through and keeps the first sniffSize bytes read.
type headReader struct {
	r    io.Reader
	head []byte
}

func (h *headReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if rest := sniffSize - len(h.head); rest > 0 {
		h.head = append(h.head, p[:min(n, rest)]...)
	}
	return n, err //nolint:wrapcheck // io.Reader contract: EOF must pass through unwrapped
}

// lazyReader yields the bytes build returns, calling it on the first Read.
type lazyReader struct {
	build func() ([]byte, error)
	r     *bytes.Reader
}

func (l *lazyReader) Read(p []byte) (int, error) {
	if l.r == nil {
		b, err := l.build()
		if err != nil {
			return 0, err
		}
		l.r = bytes.NewReader(b)
	}
	return l.r.Read(p) //nolint:wrapcheck // io.Reader contract: EOF must pass through unwrapped
}
