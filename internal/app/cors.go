package app

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// The CORS errors S3 answers a preflight it refuses with: 403
// AccessForbidden, with "CORSResponse: CORS is not enabled for this
// bucket." for a bucket without a CORS configuration and "CORSResponse:
// This CORS request is not allowed..." for one no rule allows. The
// troubleshooting page ("Troubleshooting CORS", S3 User Guide) shows the
// 403 and the messages' first sentences; the code and the messages in full
// are S3's as its clients report them, which its error code list does not
// include.
var (
	errCORSNotEnabled = s3api.NewError(http.StatusForbidden, "AccessForbidden",
		"CORSResponse: CORS is not enabled for this bucket.")
	errCORSNotAllowed = s3api.NewError(http.StatusForbidden, "AccessForbidden",
		"CORSResponse: This CORS request is not allowed. This is usually because the "+
			"evalution of Origin, request method / Access-Control-Request-Method or "+
			"Access-Control-Request-Headers are not whitelisted by the resource's CORS spec.")
)

// corsVary is the Vary header S3 answers a CORS request with ("Testing
// CORS", S3 User Guide).
const corsVary = "Origin, Access-Control-Request-Headers, Access-Control-Request-Method"

// isPreflight reports whether r is a CORS preflight request: an OPTIONS
// with an Origin and an Access-Control-Request-Method.
func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Origin") != "" &&
		r.Header.Get("Access-Control-Request-Method") != ""
}

// bucketCORS answers CORS requests with the CORS configuration of the
// bucket they address, as S3 does ("Using cross-origin resource sharing
// (CORS)" and "How does Amazon S3 evaluate the CORS configuration on a
// bucket?", S3 User Guide). The bucket is the one a <bucket>.<domain> host
// names, or the first segment of the path. It runs before verifySignature:
//
//   - A preflight is answered here, without a signature, as browsers send
//     it without one: 200 with Access-Control-Allow-Origin,
//     Access-Control-Allow-Methods (the rule's methods),
//     Access-Control-Allow-Headers (the headers requested, when any),
//     Access-Control-Expose-Headers, Access-Control-Max-Age,
//     Access-Control-Allow-Credentials and Vary, as "Testing CORS" shows
//     S3 answering one, when the first rule matching its origin, method
//     and requested headers allows it, and 403 AccessForbidden otherwise,
//     or when the bucket has no CORS configuration or does not exist.
//   - Any other request with an Origin header is passed on with the same
//     headers but Access-Control-Allow-Headers set, when a rule matches
//     its origin and method, so its response, an error's included, is
//     readable by the page. The request is then verified and authorized
//     as any other: CORS grants no access.
//
// A request allowed by a bare "*" AllowedOrigin is answered with
// Access-Control-Allow-Origin: * and no Access-Control-Allow-Credentials,
// and one allowed by any other AllowedOrigin of its rule with the
// request's origin and
// Access-Control-Allow-Credentials: true, as S3 is observed to answer;
// its documentation shows only the second. Vary is sent on every response
// to a request with an Origin whose bucket has a CORS configuration.
// Requests without an Origin are passed on untouched, without reading the
// configuration. A configuration that cannot be read or decoded fails the
// request with 500 InternalError; it is never treated as absent.
func bucketCORS(domain string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			bucket := corsBucket(r, domain)
			cors, err := readCORS(r, bucket)
			if err != nil {
				s3api.WriteHTTPError(w, r, s3api.Target{Bucket: bucket}, err)
				return
			}
			if isPreflight(r) {
				preflight(w, r, bucket, cors)
				return
			}
			if cors != nil {
				w.Header().Set("Vary", corsVary)
				if rule, allowed, ok := cors.Match(origin, r.Method, nil); ok {
					setCORSHeaders(w.Header(), rule, allowed, origin)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// corsBucket returns the bucket r addresses, "" for none.
func corsBucket(r *http.Request, domain string) string {
	if b := hostBucket(r.Host, domain); b != "" {
		return b
	}
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	return bucket
}

// readCORS returns bucket's CORS configuration, nil when it has none or
// there is no such bucket.
func readCORS(r *http.Request, bucket string) (*s3api.CORSConfiguration, error) {
	if bucket == "" {
		return nil, nil
	}
	store, err := gas.ResolveFromRequestScope[contracts.StorageProvider](r)
	if err != nil {
		return nil, fmt.Errorf("app: resolve the storage: %w", err)
	}
	doc, err := store.GetBucketConfig(r.Context(), bucket, contracts.ConfigCORS)
	switch {
	case errors.Is(err, contracts.ErrNoSuchBucketConfig), errors.Is(err, storage.ErrBucketNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("app: read the CORS configuration of bucket %s: %w", bucket, err)
	}
	cors, err := s3api.ParseCORSConfiguration(doc)
	if err != nil {
		return nil, fmt.Errorf("app: the stored CORS configuration of bucket %s does not "+
			"parse: %w", bucket, err)
	}
	return cors, nil
}

// preflight answers the preflight r with cors, bucket's CORS configuration
// (nil for none).
func preflight(
	w http.ResponseWriter,
	r *http.Request,
	bucket string,
	cors *s3api.CORSConfiguration,
) {
	target := s3api.Target{Bucket: bucket}
	if cors == nil {
		s3api.WriteHTTPError(w, r, target, errCORSNotEnabled)
		return
	}
	var headers []string
	for _, v := range r.Header.Values("Access-Control-Request-Headers") {
		for h := range strings.SplitSeq(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				headers = append(headers, h)
			}
		}
	}
	origin := r.Header.Get("Origin")
	rule, allowed, ok := cors.Match(origin, r.Header.Get("Access-Control-Request-Method"),
		headers)
	if !ok {
		s3api.WriteHTTPError(w, r, target, errCORSNotAllowed)
		return
	}
	h := w.Header()
	setCORSHeaders(h, rule, allowed, origin)
	if len(headers) > 0 {
		h.Set("Access-Control-Allow-Headers", strings.Join(headers, ", "))
	}
	h.Set("Vary", corsVary)
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

// setCORSHeaders sets the headers a request from origin that rule allows,
// by its AllowedOrigin allowed, is answered with.
func setCORSHeaders(h http.Header, rule *s3api.CORSRule, allowed, origin string) {
	if allowed == "*" {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
	}
	h.Set("Access-Control-Allow-Methods", strings.Join(rule.AllowedMethods, ", "))
	if len(rule.ExposeHeaders) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeaders, ", "))
	}
	if rule.MaxAgeSeconds != nil {
		h.Set("Access-Control-Max-Age", strconv.Itoa(*rule.MaxAgeSeconds))
	}
}
