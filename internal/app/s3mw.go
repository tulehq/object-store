package app

import (
	"net"
	"net/http"
	"strings"

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
