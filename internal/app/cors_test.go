package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCORSAllowsHeadRequests guards the browser support the README promises
// for HeadObject and HeadBucket. The CORS middleware answers only for the
// methods it is configured with, so dropping one from AllowedMethods leaves
// cross-origin responses for it unreadable, preflight and actual alike.
func TestCORSAllowsHeadRequests(t *testing.T) {
	h := startApp(t).Handler()

	preflight := httptest.NewRequest(http.MethodOptions, "/bucket/key", nil)
	preflight.Header.Set("Origin", "https://app.example")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodHead)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, preflight)

	if allowed := w.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(
		strings.ToUpper(allowed), http.MethodHead,
	) {
		t.Errorf("preflight Access-Control-Allow-Methods = %q, want it to allow HEAD", allowed)
	}

	actual := httptest.NewRequest(http.MethodHead, "/bucket/key", nil)
	actual.Header.Set("Origin", "https://app.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, actual)

	if origin := w.Header().Get("Access-Control-Allow-Origin"); origin == "" {
		t.Error("HEAD response carries no Access-Control-Allow-Origin")
	}
	// The middleware canonicalizes the names; browsers match them case-insensitively.
	exposed := w.Header().Get("Access-Control-Expose-Headers")
	if !strings.Contains(strings.ToLower(exposed), "etag") {
		t.Errorf("Access-Control-Expose-Headers = %q, want it to expose ETag", exposed)
	}
}
