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
