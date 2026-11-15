// Package app loads the configuration and wires the services, modules and
// middleware into a gas application.
package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/gasmod/gas"
	"github.com/go-chi/chi/v5/middleware"
	"tule.dev/sigv4"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/httpx"
	"tule.dev/object-store/internal/modules/bucket"
	"tule.dev/object-store/internal/modules/health"
	"tule.dev/object-store/internal/modules/object"
	"tule.dev/object-store/internal/s3api"
	"tule.dev/object-store/internal/services/storage/dirimpl"
	"tule.dev/object-store/internal/services/sweep"
)

// New builds the application from cfg. extra options are applied after the
// app's own.
func New(cfg *Config, extra ...gas.Option) (*gas.App, error) {
	logger, lErr := newLogger(cfg.Log, cfg.Env)
	if lErr != nil {
		return nil, lErr
	}
	verifier, vErr := newVerifier(cfg.Auth)
	if vErr != nil {
		return nil, vErr
	}
	bound, pErr := newPolicyPrincipals(cfg.Auth)
	if pErr != nil {
		return nil, pErr
	}

	// S3 requests are authenticated by signature, not cookies, so the
	// cross-origin protection meant for browser sessions only gets in the way
	// of browser uploads to presigned URLs, which bucketCORS answers as S3
	// does.
	//
	// A request the protection still rejects, one whose path is not in the
	// canonical form the bypass pattern matches, such as a key holding "//",
	// gets net/http's own plain-text 403, which a nil deny handler restores
	// in place of gas's problem details.
	opts := append([]gas.Option{
		gas.WithCSRFInsecureBypassPattern("/"),
		gas.WithCSRFDenyHandler(nil),
	}, extra...)
	app := gas.NewApp(opts...)

	registerInfraServices(app, cfg, logger)
	ready := make(chan struct{})
	registerAppServices(app, cfg, verifier, bound, ready)
	registerAppModules(app, cfg)
	installMiddleware(app.Router(), cfg, logger, verifier)
	app.ReadyFunc(createBuckets(cfg.Storage.Buckets))
	// Last, so no lifecycle rule applies before every service and ready
	// hook has succeeded.
	app.ReadyFunc(func(*gas.ServiceContainer) error {
		close(ready)
		return nil
	})

	return app, nil
}

func registerInfraServices(app *gas.App, cfg *Config, logger gas.Logger) {
	app.RegisterServiceInstance[gas.ConfigProvider](cfg.store)
	app.RegisterServiceInstance[gas.Logger](logger)
	app.RegisterScopedService[httpx.RequestLogger](resolveRequestLogger)
}

// registerAppServices registers the storage, the signature verifier, whose
// ReadBody the modules read small request documents with, and the
// authorizer the modules evaluate each operation with, which reads bucket
// policies from the storage, and how bucket policies' principals are bound;
// and the lifecycle sweep, which applies no rule before ready is closed.
func registerAppServices(
	app *gas.App,
	cfg *Config,
	verifier *sigv4.Verifier,
	bound contracts.PolicyPrincipals,
	ready <-chan struct{},
) {
	clock := cfg.clockOrSystem()
	app.RegisterServiceInstance[*sigv4.Verifier](verifier)
	app.RegisterServiceInstance[contracts.PolicyPrincipals](bound)
	app.RegisterSingletonService[contracts.StorageProvider](
		dirimpl.NewService(cfg.Storage, clock),
	)
	// It depends on the storage, so gas closes it, stopping its sweeps,
	// before the storage.
	app.RegisterSingletonService[*sweep.Service](
		sweep.NewService(sweep.Settings{Clock: clock, Ready: ready}),
	)
	app.RegisterSingletonService[*authz.Authorizer](
		func(storage contracts.StorageProvider) *authz.Authorizer {
			return authz.New(cfg.Auth.AccountID, storage)
		})
}

func registerAppModules(app *gas.App, _ *Config) {
	app.RegisterSingletonService[*bucket.Module](bucket.NewModule)
	app.RegisterSingletonService[*object.Module](object.NewModule)
	app.RegisterSingletonService[*health.Module](health.NewModule)
}

// createBuckets returns a ready hook that creates each of names that does not
// exist yet, with a new bucket's configuration (bucket.DefaultConfigs). Ready
// hooks run before the server listens, so clients never see the store
// without them.
func createBuckets(names []string) func(*gas.ServiceContainer) error {
	return func(sc *gas.ServiceContainer) error {
		storage, err := sc.Resolve[contracts.StorageProvider]()
		if err != nil {
			return fmt.Errorf("app: failed to resolve storage: %w", err)
		}
		configs, err := bucket.DefaultConfigs(s3api.OwnershipBucketOwnerEnforced, nil)
		if err != nil {
			return fmt.Errorf("app: failed to build the new bucket configuration: %w", err)
		}
		for _, name := range names {
			if name == "" {
				continue
			}
			_, cErr := storage.CreateBucket(context.Background(), name, configs)
			if cErr != nil && !errors.Is(cErr, contracts.ErrBucketExists) {
				return fmt.Errorf("app: failed to create bucket %q: %w", name, cErr)
			}
		}
		return nil
	}
}

func installMiddleware(r *gas.Router, cfg *Config, logger gas.Logger, verifier *sigv4.Verifier) {
	r.UseMiddlewareFunc(nil, httpx.RequestID(cfg.Env))

	r.UseMiddlewareFunc(nil, middleware.ClientIPFromXFFTrustedProxies(1))

	r.UseMiddlewareFunc(nil, withRequestLogger(logger))

	r.UseMiddlewareFunc(nil, gas.RequestLogger[httpx.RequestLogger]())

	// Before the signature check, so preflights, which carry none, are
	// answered, and so the CORS headers reach a page on its errors too.
	r.UseMiddlewareFunc(nil, bucketCORS(cfg.Server.Domain))

	r.UseMiddlewareFunc(nil, verifySignature(verifier, cfg.Server.Domain))
	if cfg.Server.Domain != "" {
		r.UseMiddlewareFunc(nil, virtualHost(cfg.Server.Domain))
	}

	var hsts string
	if cfg.HSTS != nil && cfg.HSTS.MaxAge > 0 {
		hsts = fmt.Sprintf("max-age=%0.f", cfg.HSTS.MaxAge.Seconds())
	}
	r.UseMiddlewareFunc(nil, gas.SecurityHeaders(
		gas.WithSecurityHeadersStrictTransportSecurity(hsts),
	))

	// Last, so it sees the path virtualHost rewrote and answers with the
	// security headers set.
	r.UseMiddlewareFunc(nil, methodNotAllowed)
}
