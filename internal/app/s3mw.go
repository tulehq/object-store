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
			if isProbe(r) && hostBucket(r.Host, domain) == "" {
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

// isProbe reports whether r is a probe request. The probes are GET routes,
// which the router also serves as HEAD, so no other method is a probe and
// every other one stays signed. The path is matched against the escaped form,
// which is what the router matches routes against: a path that only decodes
// to a probe path, such as "/%5Fhealth", is routed elsewhere and so is not
// exempt from verification.
func isProbe(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	path := r.URL.EscapedPath()
	return path == health.HealthPath || path == health.ReadyPath
}
