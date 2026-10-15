package s3api

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// maxChunkHeader bounds a chunk header line: a hex size plus an optional
// ";chunk-signature=<64 hex>" extension.
const maxChunkHeader = 4096

// IsAWSChunked reports whether the request body uses the aws-chunked encoding
// that SDKs use for streaming SigV4 uploads and trailing checksums.
func IsAWSChunked(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return true
	}
	for _, enc := range strings.Split(r.Header.Get("Content-Encoding"), ",") {
		if strings.TrimSpace(enc) == "aws-chunked" {
			return true
		}
	}
	return false
}

// NewChunkedReader decodes an aws-chunked body into the object's bytes. Chunk
// signatures and trailing checksums are not verified. decodedLen is the
// x-amz-decoded-content-length header, or -1 when absent; when known, a body
// that decodes to a different length fails with IncompleteBody so the upload
// is not committed.
func NewChunkedReader(body io.Reader, decodedLen int64) io.Reader {
	return &chunkedReader{r: bufio.NewReader(body), want: decodedLen}
}

type chunkedReader struct {
	r       *bufio.Reader
	err     error
	remain  int64 // bytes left in the current chunk
	total   int64
	want    int64
	started bool
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}

	if c.remain == 0 {
		if c.err = c.nextChunk(); c.err != nil {
			return 0, c.err
		}
	}

	if int64(len(p)) > c.remain {
		p = p[:c.remain]
	}
	n, err := c.r.Read(p)
	c.remain -= int64(n)
	c.total += int64(n)
	if err != nil {
		if err == io.EOF { //nolint:errorlint // bufio returns io.EOF unwrapped
			err = io.ErrUnexpectedEOF
		}
		c.err = err
	}
	return n, err
}

// nextChunk consumes the CRLF ending the previous chunk and the next chunk
// header. On the final zero-length chunk it checks the decoded length and
// returns io.EOF, leaving any trailers unread.
func (c *chunkedReader) nextChunk() error {
	if c.started {
		if err := c.expectCRLF(); err != nil {
			return err
		}
	}
	c.started = true

	line, err := c.readLine()
	if err != nil {
		return err
	}

	sizeHex, _, _ := strings.Cut(line, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
	if err != nil || size < 0 {
		return ErrIncompleteBody(fmt.Sprintf("invalid aws-chunked chunk header %q", line))
	}

	if size == 0 {
		if c.want >= 0 && c.total != c.want {
			return ErrIncompleteBody(fmt.Sprintf(
				"decoded %d bytes, x-amz-decoded-content-length is %d", c.total, c.want))
		}
		return io.EOF
	}

	c.remain = size
	return nil
}

func (c *chunkedReader) readLine() (string, error) {
	var sb strings.Builder
	for {
		b, err := c.r.ReadByte()
		if err != nil {
			return "", ErrIncompleteBody("truncated aws-chunked body")
		}
		if b == '\n' {
			return strings.TrimSuffix(sb.String(), "\r"), nil
		}
		if sb.Len() >= maxChunkHeader {
			return "", ErrIncompleteBody("aws-chunked chunk header too long")
		}
		sb.WriteByte(b)
	}
}

func (c *chunkedReader) expectCRLF() error {
	line, err := c.readLine()
	if err != nil {
		return err
	}
	if line != "" {
		return ErrIncompleteBody("aws-chunked chunk data longer than its declared size")
	}
	return nil
}
