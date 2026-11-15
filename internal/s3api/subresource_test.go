package s3api

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"testing"
)

func queryRequest(t *testing.T, rawQuery string) *http.Request {
	t.Helper()
	u, err := url.Parse("http://localhost/bucket/key?" + rawQuery)
	if err != nil {
		t.Fatalf("parse query %q: %v", rawQuery, err)
	}
	return &http.Request{URL: u}
}

func TestSubresources(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", nil},
		{"list-type=2&encoding-type=url", nil},
		{"tagging", []string{"tagging"}},
		{"versionId=X&tagging", []string{"tagging", "versionId"}},
		{"uploadId=U&partNumber=1", []string{"partNumber", "uploadId"}},
	} {
		got := Subresources(queryRequest(t, tc.query))
		if !slices.Equal(got, tc.want) {
			t.Errorf("Subresources(?%s) = %v, want %v", tc.query, got, tc.want)
		}
		wantFirst := ""
		if len(tc.want) > 0 {
			wantFirst = tc.want[0]
		}
		if got := Subresource(queryRequest(t, tc.query)); got != wantFirst {
			t.Errorf("Subresource(?%s) = %q, want %q", tc.query, got, wantFirst)
		}
	}
}

// TestCheckSubresourceEvery covers requests whose first subresource is
// served but whose others are not: each was served against the current
// object with ?versionId ignored.
func TestCheckSubresourceEvery(t *testing.T) {
	for _, tc := range []struct {
		query   string
		refused string // the subresource named in the error, "" for none
		allowed []string
	}{
		// The calls the handlers make once they dispatch on Subresource.
		{"tagging&versionId=X", "versionId", []string{"tagging"}},
		{"acl&versionId=X", "versionId", []string{"acl"}},
		{"attributes&versionId=X", "versionId", []string{"attributes"}},
		{"partNumber=1&versionId=X", "versionId", []string{"partNumber"}},
		{"uploadId=U&versionId=X", "versionId", []string{"uploadId"}},
		{"uploadId=U&partNumber=1&versionId=X", "versionId", []string{"uploadId", "partNumber"}},
		{"acl&tagging", "tagging", []string{"acl"}},
		{"uploads&versionId=X", "versionId", []string{"uploads"}},
		{"versionId=X", "versionId", nil},

		// Combinations served today stay served.
		{"", "", nil},
		{"list-type=2&encoding-type=url&prefix=a", "", nil},
		{"tagging", "", []string{"tagging"}},
		{"partNumber=1", "", []string{"partNumber"}},
		{"uploadId=U&partNumber=1", "", []string{"uploadId", "partNumber"}},
		{"uploadId=U&max-parts=10&part-number-marker=2", "", []string{"uploadId"}},
		{"uploads&key-marker=a&upload-id-marker=b&max-uploads=5", "", []string{"uploads"}},
		{"delete", "", []string{"delete"}},
	} {
		err := CheckSubresource(queryRequest(t, tc.query), tc.allowed...)
		if tc.refused == "" {
			if err != nil {
				t.Errorf("CheckSubresource(?%s, %v) = %v, want nil", tc.query, tc.allowed, err)
			}
			continue
		}
		var s3Err *Error
		if !errors.As(err, &s3Err) || s3Err.Status != http.StatusNotImplemented ||
			s3Err.Code != "NotImplemented" {
			t.Errorf(
				"CheckSubresource(?%s, %v) = %v, want 501 NotImplemented",
				tc.query,
				tc.allowed,
				err,
			)
			continue
		}
		if want := "The ?" + tc.refused + " subresource is not supported."; s3Err.Message != want {
			t.Errorf(
				"CheckSubresource(?%s, %v) message %q, want %q",
				tc.query,
				tc.allowed,
				s3Err.Message,
				want,
			)
		}
	}
}
