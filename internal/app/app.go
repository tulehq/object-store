// Package app loads the configuration and wires the services, modules and
// middleware into a gas application.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gasmod/gas"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/httpx"
	"tule.dev/object-store/internal/modules/bucket"
	"tule.dev/object-store/internal/modules/health"
	"tule.dev/object-store/internal/modules/object"
	"tule.dev/object-store/internal/services/storage/dirimpl"
	"tule.dev/object-store/internal/sigv4"
)

// New builds the application from cfg. extra options are applied after the
// app's own.
func New(cfg *Config, extra ...gas.Option) (*gas.App, error) {
	logger, lErr := newLogger(cfg.Log, cfg.Env)
	if lErr != nil {
		return nil, lErr
	}

	// S3 requests are authenticated by signature, not cookies, so the
	// cross-origin protection meant for browser sessions only gets in the way
	// of browser uploads to presigned URLs.
	opts := append([]gas.Option{gas.WithCSRFInsecureBypassPattern("/")}, extra...)
	app := gas.NewApp(opts...)

	installHeaderTimeout(app, cfg.Server.HeaderTimeout)
	registerInfraServices(app, cfg, logger)
	registerAppServices(app, cfg)
	registerAppModules(app, cfg)
	installMiddleware(app.Router(), cfg, logger)
	app.ReadyFunc(createBuckets(cfg.Storage.Buckets))

	return app, nil
}

// installHeaderTimeout bounds how long a client may take to send its request
// headers, and with them to complete a TLS handshake.
//
// gas's ServerSettings has no such field, and its *http.Server is built from
// the configuration while the app starts, so the field is set from the
// AppConfigUpdated event that ends the binding -- the last point before
// anything serves, and the first at which Server() builds the server the
// deployment actually uses. net/http derives both deadlines from ReadTimeout
// when ReadHeaderTimeout is unset, and ReadTimeout is deliberately unset
// here, so without this a connection that trickles header bytes or stalls
// mid-handshake is held open indefinitely.
func installHeaderTimeout(app *gas.App, d time.Duration) {
	app.EventBus().Subscribe[gas.AppConfigUpdated](func(gas.AppConfigUpdatedPayload) {
		app.Server().ReadHeaderTimeout = d
	})
}

func registerInfraServices(app *gas.App, cfg *Config, logger gas.Logger) {
	app.RegisterServiceInstance[gas.ConfigProvider](cfg.store)
	app.RegisterServiceInstance[gas.Logger](logger)
	app.RegisterScopedService[httpx.RequestLogger](resolveRequestLogger)
}

func registerAppServices(app *gas.App, cfg *Config) {
	app.RegisterSingletonService[contracts.StorageProvider](dirimpl.NewService(cfg.Storage))
}

func registerAppModules(app *gas.App, _ *Config) {
	app.RegisterSingletonService[*bucket.Module](bucket.NewModule)
	app.RegisterSingletonService[*object.Module](object.NewModule)
	app.RegisterSingletonService[*health.Module](health.NewModule)
}

// createBuckets returns a ready hook that creates each of names that does not
// exist yet. Ready hooks run before the server listens, so clients never see
// the store without them.
func createBuckets(names []string) func(*gas.ServiceContainer) error {
	return func(sc *gas.ServiceContainer) error {
		storage, err := sc.Resolve[contracts.StorageProvider]()
		if err != nil {
			return fmt.Errorf("app: failed to resolve storage: %w", err)
		}
		for _, name := range names {
			if name == "" {
				continue
			}
			_, cErr := storage.CreateBucket(context.Background(), name)
			if cErr != nil && !errors.Is(cErr, contracts.ErrBucketExists) {
				return fmt.Errorf("app: failed to create bucket %q: %w", name, cErr)
			}
		}
		return nil
	}
}

func installMiddleware(r *gas.Router, cfg *Config, logger gas.Logger) {
	r.UseMiddlewareFunc(cors.Handler(cors.Options{
		// Echo any origin back, which unlike "*" also works for credentialed
		// requests.
		AllowOriginFunc: func(*http.Request, string) bool { return true },
		AllowedMethods: []string{
			http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost, http.MethodDelete,
		},
		AllowedHeaders: []string{"*"},
		ExposedHeaders: []string{
			"ETag", "Content-Length", "Content-Range", "Content-Disposition",
			"Content-Encoding", "Content-Language", "Expires", "Last-Modified",
			"Accept-Ranges", "Cache-Control", "X-Amz-Mp-Parts-Count",
		},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.UseMiddlewareFunc(httpx.RequestID(cfg.Env))

	r.UseMiddlewareFunc(middleware.ClientIPFromXFFTrustedProxies(1))

	r.UseMiddlewareFunc(withRequestLogger(logger))

	r.UseMiddlewareFunc(gas.RequestLogger[httpx.RequestLogger]())

	r.UseMiddlewareFunc(verifySignature(sigv4.NewVerifier(sigv4.Credentials{
		AccessKeyID:     cfg.Auth.AccessKeyID,
		SecretAccessKey: cfg.Auth.SecretAccessKey,
	}), cfg.Server.Domain))
	if cfg.Server.Domain != "" {
		r.UseMiddlewareFunc(virtualHost(cfg.Server.Domain))
	}

	var hsts string
	if cfg.HSTS != nil && cfg.HSTS.MaxAge > 0 {
		hsts = fmt.Sprintf("max-age=%0.f", cfg.HSTS.MaxAge.Seconds())
	}
	r.UseMiddlewareFunc(gas.SecurityHeaders(
		gas.WithSecurityHeadersStrictTransportSecurity(hsts),
	))
}
