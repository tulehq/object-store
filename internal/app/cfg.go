package app

import (
	"errors"
	"fmt"
	"time"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/config"

	"tule.dev/object-store/internal/services/storage/dirimpl"
)

type logSettings struct {
	ShippingEndpoint string `validate:"omitempty,https_url"`
	ShippingAPIKey   string
}

// serverSettings configures the HTTP server. Domain enables
// virtual-hosted-style addressing: a request to <bucket>.<Domain> addresses
// that bucket.
type serverSettings struct {
	Domain         string
	RequestTimeout time.Duration `validate:"required"`
}

// hstsSettings configures the Strict-Transport-Security header. It is only
// sent when MaxAge is set, so plain-HTTP deployments can leave it out.
type hstsSettings struct {
	MaxAge time.Duration
}

// authSettings holds the one access key the server accepts. Every request
// must be SigV4-signed with it, so both fields are required.
type authSettings struct {
	AccessKeyID     string
	SecretAccessKey string
}

// errNoCredentials is returned when the access key is not configured.
var errNoCredentials = errors.New("config: TULE_Auth_AccessKeyID and " +
	"TULE_Auth_SecretAccessKey must be set; every request is verified against them")

// Config is the bound application configuration. It keeps the store it was
// bound from, which the app registers as the gas.ConfigProvider so services
// can read settings Config itself does not model.
type Config struct {
	Log     *logSettings
	Server  *serverSettings `validate:"required"`
	HSTS    *hstsSettings
	Auth    *authSettings
	Storage *dirimpl.Settings `validate:"required"`

	store gas.ConfigProvider
	Env   config.Environment
}

const (
	envVarSeparator = "_"
	envVarPrefix    = "TULE_"
)

// LoadConfig loads configuration from a .env file and then the process
// environment, and in production-like environments layers AWS Secrets Manager
// on top. It fails if a production-like environment is missing its Secrets
// Manager settings.
func LoadConfig() (*Config, error) {
	return LoadConfigFrom(
		config.NewDotEnvProvider(
			config.WithDotEnvSeparator(envVarSeparator),
			config.WithDotEnvFileNotFoundPanic(false),
		),
		config.NewEnvProvider(
			config.WithEnvSeparator(envVarSeparator),
			config.WithEnvPrefix(envVarPrefix),
		),
	)
}

// LoadConfigFrom loads configuration from the given providers in order, then
// applies the same Secrets Manager layering and binding as LoadConfig.
func LoadConfigFrom(provs ...config.Provider) (*Config, error) {
	env := config.NewEnvExtension()

	opts := make([]config.Option, 0, len(provs)+1)
	opts = append(opts, config.WithExtension(env))

	for _, p := range provs {
		opts = append(opts, config.WithProvider(p))
	}

	store := config.New(opts...)

	if err := store.Load(); err != nil {
		return nil, fmt.Errorf("config: load: %w", err)
	}

	return bindConfig(store, env.Current())
}

func bindConfig(store gas.ConfigProvider, env config.Environment) (*Config, error) {
	if err := applyDefaults(store, env); err != nil {
		return nil, err
	}

	cfg := Config{Env: env, store: store}
	if err := store.Bind(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if cfg.Auth == nil || cfg.Auth.AccessKeyID == "" || cfg.Auth.SecretAccessKey == "" {
		return nil, errNoCredentials
	}

	return &cfg, nil
}

func applyDefaults(store gas.ConfigProvider, _ config.Environment) error {
	defaults := []struct {
		value any
		key   string
	}{
		{9000, "Server.Port"},
		{"30s", "Server.RequestTimeout"},
		{"./data", "Storage.RootDir"},
		{"localhost", "Server.Domain"},
	}
	for _, d := range defaults {
		if err := store.SetDefault(d.key, d.value); err != nil {
			return fmt.Errorf("config: default %s: %w", d.key, err)
		}
	}
	return nil
}
