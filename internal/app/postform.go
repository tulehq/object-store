package app

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"tule.dev/sigv4"

	"tule.dev/object-store/internal/postpolicy"
	"tule.dev/object-store/internal/s3api"
)

// formBucket returns the bucket r posts a POST Object form to, false when
// r is not one: a POST of multipart/form-data, with no Authorization header
// and no query, to a bucket alone, path-style or on a <bucket>.<domain>
// host. It runs before virtualHost, so it reads the path as the client sent
// it. Any other request, such as a form posted with a query, is verified as
// a signed request.
func formBucket(r *http.Request, domain string) (string, bool) {
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" ||
		r.URL.RawQuery != "" {
		return "", false
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		return "", false
	}
	if bucket := hostBucket(r.Host, domain); bucket != "" {
		return bucket, r.URL.Path == "/" || r.URL.Path == ""
	}
	bucket := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/")
	return bucket, bucket != "" && !strings.Contains(bucket, "/")
}

// serveForm verifies r, a POST Object form to bucket, against the policy
// it carries, with keys, and serves next the request that stores it: its
// context carries the policy's signer, as a verified request's does, and
// the upload (postpolicy.FromContext); its body is the form's file; and its
// headers are only those the form's fields set, Content-Type, so that no
// header of the request, which nothing signed, sets anything of the
// object or plays a part in its evaluation.
func serveForm(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
	keys sigv4.KeySource,
	bucket string,
) {
	up, err := postpolicy.Verify(r.Context(), r, bucket, keys, time.Now())
	if err != nil {
		var kErr *postpolicy.KeyError
		if errors.As(err, &kErr) {
			err = formKeyError(kErr.Err)
		}
		s3api.WriteHTTPError(w, r, s3api.Target{Bucket: bucket}, err)
		return
	}

	ctx := postpolicy.NewContext(sigv4.NewContext(r.Context(), up.Principal), up)
	fr := r.WithContext(ctx)
	fr.Header = http.Header{}
	if up.ContentType != "" {
		fr.Header.Set("Content-Type", up.ContentType)
	}
	fr.Body = io.NopCloser(up.Body)
	fr.ContentLength = -1
	next.ServeHTTP(w, fr)
}

// formKeyError is the error of a KeySource that gave no key for a POST
// policy's credential, mapped as the same failure of a signed request is
// (s3AuthError): an unknown key, a session token that is invalid or
// expired, or, for any other error, a failed lookup, which is an
// InternalError.
func formKeyError(err error) error {
	kind := sigv4.KindKeyLookupFailed
	switch {
	case errors.Is(err, sigv4.ErrUnknownAccessKey):
		kind = sigv4.KindUnknownAccessKey
	case errors.Is(err, sigv4.ErrInvalidSessionToken):
		kind = sigv4.KindInvalidSessionToken
	case errors.Is(err, sigv4.ErrExpiredSessionToken):
		kind = sigv4.KindExpiredSessionToken
	}
	return s3AuthError(&sigv4.Error{Kind: kind, Err: err, Message: err.Error()})
}
