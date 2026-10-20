package app

import (
	"context"
	"testing"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/config/providers"
)

// startApp starts the application in-process on a fresh storage root, without
// listening, and returns it.
func startApp(t *testing.T) *gas.App {
	t.Helper()

	t.Setenv("TULE_GasEnv", "testing")
	t.Setenv("TULE_Storage_RootDir", t.TempDir())
	t.Setenv("TULE_Auth_AccessKeyID", "test")
	t.Setenv("TULE_Auth_SecretAccessKey", "test")

	cfg, err := LoadConfigFrom(providers.NewEnvProvider(
		providers.WithEnvSeparator(envVarSeparator),
		providers.WithEnvPrefix(envVarPrefix),
	))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	a, err := New(cfg)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	if err = a.Start(context.Background()); err != nil {
		t.Fatalf("start app: %v", err)
	}
	t.Cleanup(func() {
		if sErr := a.Shutdown(context.Background()); sErr != nil {
			t.Errorf("shutdown: %v", sErr)
		}
	})
	return a
}

// TestServerTimeoutsAllowStreamingObjects covers the timeouts of the
// *http.Server the binary serves from: an upload or download of an object
// streams for as long as the client needs, so neither reading a request nor
// writing a response may be cut off by a fixed deadline. The e2e suite
// asserts what these do to a connection; this pins the values the deployment
// runs with.
func TestServerTimeoutsAllowStreamingObjects(t *testing.T) {
	srv := startApp(t).Server()

	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want no limit", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want no limit", srv.WriteTimeout)
	}
	// An object store that serves no request for two minutes is idle, not
	// slow, and gas's own default reaps those connections.
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want a limit", srv.IdleTimeout)
	}
}
