package httpx

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
)

var requestIDFormat = regexp.MustCompile(`^[0-9A-F]{16}$`)

// serve runs one request through RequestID and returns the response's
// header and the IDs the handler saw on its context.
func serve(t *testing.T, req *http.Request) (http.Header, string, string) {
	t.Helper()
	var ctxID, ctxHost string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxID, ctxHost = middleware.GetReqID(r.Context()), HostID(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result().Header, ctxID, ctxHost //nolint:bodyclose // a recorder's body
}

func TestRequestIDAnswersThePairTheHandlerSees(t *testing.T) {
	h, ctxID, ctxHost := serve(t, httptest.NewRequest(http.MethodGet, "/b/k", nil))

	id, host := h.Get(RequestIDHeader), h.Get(HostIDHeader)
	if !requestIDFormat.MatchString(id) {
		t.Errorf("request ID %q is not 16 uppercase hex characters", id)
	}
	raw, err := base64.StdEncoding.DecodeString(host)
	if err != nil || len(raw) != hostIDBytes || len(host) != 64 {
		t.Errorf(
			"host ID %q: %d bytes, %v; want base64 of %d bytes",
			host,
			len(raw),
			err,
			hostIDBytes,
		)
	}
	if ctxID != id || ctxHost != host {
		t.Errorf("context (%q, %q), response (%q, %q)", ctxID, ctxHost, id, host)
	}
}

func TestRequestIDIsNewForEachRequest(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		h, _, _ := serve(t, httptest.NewRequest(http.MethodGet, "/", nil))
		for _, v := range []string{h.Get(RequestIDHeader), h.Get(HostIDHeader)} {
			if seen[v] {
				t.Fatalf("%q repeated", v)
			}
			seen[v] = true
		}
	}
}

func TestRequestIDIgnoresTheClientsRequestID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "from-the-client")
	h, ctxID, _ := serve(t, req)
	if ctxID == "from-the-client" || h.Get(RequestIDHeader) != ctxID {
		t.Errorf("context %q, header %q", ctxID, h.Get(RequestIDHeader))
	}
}

func TestHostIDOutsideARequest(t *testing.T) {
	if got := HostID(t.Context()); got != "" {
		t.Errorf("HostID = %q", got)
	}
}
