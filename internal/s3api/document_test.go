package s3api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func documentRequest(body, payloadHash string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/b?delete", strings.NewReader(body))
	if payloadHash != "" {
		r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	return r
}

func TestReadDocumentVerifiesASignedDocument(t *testing.T) {
	refused := errors.New("refused")
	r := documentRequest("<Delete/>", strings.Repeat("ab", 32))
	_, err := ReadDocument(r, 100, func(got *http.Request, maxBytes int64) ([]byte, error) {
		if got != r || maxBytes != 100 {
			t.Errorf("readVerified(%p, %d), want (%p, 100)", got, maxBytes, r)
		}
		return nil, refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the verified read's", err)
	}
}

func TestReadDocumentReadsAnUnsignedDocumentAsSent(t *testing.T) {
	notCalled := func(*http.Request, int64) ([]byte, error) {
		t.Fatal("an unsigned document was read as a signed one")
		return nil, nil
	}
	for _, tc := range []struct{ name, hash, body string }{
		{"unsigned payload", "UNSIGNED-PAYLOAD", "<Delete/>"},
		{"no hash, as a presigned url sends it", "", "<Delete/>"},
		{"streaming", "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "9\r\n<Delete/>\r\n0\r\n\r\n"},
	} {
		doc, err := ReadDocument(documentRequest(tc.body, tc.hash), 100, notCalled)
		if err != nil || string(doc) != "<Delete/>" {
			t.Errorf("%s: ReadDocument = %q, %v, want %q", tc.name, doc, err, "<Delete/>")
		}
	}
}

func TestReadDocumentBoundsAnUnsignedDocument(t *testing.T) {
	_, err := ReadDocument(documentRequest(strings.Repeat("x", 11), "UNSIGNED-PAYLOAD"), 10,
		nil)
	var s3Err *Error
	if !errors.As(err, &s3Err) || s3Err.Code != "MaxMessageLengthExceeded" {
		t.Fatalf("err = %v, want MaxMessageLengthExceeded", err)
	}
}

func TestReadDocumentRefusesSignedChunks(t *testing.T) {
	notCalled := func(*http.Request, int64) ([]byte, error) {
		t.Fatal("a signed-chunk document was read as a verified one")
		return nil, nil
	}
	for _, hash := range []string{
		"STREAMING-AWS4-HMAC-SHA256-PAYLOAD",
		"STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER",
		"STREAMING-AWS4-ECDSA-P256-SHA256-PAYLOAD",
		"STREAMING-AWS4-ECDSA-P256-SHA256-PAYLOAD-TRAILER",
	} {
		r := documentRequest("9;chunk-signature="+strings.Repeat("a", 64)+
			"\r\n<Delete/>\r\n0\r\n\r\n", hash)
		_, err := ReadDocument(r, 100, notCalled)
		var s3Err *Error
		if !errors.As(err, &s3Err) || s3Err.Code != "InvalidRequest" ||
			s3Err.Status != http.StatusBadRequest {
			t.Errorf("%s: err = %v, want 400 InvalidRequest", hash, err)
		}
	}
}
