package app

import (
	"errors"
	"testing"

	"github.com/gasmod/gas/config"
)

func loadEnvConfig(t *testing.T, id, secret string) error {
	t.Helper()
	t.Setenv("TULE_GasEnv", "testing")
	t.Setenv("TULE_Auth_AccessKeyID", id)
	t.Setenv("TULE_Auth_SecretAccessKey", secret)

	_, err := LoadConfigFrom(config.NewEnvProvider(
		config.WithEnvSeparator(envVarSeparator),
		config.WithEnvPrefix(envVarPrefix),
	))
	return err
}

func TestLoadConfigRequiresCredentials(t *testing.T) {
	tests := []struct {
		name, id, secret string
		wantErr          bool
	}{
		{name: "both set", id: "AKID", secret: "secret"},
		{name: "neither set", wantErr: true},
		{name: "no secret", id: "AKID", wantErr: true},
		{name: "no access key", secret: "secret", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := loadEnvConfig(t, tt.id, tt.secret)
			if tt.wantErr != errors.Is(err, errNoCredentials) {
				t.Fatalf("got %v, want errNoCredentials: %v", err, tt.wantErr)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadConfigValidatesTheAccountID(t *testing.T) {
	for name, tt := range map[string]struct {
		id      string
		wantErr bool
	}{
		"default":    {},
		"12 digits":  {id: "123456789012"},
		"too short":  {id: "12345", wantErr: true},
		"not digits": {id: "12345678901a", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			if tt.id != "" {
				t.Setenv("TULE_Auth_AccountID", tt.id)
			}
			err := loadEnvConfig(t, "AKID", "secret")
			if (err != nil) != tt.wantErr {
				t.Fatalf("got %v, want an error: %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigValidatesTheIdentityEndpoint(t *testing.T) {
	tests := []struct {
		name, endpoint string
		wantErr        bool
	}{
		{name: "unset"},
		{name: "http", endpoint: "http://localhost:9030"},
		{name: "https", endpoint: "https://identity.example.com/"},
		{name: "no scheme", endpoint: "localhost:9030", wantErr: true},
		{name: "another scheme", endpoint: "ftp://localhost:9030", wantErr: true},
		{name: "no host", endpoint: "http://", wantErr: true},
		{name: "relative", endpoint: "/identity", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.endpoint != "" {
				t.Setenv("TULE_Auth_IdentityEndpoint", tt.endpoint)
			}
			err := loadEnvConfig(t, "AKID", "secret")
			if (err != nil) != tt.wantErr {
				t.Fatalf("got %v, want an error: %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigValidatesTheDestinationEndpoints(t *testing.T) {
	tests := []struct {
		name, endpoint string
		wantErr        bool
	}{
		{name: "unset"},
		{name: "http", endpoint: "http://localhost:9050"},
		{name: "https", endpoint: "https://queues.example.com/"},
		{name: "no scheme", endpoint: "localhost:9050", wantErr: true},
		{name: "another scheme", endpoint: "ftp://localhost:9050", wantErr: true},
		{name: "no host", endpoint: "http://", wantErr: true},
		{name: "relative", endpoint: "/queues", wantErr: true},
	}

	for _, setting := range []string{
		"TULE_Storage_QueuesEndpoint", "TULE_Storage_FunctionsEndpoint",
	} {
		for _, tt := range tests {
			t.Run(setting+"/"+tt.name, func(t *testing.T) {
				if tt.endpoint != "" {
					t.Setenv(setting, tt.endpoint)
				}
				err := loadEnvConfig(t, "AKID", "secret")
				if (err != nil) != tt.wantErr {
					t.Fatalf("got %v, want an error: %v", err, tt.wantErr)
				}
			})
		}
	}
}

func TestLoadConfigReadsTheDestinationEndpoints(t *testing.T) {
	t.Setenv("TULE_GasEnv", "testing")
	t.Setenv("TULE_Auth_AccessKeyID", "AKID")
	t.Setenv("TULE_Auth_SecretAccessKey", "secret")
	t.Setenv("TULE_Storage_QueuesEndpoint", "http://queues:9050")
	t.Setenv("TULE_Storage_FunctionsEndpoint", "http://functions:9060")
	cfg, err := LoadConfigFrom(config.NewEnvProvider(
		config.WithEnvSeparator(envVarSeparator),
		config.WithEnvPrefix(envVarPrefix),
	))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.QueuesEndpoint != "http://queues:9050" ||
		cfg.Storage.FunctionsEndpoint != "http://functions:9060" {
		t.Fatalf("got %q and %q", cfg.Storage.QueuesEndpoint, cfg.Storage.FunctionsEndpoint)
	}
}
