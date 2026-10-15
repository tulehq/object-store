package app

import (
	"fmt"
	"time"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/config"
	"github.com/gasmod/gas/config/extensions/gasenv"
	"github.com/gasmod/gas/config/providers"

	"tule.dev/object-store/internal/services/storage/dirimpl"
)

type logSettings struct {
	ShippingEndpoint string `validate:"omitempty,https_url"`
	ShippingAPIKey   string
}

type serverSettings struct {
	RequestTimeout time.Duration `validate:"required"`
}

// hstsSettings configures the Strict-Transport-Security header. It is only
// sent when MaxAge is set, so plain-HTTP deployments can leave it out.
type hstsSettings struct {
	MaxAge time.Duration
}

// s3Settings configures the S3 endpoint. Domain enables virtual-hosted-style
// addressing: a request to <bucket>.<Domain> addresses that bucket. Setting
// AccessKeyID and SecretAccessKey turns on SigV4 request verification;
// without them any credentials are accepted.
type s3Settings struct {
	Domain          string
	AccessKeyID     string `validate:"required_with=SecretAccessKey"`
	SecretAccessKey string `validate:"required_with=AccessKeyID"`
}

// Config is the bound application configuration. It keeps the store it was
// bound from, which the app registers as the gas.ConfigProvider so services
// can read settings Config itself does not model.
type Config struct {
	Log     *logSettings
	Server  *serverSettings `validate:"required"`
	HSTS    *hstsSettings
	S3      *s3Settings       `validate:"required"`
	Storage *dirimpl.Settings `validate:"required"`

	store gas.ConfigProvider
	Env   gasenv.Environment
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
		providers.NewDotEnvProvider(
			providers.WithDotEnvSeparator(envVarSeparator),
			providers.WithDotEnvFileNotFoundPanic(false),
		),
		providers.NewEnvProvider(
			providers.WithEnvSeparator(envVarSeparator),
			providers.WithEnvPrefix(envVarPrefix),
		),
	)
}

// LoadConfigFrom loads configuration from the given providers in order, then
// applies the same Secrets Manager layering and binding as LoadConfig.
func LoadConfigFrom(provs ...providers.Provider) (*Config, error) {
	env := gasenv.NewExtension()

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

func bindConfig(store gas.ConfigProvider, env gasenv.Environment) (*Config, error) {
	applyDefaults(store, env)

	cfg := Config{Env: env, store: store}
	if err := store.Bind(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	return &cfg, nil
}

func applyDefaults(store gas.ConfigProvider, _ gasenv.Environment) {
	store.SetDefault("Server.Port", 9000)
	store.SetDefault("Server.RequestTimeout", "30s")
	store.SetDefault("Storage.RootDir", "./data")
	store.SetDefault("S3.Domain", "localhost")
}
