package s3api

import (
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// ReadDocument reads a request document an operation acts on, such as a
// DeleteObjects or CompleteMultipartUpload body, of at most maxBytes.
//
// A document sent under a hex X-Amz-Content-Sha256 is read with
// readVerified, which returns it only once it matches that hash, since a
// decoder stops at the end of its value and would act on a forged body
// padded to the signed one's length. A document the client chose not to
// sign is read as sent, decoding aws-chunked, as S3 reads it: one sent as
// UNSIGNED-PAYLOAD or STREAMING-UNSIGNED-PAYLOAD-TRAILER, or by a presigned
// URL that declares no hash and so sends UNSIGNED-PAYLOAD. (The verifier
// refuses an Authorization header request that declares none.)
//
// Any other hash, such as STREAMING-AWS4-HMAC-SHA256-PAYLOAD, is refused
// with InvalidRequest before the body is read: the client signed each
// chunk, and chunk signatures are not verified, so the document would be
// acted on while the client believes it covered.
func ReadDocument(
	r *http.Request,
	maxBytes int64,
	readVerified func(*http.Request, int64) ([]byte, error),
) ([]byte, error) {
	switch hash := r.Header.Get("X-Amz-Content-Sha256"); {
	case isHexSHA256(hash):
		return readVerified(r, maxBytes)
	case hash == "", hash == "UNSIGNED-PAYLOAD", hash == "STREAMING-UNSIGNED-PAYLOAD-TRAILER":
	default:
		return nil, ErrInvalidRequest("The signed streaming chunks of a request document " +
			"are not verified; send it with a hex x-amz-content-sha256 or UNSIGNED-PAYLOAD.")
	}

	body := io.Reader(r.Body)
	if IsAWSChunked(r) {
		decodedLen := int64(-1)
		if v := r.Header.Get("X-Amz-Decoded-Content-Length"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return nil, ErrInvalidArgument("invalid x-amz-decoded-content-length")
			}
			decodedLen = n
		}
		body = NewChunkedReader(r.Body, decodedLen)
	}

	doc, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read request document: %w", err)
	}
	if int64(len(doc)) > maxBytes {
		return nil, ErrMaxMessageLengthExceeded()
	}
	return doc, nil
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
