package app

import (
	"errors"
	"net/http"
	"testing"

	"tule.dev/sigv4"

	"tule.dev/object-store/internal/s3api"
)

func TestS3AuthErrorPicksTheQueryErrorForAPresignedURL(t *testing.T) {
	for _, tc := range []struct {
		err  *sigv4.Error
		want s3api.Error
	}{
		{
			err: &sigv4.Error{Kind: sigv4.KindIncompleteAuth, Message: "header text"},
			want: s3api.Error{
				Status: http.StatusBadRequest, Code: "AuthorizationHeaderMalformed",
				Message: "header text",
			},
		},
		{
			err: &sigv4.Error{
				Kind: sigv4.KindIncompleteAuth, Message: "query text", Presigned: true,
			},
			want: s3api.Error{
				Status: http.StatusBadRequest, Code: "AuthorizationQueryParametersError",
				Message: "query text",
			},
		},
		{
			// No query error: a presigned URL gets the header one.
			err: &sigv4.Error{
				Kind: sigv4.KindExpired, Message: "Request has expired",
				Presigned: true,
			},
			want: s3api.Error{
				Status: http.StatusForbidden, Code: "AccessDenied", Message: "Request has expired",
			},
		},
		{
			// S3's own text replaces the failure's.
			err: &sigv4.Error{Kind: sigv4.KindMissingAuth, Message: "Missing Authentication Token"},
			want: s3api.Error{
				Status: http.StatusForbidden, Code: "AccessDenied", Message: "Access Denied",
			},
		},
	} {
		var got *s3api.Error
		if !errors.As(s3AuthError(tc.err), &got) {
			t.Fatalf("%s: not an *s3api.Error", tc.err.Kind)
		}
		if *got != tc.want {
			t.Errorf(
				"%s (presigned %t) = %+v, want %+v",
				tc.err.Kind,
				tc.err.Presigned,
				*got,
				tc.want,
			)
		}
	}
}

func TestS3AuthErrorDoesNotChangeTheSharedErrors(t *testing.T) {
	first := s3AuthError(&sigv4.Error{Kind: sigv4.KindIncompleteAuth, Message: "first"})
	second := s3AuthError(&sigv4.Error{Kind: sigv4.KindIncompleteAuth, Message: "second"})
	if first.Error() == second.Error() {
		t.Fatalf("both failures map to %q", first)
	}
	if malformedHeader.Message != "" {
		t.Fatalf("the shared error's message became %q", malformedHeader.Message)
	}
}

func TestS3AuthErrorLeavesAnUnmappedKindAnInternalError(t *testing.T) {
	e := &sigv4.Error{Kind: sigv4.KindKeyLookupFailed, Message: "lookup"}
	if got := s3AuthError(e); !errors.Is(got, e) {
		t.Fatalf("s3AuthError = %v, want the failure itself", got)
	}
	var s3Err *s3api.Error
	if errors.As(s3AuthError(e), &s3Err) {
		t.Fatalf("an unmapped failure became %v", s3Err)
	}
}
