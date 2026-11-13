package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
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

func TestS3AuthErrorAnswersKeyRefusalsAsS3Does(t *testing.T) {
	for _, tc := range []struct {
		want s3api.Error
		kind sigv4.Kind
	}{
		{kind: sigv4.KindUnknownAccessKey, want: s3api.Error{
			Status: http.StatusForbidden, Code: "InvalidAccessKeyId",
			Message: "The AWS Access Key Id you provided does not exist in our records.",
		}},
		{kind: sigv4.KindInvalidSessionToken, want: s3api.Error{
			Status: http.StatusBadRequest, Code: "InvalidToken",
			Message: "The provided token is malformed or otherwise invalid.",
		}},
		{kind: sigv4.KindExpiredSessionToken, want: s3api.Error{
			Status: http.StatusBadRequest, Code: "ExpiredToken",
			Message: "The provided token has expired.",
		}},
	} {
		for _, presigned := range []bool{false, true} {
			var got *s3api.Error
			e := &sigv4.Error{Kind: tc.kind, Message: "text", Presigned: presigned}
			if !errors.As(s3AuthError(e), &got) {
				t.Fatalf("%s: not an *s3api.Error", tc.kind)
			}
			if *got != tc.want {
				t.Errorf("%s (presigned %t) = %+v, want %+v", tc.kind, presigned, *got, tc.want)
			}
		}
	}
}

// verifyAs signs a request with the key id and the secret "secret" and
// returns v's error verifying it, as the error map left it.
func verifyAs(t *testing.T, v *sigv4.Verifier, id string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://localhost:9000/", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	req.Header.Set("X-Amz-Content-Sha256", emptyHash)
	err = v4.NewSigner().SignHTTP(context.Background(),
		aws.Credentials{AccessKeyID: id, SecretAccessKey: "secret"},
		req, emptyHash, "s3", "us-east-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.Verify(req)
	return err //nolint:wrapcheck // the test inspects the error as is
}

func TestVerifierWithoutIdentityKnowsOnlyTheRootKey(t *testing.T) {
	v, err := newVerifier(&authSettings{AccessKeyID: "root", SecretAccessKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyAs(t, v, "root"); err != nil {
		t.Fatalf("root key: %v", err)
	}
	var s3Err *s3api.Error
	err = verifyAs(t, v, "AKIAOTHER")
	if !errors.As(err, &s3Err) || s3Err.Code != "InvalidAccessKeyId" {
		t.Fatalf("another key: got %v, want InvalidAccessKeyId", err)
	}
}

// An unreachable identity is a failed lookup, which s3AuthError leaves an
// InternalError, never an unknown key or a verified request.
func TestVerifierFailsClosedWhenIdentityIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	v, err := newVerifier(&authSettings{
		AccessKeyID: "root", SecretAccessKey: "secret", IdentityEndpoint: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyAs(t, v, "root"); err != nil {
		t.Fatalf("root key: %v", err)
	}
	var vErr *sigv4.Error
	err = verifyAs(t, v, "AKIAOTHER")
	if !errors.As(err, &vErr) || vErr.Kind != sigv4.KindKeyLookupFailed {
		t.Fatalf("another key: got %v, want %s", err, sigv4.KindKeyLookupFailed)
	}
}

// A hung identity is a failed lookup once the call times out, not a request
// held open.
func TestVerifierFailsClosedWhenIdentityHangs(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	saved := identityTimeout
	identityTimeout = 100 * time.Millisecond
	t.Cleanup(func() { identityTimeout = saved })

	v, err := newVerifier(&authSettings{
		AccessKeyID: "root", SecretAccessKey: "secret", IdentityEndpoint: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var vErr *sigv4.Error
	err = verifyAs(t, v, "AKIAOTHER")
	if !errors.As(err, &vErr) || vErr.Kind != sigv4.KindKeyLookupFailed {
		t.Fatalf("another key: got %v, want %s", err, sigv4.KindKeyLookupFailed)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the lookup took %v, want about the 100ms timeout", elapsed)
	}
}
