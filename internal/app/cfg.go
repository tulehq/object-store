package app

import (
	"errors"
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

// serverSettings configures the HTTP server. Domain enables
// virtual-hosted-style addressing: a request to <bucket>.<Domain> addresses
// that bucket. HeaderTimeout bounds the start of a request; zero means no
// bound.
type serverSettings struct {
	Domain        string
	HeaderTimeout time.Duration
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
	applyDefaults(store)

	cfg := Config{Env: env, store: store}
	if err := store.Bind(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if cfg.Auth == nil || cfg.Auth.AccessKeyID == "" || cfg.Auth.SecretAccessKey == "" {
		return nil, errNoCredentials
	}

	return &cfg, nil
}

func applyDefaults(store gas.ConfigProvider) {
	store.SetDefault("Server.Port", 9000)
	store.SetDefault("Storage.RootDir", "./data")
	store.SetDefault("Server.Domain", "localhost")

	// gas binds its own server settings from this store, and defaults to a 5s
	// read and a 10s write timeout. Object bodies stream for as long as the
	// client needs, so a request that reads or writes one cannot be held to a
	// fixed deadline: both are unbounded here.
	//
	// What stays bounded is the start of a request, which carries no object
	// data: Server.HeaderTimeout becomes the server's ReadHeaderTimeout, and
	// with it the TLS handshake deadline. Unbounded body reads cover neither,
	// and gas's 2m idle timeout only covers connections between requests.
	store.SetDefault("Server.ReadTimeout", "0s")
	store.SetDefault("Server.WriteTimeout", "0s")
	store.SetDefault("Server.HeaderTimeout", "10s")
}
