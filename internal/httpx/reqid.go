// Package httpx holds HTTP plumbing shared by the modules: request IDs and the
// request-scoped logger.
package httpx

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"tule.dev/object-store/internal/contracts"
)

// The response headers that carry a request's ID pair, as S3 names them.
const (
	RequestIDHeader = "x-amz-request-id"
	HostIDHeader    = "x-amz-id-2"
)

// hostIDBytes is how many random bytes a host ID encodes (see
// contracts.HostIDBytes).
const hostIDBytes = contracts.HostIDBytes

type hostIDKey struct{}

// RequestID is middleware that gives each request an ID pair, as S3 does,
// and answers it on every response, errors included:
//
//   - the request ID, 8 random bytes as 16 uppercase hex characters, in
//     x-amz-request-id and on the context under chi's middleware.RequestIDKey,
//     so the request logger and middleware.GetReqID read the same value;
//   - the host ID, base64 of 48 random bytes, in x-amz-id-2 and on the
//     context for HostID.
//
// Both are random in every environment, so an ID never repeats across a
// restart, and a request's X-Request-Id header is ignored, as S3 never takes
// a request ID from the client. Both values are opaque to clients.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, hostID := contracts.NewRequestID(), contracts.NewHostID()
		w.Header().Set(RequestIDHeader, id)
		w.Header().Set(HostIDHeader, hostID)
		ctx := context.WithValue(r.Context(), middleware.RequestIDKey, id)
		ctx = context.WithValue(ctx, hostIDKey{}, hostID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// HostID returns the host ID RequestID gave the request ctx belongs to, the
// value its response carries in x-amz-id-2, or "" outside one.
func HostID(ctx context.Context) string {
	id, _ := ctx.Value(hostIDKey{}).(string)
	return id
}
