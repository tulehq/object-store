package app

import (
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
		return gas.NewNopLogger()(), nil
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

	return log.NewZeroLogLogger(log.WithZeroLogInstance(&zl))(), nil
}

func newDevLogger(_ *logSettings) (gas.Logger, error) {
	out := zerolog.ConsoleWriter{Out: os.Stderr}

	zl := zerolog.New(out).Level(zerolog.TraceLevel).With().Timestamp().
		Str("app_ver", buildinfo.Version).Logger()

	return log.NewZeroLogLogger(log.WithZeroLogInstance(&zl))(), nil
}

func resolveRequestLogger(l gas.Logger) httpx.RequestLogger {
	return l.With().Logger()
}

func withRequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger, err := gas.ResolveFromRequestScope[httpx.RequestLogger](r)
		if err != nil {
			return
		}

		ctx := gas.WithLogger(r.Context(), logger)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
