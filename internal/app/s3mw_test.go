package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"tule.dev/object-store/internal/modules/health"
	"tule.dev/object-store/internal/sigv4"
)

// TestVerifySignatureExemptsOnlyTheLiteralProbePaths covers what the probe
// exemption does and does not cover. A path that only decodes to a probe path
// is not one -- chi routes on the escaped path, so such a request reaches the
// bucket and object routes -- and neither is a write to a probe path, which
// the probe routes do not serve.
func TestVerifySignatureExemptsOnlyTheLiteralProbePaths(t *testing.T) {
	v := sigv4.NewVerifier(sigv4.Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret"})
	served := false
	h := verifySignature(v, "localhost")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		served = true
	}))

	tests := []struct {
		name, method, path string
		wantServed         bool
	}{
		{name: "health probe", method: http.MethodGet, path: health.HealthPath, wantServed: true},
		{name: "ready probe", method: http.MethodGet, path: health.ReadyPath, wantServed: true},
		{name: "head probe", method: http.MethodHead, path: health.HealthPath, wantServed: true},
		{name: "escaped health probe", method: http.MethodGet, path: "/%5Fhealth"},
		{name: "delete on a probe path", method: http.MethodDelete, path: health.HealthPath},
		{name: "put on a probe path", method: http.MethodPut, path: health.ReadyPath},
		{name: "post on a probe path", method: http.MethodPost, path: health.HealthPath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			served = false
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))

			if served != tt.wantServed {
				t.Fatalf("handler served = %v, want %v", served, tt.wantServed)
			}
			if !tt.wantServed && w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
			}
		})
	}
}

// TestSecurityHeadersOnRejectedRequests covers the responses the signature
// middleware writes itself. Security headers set inside the chain never reach
// them, so the response that any web page on the machine can provoke would be
// the one response without them.
func TestSecurityHeadersOnRejectedRequests(t *testing.T) {
	h := startApp(t).Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/bucket/key", nil))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	for _, name := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if w.Header().Get(name) == "" {
			t.Errorf("%s is missing from the rejected request's response", name)
		}
	}
}
