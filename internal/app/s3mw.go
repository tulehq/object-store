package app

import (
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"tule.dev/object-store/internal/modules/health"
	"tule.dev/object-store/internal/s3api"
	"tule.dev/object-store/internal/sigv4"
)

// verifySignature rejects requests whose SigV4 signature does not verify
// against v. It runs before virtualHost, since clients sign the path as they
// sent it.
//
// The health probes are exempt, so health checks need no credentials. On a
// <bucket>.<domain> host the same path is an object key, which stays signed.
func verifySignature(v *sigv4.Verifier, domain string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProbe(r.URL.Path) && hostBucket(r.Host, domain) == "" {
				next.ServeHTTP(w, r)
				return
			}
			if err := v.Verify(r); err != nil {
				s3api.WriteHTTPError(w, r, s3api.Target{}, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// virtualHost rewrites virtual-hosted-style requests, addressed to
// <bucket>.<domain>, into the path-style requests the routes serve.
func virtualHost(domain string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bucket := hostBucket(r.Host, domain)
			if bucket == "" {
				next.ServeHTTP(w, r)
				return
			}

			u := *r.URL
			u.Path = "/" + bucket + u.Path
			if u.RawPath != "" {
				u.RawPath = "/" + bucket + u.RawPath
			}
			rr := *r
			rr.URL = &u
			next.ServeHTTP(w, &rr)
		})
	}
}

// hostBucket returns the bucket a <bucket>.<domain> host addresses, or "" when
// host is not one or domain is empty.
func hostBucket(host, domain string) string {
	if domain == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	bucket, ok := strings.CutSuffix(strings.ToLower(host), "."+strings.ToLower(domain))
	if !ok {
		return ""
	}
	return bucket
}

func isProbe(path string) bool {
	return path == health.HealthPath || path == health.ReadyPath
}

// chiMethods are the methods the router's chi tree knows, in the order the
// Allow header lists them.
var chiMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost, http.MethodDelete,
	http.MethodPatch, http.MethodOptions, http.MethodConnect, http.MethodTrace, "QUERY",
}

// methodNotAllowed answers as chi's own handler did, before gas's router
// would answer with problem details: a method chi does not know gets 405
// with an empty body and no Allow header, on any path; a known method on a
// path routed for other methods only gets 405 with an empty body and an
// Allow header naming them. A known method on a path no route matches is
// left to the router.
func methodNotAllowed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.RouteContext(r.Context())
		if rctx == nil || rctx.Routes == nil {
			next.ServeHTTP(w, r)
			return
		}

		if slices.Contains(chiMethods, r.Method) {
			allow := routedMethods(rctx.Routes, r)
			if len(allow) == 0 || slices.Contains(allow, r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			for _, m := range allow {
				w.Header().Add("Allow", m)
			}
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
}

// routedMethods returns the methods routes serves r's path for, matching
// the path as chi routes it.
func routedMethods(routes chi.Routes, r *http.Request) []string {
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.Path
	}
	if path == "" {
		path = "/"
	}

	var allow []string
	for _, m := range chiMethods {
		if routes.Match(chi.NewRouteContext(), m, path) {
			allow = append(allow, m)
		}
	}
	return allow
}
