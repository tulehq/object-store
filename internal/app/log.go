package app

import (
	"io"
	"net/http"
	"os"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/config/extensions/gasenv"
	"github.com/gasmod/gas/log"
	"github.com/rs/zerolog"

	"tule.dev/object-store/internal/httpx"
	"tule.dev/object-store/internal/util/buildinfo"
)

func newLogger(s *logSettings, env gasenv.Environment) (gas.Logger, error) {
	if env.IsTesting() {
		return &gas.NopLogger{}, nil
	}
	if env.IsProductionLike() {
		return newProdLogger(os.Stderr)
	}
	return newDevLogger(os.Stderr)
}

// newProdLogger logs to w at warn level. Production-like output is read by
// whatever collects it rather than by a person at a terminal, so it is
// structured JSON, unlike the console format newDevLogger writes.
func newProdLogger(w io.Writer) (gas.Logger, error) {
	zl := zerolog.New(w).Level(zerolog.WarnLevel).With().Timestamp().
		Str("app_ver", buildinfo.Version).Logger()

	return log.NewZeroLogLogger(log.WithZeroLogInstance(&zl))()
}

// newDevLogger logs to w at trace level, in zerolog's console format: a
// person reads it as it is written.
func newDevLogger(w io.Writer) (gas.Logger, error) {
	out := zerolog.ConsoleWriter{Out: w}

	zl := zerolog.New(out).Level(zerolog.TraceLevel).With().Timestamp().
		Str("app_ver", buildinfo.Version).Logger()

	return log.NewZeroLogLogger(log.WithZeroLogInstance(&zl))()
}

func resolveRequestLogger(l gas.Logger) httpx.RequestLogger {
	return l.With().Logger()
}

// withRequestLogger puts the request-scoped logger on the request context, so
// handlers and services read it with httpx.Logger.
//
// Broken logging must not cost the client its request: when the scoped logger
// cannot be resolved the request is served without one, which httpx.Logger
// answers with a no-op logger, and the resolve failure is reported on the
// application logger app, which does not depend on the request scope. This is
// also how the gas.RequestLogger middleware after it behaves.
func withRequestLogger(app gas.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger, err := gas.ResolveFromRequestScope[httpx.RequestLogger](r)
			if err != nil {
				app.Error("failed to resolve the request logger; serving the request unlogged").
					Err("error", err).Send()
				next.ServeHTTP(w, r)
				return
			}

			ctx := gas.WithLogger(r.Context(), logger)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
