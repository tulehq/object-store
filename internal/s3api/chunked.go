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

// maxTrailer bounds what may follow the final chunk of an aws-chunked body:
// its trailing headers, such as a checksum and its signature.
const maxTrailer = 64 << 10

// IsAWSChunked reports whether the request body uses the aws-chunked encoding
// that SDKs use for streaming SigV4 uploads and trailing checksums. The
// signed X-Amz-Content-Sha256 decides: a STREAMING-* value declares it.
//
// Content-Encoding is not signed, so it cannot make a body aws-chunked. One
// naming aws-chunked under any other hash fails with InvalidRequest: decoded,
// the body would end at its final chunk, before the hash that covers the
// whole body is checked.
func IsAWSChunked(r *http.Request) (bool, error) {
	if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return true, nil
	}
	for _, enc := range strings.Split(r.Header.Get("Content-Encoding"), ",") {
		if strings.TrimSpace(enc) == "aws-chunked" {
			return false, ErrInvalidRequest("aws-chunked encoding is only accepted with a " +
				"STREAMING-* x-amz-content-sha256.")
		}
	}
	return false, nil
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
// header. On the final zero-length chunk it checks the decoded length, reads
// the rest of the body, its trailers, so the whole body is read before the
// upload ends, and returns io.EOF. A body that runs on past maxTrailer after
// its final chunk fails with IncompleteBody.
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
		return c.drainTrailer()
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

// drainTrailer reads what follows the final chunk, at most maxTrailer bytes,
// and returns io.EOF once the body has ended. A read error of the body,
// such as its payload hash failing, is returned as is.
func (c *chunkedReader) drainTrailer() error {
	n, err := io.Copy(io.Discard, io.LimitReader(c.r, maxTrailer+1))
	if err != nil {
		return err //nolint:wrapcheck // the body's own error, such as a hash mismatch
	}
	if n > maxTrailer {
		return ErrIncompleteBody("aws-chunked body continues past its final chunk")
	}
	return io.EOF
}
