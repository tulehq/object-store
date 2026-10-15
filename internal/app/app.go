// Package app loads the configuration and wires the services, modules and
// middleware into a gas application.
package app

import (
	"fmt"
	"net/http"

	"github.com/gasmod/gas"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/httpx"
	"tule.dev/object-store/internal/modules/bucket"
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

	registerInfraServices(app, cfg, logger)
	registerAppServices(app, cfg)
	registerAppModules(app, cfg)
	installMiddleware(app.Router(), cfg)

	return app, nil
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
}

func installMiddleware(r *gas.Router, cfg *Config) {
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

	r.UseMiddlewareFunc(withRequestLogger)

	r.UseMiddlewareFunc(gas.RequestLogger[httpx.RequestLogger]())

	r.UseMiddlewareFunc(verifySignature(sigv4.NewVerifier(sigv4.Credentials{
		AccessKeyID:     cfg.Auth.AccessKeyID,
		SecretAccessKey: cfg.Auth.SecretAccessKey,
	})))
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
