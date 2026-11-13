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
