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
	"tule.dev/functions/serviceinvoke"
	"tule.dev/queues/servicesend"
	"tule.dev/sigv4"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/httpx"
	"tule.dev/object-store/internal/modules/bucket"
	"tule.dev/object-store/internal/modules/health"
	"tule.dev/object-store/internal/modules/object"
	"tule.dev/object-store/internal/notification"
	"tule.dev/object-store/internal/s3api"
	"tule.dev/object-store/internal/services/destinations"
	"tule.dev/object-store/internal/services/notify"
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
	implicitACL, aErr := object.ImplicitACL(cfg.Auth.AccountID)
	if aErr != nil {
		return nil, aErr
	}
	queues, functions, dErr := newDestinationClients(cfg)
	if dErr != nil {
		return nil, dErr
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
	registerAppServices(app, cfg, verifier, bound, ready, implicitACL, queues, functions)
	registerAppModules(app, cfg)
	installMiddleware(app.Router(), cfg, logger, verifier)
	app.ReadyFunc(createBuckets(cfg.Storage.Buckets))
	app.ReadyFunc(recoverEvents(logger))
	// Last, so no lifecycle rule applies and no event is delivered before
	// every service and ready hook has succeeded, the events' recovery
	// included.
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
// the notification destinations, through queues and functions, either nil
// for a service without an endpoint; the lifecycle sweep, which applies
// no rule before ready is closed; and the event deliverer, which delivers
// nothing before it either. implicitACL is the ACL document of an
// object without one of its own.
func registerAppServices(
	app *gas.App,
	cfg *Config,
	verifier *sigv4.Verifier,
	bound contracts.PolicyPrincipals,
	ready <-chan struct{},
	implicitACL []byte,
	queues *servicesend.Client,
	functions *serviceinvoke.Client,
) {
	clock := cfg.clockOrSystem()
	// The storage wakes the deliverer through signal once it confirms an
	// event; a wake-up already waiting stands for this one too.
	signal := make(chan struct{}, 1)
	app.RegisterServiceInstance[*sigv4.Verifier](verifier)
	app.RegisterServiceInstance[contracts.PolicyPrincipals](bound)
	app.RegisterServiceInstance[contracts.Destinations](destinations.New(queues, functions))
	// The router sends events to the destination types whose service has
	// an endpoint, so the storage records only events that can be
	// delivered.
	available := map[contracts.DestinationType]bool{
		contracts.DestinationQueue:    queues != nil,
		contracts.DestinationFunction: functions != nil,
	}
	app.RegisterSingletonService[contracts.StorageProvider](
		dirimpl.NewService(cfg.Storage, clock, dirimpl.Events{
			Router:      notification.NewRouter(cfg.Auth.AccountID, available),
			ImplicitACL: implicitACL,
			Signal: func() {
				select {
				case signal <- struct{}{}:
				default:
				}
			},
		}),
	)
	// It depends on the storage, so gas closes it, stopping its sweeps,
	// before the storage.
	app.RegisterSingletonService[*sweep.Service](
		sweep.NewService(sweep.Settings{Clock: clock, Ready: ready}),
	)
	// The deliverer depends on the storage too, so it stops, leaving the
	// events in flight in the outbox, before the storage closes. It runs
	// on the system's clock, never the storage's (the event notifications
	// design, section 2.3): an event's PendingEvent.Created is the wall
	// time it was recorded at, which its 24-hour age bound is measured
	// against, and its retries wait in wall time, as the destination
	// calls' timeouts do.
	timing := cfg.notifyTiming
	timing.Clock, timing.Ready, timing.Signal = contracts.SystemClock, ready, signal
	timing.Account = cfg.Auth.AccountID
	app.RegisterSingletonService[*notify.Service](notify.NewService(timing))
	app.RegisterSingletonService[*authz.Authorizer](
		func(storage contracts.StorageProvider) *authz.Authorizer {
			return authz.New(cfg.Auth.AccountID, storage)
		})
}

// destinationTimeout bounds one call to the queues service's service-send
// endpoint or the functions service's service-invoke endpoint: a send, or
// a dry run or Event invocation, neither of which waits for the function.
const destinationTimeout = 10 * time.Second

// newDestinationClients returns the clients of the queues and functions
// services' endpoints, each nil when its endpoint is not set. Both
// endpoints accept only their account's root user, so the calls are signed
// with the configured root key, for the region every bucket is in.
func newDestinationClients(
	cfg *Config,
) (*servicesend.Client, *serviceinvoke.Client, error) {
	var (
		queues    *servicesend.Client
		functions *serviceinvoke.Client
		err       error
	)
	if cfg.Storage.QueuesEndpoint != "" {
		queues, err = servicesend.New(servicesend.Config{
			HTTPClient:      &http.Client{Timeout: destinationTimeout},
			Endpoint:        cfg.Storage.QueuesEndpoint,
			Region:          contracts.Region,
			AccessKeyID:     cfg.Auth.AccessKeyID,
			SecretAccessKey: cfg.Auth.SecretAccessKey,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("app: queues service-send client: %w", err)
		}
	}
	if cfg.Storage.FunctionsEndpoint != "" {
		functions, err = serviceinvoke.New(serviceinvoke.Config{
			HTTPClient:      &http.Client{Timeout: destinationTimeout},
			Endpoint:        cfg.Storage.FunctionsEndpoint,
			Region:          contracts.Region,
			AccessKeyID:     cfg.Auth.AccessKeyID,
			SecretAccessKey: cfg.Auth.SecretAccessKey,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("app: functions service-invoke client: %w", err)
		}
	}
	return queues, functions, nil
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

// recoverEvents returns a ready hook that settles the bucket events a stop
// left recorded but not confirmed (see StorageProvider's RecoverEvents,
// then ConfirmCommitted),
// before the server listens and before the sweep runs, so nothing changes
// what it checks. A failure is logged and does not fail the start: the
// intents it could not settle are kept for the next one.
func recoverEvents(logger gas.Logger) func(*gas.ServiceContainer) error {
	return func(sc *gas.ServiceContainer) error {
		storage, err := sc.Resolve[contracts.StorageProvider]()
		if err != nil {
			return fmt.Errorf("app: failed to resolve storage: %w", err)
		}
		out, err := storage.RecoverEvents(context.Background())
		if err != nil {
			logger.Error("failed to recover bucket events").Err("error", err).Send()
		}
		committed, err := storage.ConfirmCommitted(context.Background())
		if err != nil {
			logger.Error("failed to confirm committed bucket events").Err("error", err).Send()
		}
		if out.Confirmed > 0 || out.Dropped > 0 || committed > 0 {
			logger.Info("recovered bucket events").
				Int("confirmed", out.Confirmed+committed).Int("dropped", out.Dropped).Send()
		}
		return nil
	}
}

func installMiddleware(r *gas.Router, cfg *Config, logger gas.Logger, verifier *sigv4.Verifier) {
	r.UseMiddlewareFunc(nil, httpx.RequestID)

	r.UseMiddlewareFunc(nil, middleware.ClientIPFromXFFTrustedProxies(1))

	r.UseMiddlewareFunc(nil, withRequestLogger(logger))

	r.UseMiddlewareFunc(nil, gas.RequestLogger[httpx.RequestLogger]())

	// Before the signature check, so preflights, which carry none, are
	// answered, and so the CORS headers reach a page on its errors too.
	r.UseMiddlewareFunc(nil, bucketCORS(cfg.Server.Domain))

	r.UseMiddlewareFunc(nil, verifySignature(verifier, cfg.Server.Domain))
	// After the request IDs and the signature, whose values it records.
	r.UseMiddlewareFunc(nil, eventSource)
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
