package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/log"
	"github.com/rs/zerolog"
)

// bufferLogger returns a gas.Logger writing JSON into buf.
func bufferLogger(t *testing.T, buf *bytes.Buffer) gas.Logger {
	t.Helper()
	zl := zerolog.New(buf)
	logger, err := log.NewZeroLogLogger(log.WithZeroLogInstance(&zl))()
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	return logger
}

// TestWithRequestLoggerServesWithoutALogger covers a request whose scope has
// no RequestLogger registered, so the resolve fails: the request must still
// reach the handler rather than be answered with an empty 200, and the failure
// must be reported on the application logger.
func TestWithRequestLoggerServesWithoutALogger(t *testing.T) {
	served := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusTeapot)
	})

	r := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	scope := gas.NewServiceContainer().NewScope(r.Context())
	r = r.WithContext(gas.WithRequestScope(r.Context(), scope))

	var logged bytes.Buffer
	w := httptest.NewRecorder()
	withRequestLogger(bufferLogger(t, &logged))(next).ServeHTTP(w, r)

	if !served {
		t.Fatal("handler was not called")
	}
	if w.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusTeapot)
	}
	if !strings.Contains(logged.String(), "failed to resolve the request logger") {
		t.Fatalf("the resolve failure was not logged: %q", logged.String())
	}
}

// TestProdLoggerWritesJSON covers the production-like logger: its output is
// collected by machines, so each line is a JSON object, and it logs at warn
// level.
func TestProdLoggerWritesJSON(t *testing.T) {
	var out bytes.Buffer
	logger, err := newProdLogger(&out)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	logger.Info("quiet").Send()
	if out.Len() != 0 {
		t.Fatalf("logged below warn level: %q", out.String())
	}

	logger.Warn("loud").Send()
	var line map[string]any
	if jErr := json.Unmarshal(out.Bytes(), &line); jErr != nil {
		t.Fatalf("output is not JSON: %q: %v", out.String(), jErr)
	}
	if line["message"] != "loud" {
		t.Fatalf("message = %v, want loud", line["message"])
	}
}
