package app

import (
	"fmt"
	"net/http"
	"os"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/config"
	gaszerolog "github.com/gasmod/gas/log/zerolog"
	"github.com/rs/zerolog"

	"tule.dev/object-store/internal/httpx"
	"tule.dev/object-store/internal/s3api"
	"tule.dev/object-store/internal/util/buildinfo"
)

func newLogger(s *logSettings, env config.Environment) (gas.Logger, error) {
	if env.IsTesting() {
		return &gas.NopLogger{}, nil
	}
	if env.IsProductionLike() {
		return newProdLogger(s)
	}
	return newDevLogger(s)
}

func newProdLogger(_ *logSettings) (gas.Logger, error) {
	out := zerolog.ConsoleWriter{Out: os.Stderr}

	zl := zerolog.New(out).Level(zerolog.WarnLevel).With().Timestamp().
		Str("app_ver", buildinfo.Version).Logger()

	return newZeroLogLogger(&zl)
}

func newDevLogger(_ *logSettings) (gas.Logger, error) {
	out := zerolog.ConsoleWriter{Out: os.Stderr}

	zl := zerolog.New(out).Level(zerolog.TraceLevel).With().Timestamp().
		Str("app_ver", buildinfo.Version).Logger()

	return newZeroLogLogger(&zl)
}

// newZeroLogLogger wraps zl as a gas.Logger. It returns a nil interface, not
// a nil *gaszerolog.Logger, when the backend fails.
func newZeroLogLogger(zl *zerolog.Logger) (gas.Logger, error) {
	l, err := gaszerolog.New(gaszerolog.WithLogger(zl))()
	if err != nil {
		return nil, fmt.Errorf("app: failed to create logger: %w", err)
	}
	return l, nil
}

func resolveRequestLogger(l gas.Logger) httpx.RequestLogger {
	return l.With().Logger()
}

// withRequestLogger puts the request-scoped logger on the request context.
// When it cannot be resolved the request fails with the service's internal
// error, logged through appLogger, since there is no request logger to log it
// with.
func withRequestLogger(appLogger gas.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger, err := gas.ResolveFromRequestScope[httpx.RequestLogger](r)
			if err != nil {
				ctx := gas.WithLogger(r.Context(), appLogger)
				s3api.WriteHTTPError(w, r.WithContext(ctx), s3api.Target{},
					fmt.Errorf("resolve the request logger: %w", err))
				return
			}

			ctx := gas.WithLogger(r.Context(), logger)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
