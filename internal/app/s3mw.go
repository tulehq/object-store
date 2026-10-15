package app

import (
	"net"
	"net/http"
	"strings"

	"tule.dev/object-store/internal/s3api"
	"tule.dev/object-store/internal/sigv4"
)

// verifySignature rejects requests whose SigV4 signature does not verify
// against v. It runs before virtualHost, since clients sign the path as they
// sent it.
func verifySignature(v *sigv4.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	suffix := "." + strings.ToLower(domain)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				host = r.Host
			}
			bucket, ok := strings.CutSuffix(strings.ToLower(host), suffix)
			if !ok || bucket == "" {
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
