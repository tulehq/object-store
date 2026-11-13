package app

import (
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"tule.dev/sigv4"

	"tule.dev/object-store/internal/modules/health"
	"tule.dev/object-store/internal/s3api"
)

// newVerifier returns the verifier for the one access key the server
// accepts, configured for S3: presigned URLs, a streamed payload checked
// against a hex hash as it is read, UNSIGNED-PAYLOAD and STREAMING-*
// payloads, the path encoded once, credentials scoped to s3, and failures
// answered with S3's errors. The credential's region is not checked, so a
// client may sign for any region.
func newVerifier(accessKeyID, secretAccessKey string) (*sigv4.Verifier, error) {
	keys, err := sigv4.NewStaticKey(sigv4.StaticKeyConfig{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
	})
	if err != nil {
		return nil, fmt.Errorf("app: failed to create the signing key: %w", err)
	}
	v, err := sigv4.New(
		keys,
		sigv4.WithPresignedURLs(true),
		sigv4.WithS3PathEncoding(),
		sigv4.WithStreamedPayload(),
		sigv4.WithUnsignedPayload(true),
		sigv4.WithStreamingPayload(true),
		sigv4.WithRequiredSignedHeaders(),
		sigv4.WithService("s3"),
		sigv4.WithErrorMap(s3AuthError),
	)
	if err != nil {
		return nil, fmt.Errorf("app: failed to create the signature verifier: %w", err)
	}
	return v, nil
}

// s3AuthErrors are the S3 errors a verification failure is answered with.
// header is the error for an Authorization header request; query, when set,
// replaces it for a presigned URL. An empty message stands for the
// failure's own.
var s3AuthErrors = map[sigv4.Kind]struct{ header, query *s3api.Error }{
	sigv4.KindMissingAuth: {header: s3api.NewError(http.StatusForbidden, "AccessDenied",
		"Access Denied")},
	sigv4.KindUnsupportedAlgorithm: {
		header: s3api.ErrInvalidRequest(""), query: malformedQuery,
	},
	sigv4.KindIncompleteAuth:         {header: malformedHeader, query: malformedQuery},
	sigv4.KindMalformedCredential:    {header: malformedHeader, query: malformedQuery},
	sigv4.KindCredentialDateMismatch: {header: malformedHeader, query: malformedQuery},
	sigv4.KindWrongService:           {header: malformedHeader, query: malformedQuery},
	sigv4.KindInvalidDate: {
		header: s3api.NewError(http.StatusForbidden, "AccessDenied", ""), query: malformedQuery,
	},
	sigv4.KindInvalidExpires: {header: malformedQuery},
	sigv4.KindExpired: {
		header: s3api.NewError(http.StatusForbidden, "AccessDenied", ""),
	},
	sigv4.KindRequestNotYetValid: {
		header: s3api.NewError(http.StatusForbidden, "AccessDenied", ""),
	},
	sigv4.KindUnsignedHeader: {header: s3api.NewError(http.StatusForbidden, "AccessDenied",
		"There were headers present in the request which were not signed")},
	sigv4.KindInvalidSessionToken: {header: s3api.NewError(http.StatusBadRequest, "InvalidToken",
		"The provided token is malformed or otherwise invalid.")},
	sigv4.KindExpiredSessionToken: {header: s3api.NewError(http.StatusBadRequest, "ExpiredToken",
		"The provided token has expired.")},
	sigv4.KindMissingPayloadHash: {header: s3api.ErrInvalidRequest("")},
	// The error S3 answers an X-Amz-Content-Sha256 it does not recognize
	// with.
	sigv4.KindPayloadHashNotAccepted: {header: s3api.ErrInvalidArgument("x-amz-content-sha256 " +
		"must be UNSIGNED-PAYLOAD, STREAMING-AWS4-HMAC-SHA256-PAYLOAD, or a valid sha256 value.")},
	sigv4.KindPayloadHashMismatch: {header: s3api.NewError(http.StatusBadRequest,
		"XAmzContentSHA256Mismatch", "")},
	// S3 has no error for a body it cannot verify, since it accepts unsigned
	// payloads.
	sigv4.KindUnverifiedPayload: {header: s3api.ErrInvalidRequest("")},
	// ReadBody reads the DeleteObjects and CompleteMultipartUpload
	// documents, which S3 bounds with MaxMessageLengthExceeded.
	sigv4.KindBodyTooLarge: {header: s3api.NewError(http.StatusBadRequest,
		"MaxMessageLengthExceeded", "Your request was too big.")},
	sigv4.KindRequestTimeTooSkewed: {header: s3api.NewError(http.StatusForbidden,
		"RequestTimeTooSkewed",
		"The difference between the request time and the current time is too large.")},
	sigv4.KindUnknownAccessKey: {header: s3api.NewError(http.StatusForbidden, "InvalidAccessKeyId",
		"The AWS Access Key Id you provided does not exist in our records.")},
	sigv4.KindSignatureMismatch: {header: s3api.NewError(http.StatusForbidden,
		"SignatureDoesNotMatch", "The request signature we calculated does not match the "+
			"signature you provided. Check your key and signing method.")},
}

var (
	malformedHeader = s3api.NewError(http.StatusBadRequest, "AuthorizationHeaderMalformed", "")
	malformedQuery  = s3api.NewError(http.StatusBadRequest, "AuthorizationQueryParametersError", "")
)

// s3AuthError maps a verification failure to its S3 error. A failure with
// none, such as the key lookup failing, is returned as is, which
// s3api.WriteHTTPError answers and logs as an InternalError.
func s3AuthError(e *sigv4.Error) error {
	errs, ok := s3AuthErrors[e.Kind]
	if !ok {
		return e
	}
	s3Err := errs.header
	if e.Presigned && errs.query != nil {
		s3Err = errs.query
	}
	cp := *s3Err
	if cp.Message == "" {
		cp.Message = e.Message
	}
	return &cp
}

// verifySignature rejects requests whose SigV4 signature does not verify
// against v, and serves the request Verify returns, whose body is checked
// against its declared hash as it is read. It runs before virtualHost, since
// clients sign the path as they sent it.
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
			verified, err := v.Verify(r)
			if err != nil {
				s3api.WriteHTTPError(w, r, s3api.Target{}, err)
				return
			}
			next.ServeHTTP(w, verified)
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
