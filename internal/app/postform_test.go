package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// formBucket decides which requests skip the signature check for their POST
// policy, so each request that is not a POST Object form must be left to
// it.
func TestFormBucket(t *testing.T) {
	const form = "multipart/form-data; boundary=x"
	for name, tc := range map[string]struct {
		method, url, contentType, auth, want string
		ok                                   bool
	}{
		"path-style":            {url: "http://localhost:9000/docs", ok: true, want: "docs"},
		"path-style, slash":     {url: "http://localhost:9000/docs/", ok: true, want: "docs"},
		"virtual-hosted":        {url: "http://docs.s3.test/", ok: true, want: "docs"},
		"a query":               {url: "http://localhost:9000/docs?delete"},
		"an Authorization":      {url: "http://localhost:9000/docs", auth: "AWS4-HMAC-SHA256 x"},
		"an object path":        {url: "http://localhost:9000/docs/key"},
		"an object, virtual":    {url: "http://docs.s3.test/key"},
		"the root, path-style":  {url: "http://localhost:9000/"},
		"a PUT":                 {method: http.MethodPut, url: "http://localhost:9000/docs"},
		"another content type":  {url: "http://localhost:9000/docs", contentType: "application/xml"},
		"a url-encoded form":    {url: "http://localhost:9000/docs", contentType: "application/x-www-form-urlencoded"},
		"a presigned-like path": {url: "http://localhost:9000/docs?X-Amz-Algorithm=AWS4-HMAC-SHA256"},
	} {
		t.Run(name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = http.MethodPost
			}
			r := httptest.NewRequestWithContext(t.Context(), method, tc.url, http.NoBody)
			ct := tc.contentType
			if ct == "" {
				ct = form
			}
			r.Header.Set("Content-Type", ct)
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			bucket, ok := formBucket(r, "s3.test")
			if ok != tc.ok || ok && bucket != tc.want {
				t.Fatalf("formBucket = %q, %t; want %q, %t", bucket, ok, tc.want, tc.ok)
			}
		})
	}
}
