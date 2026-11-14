//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The tests here drive CORS as a browser does, with net/http preflights
// and requests carrying Origin, against the live server.

const (
	devOrigin   = "http://localhost:3000"
	otherOrigin = "http://elsewhere.example.test"
	corsVary    = "Origin, Access-Control-Request-Headers, Access-Control-Request-Method"
)

// preflight sends the preflight a browser sends before a cross-origin
// method request with headers to rawURL from origin.
func preflight(
	t *testing.T,
	rawURL, origin, method, headers string,
) *liveResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, rawURL, nil)
	requireNoErr(t, err, "build preflight")
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", method)
	if headers != "" {
		req.Header.Set("Access-Control-Request-Headers", headers)
	}
	return sendLive(t, req)
}

// requireRefusedPreflight checks that res is S3's 403 AccessForbidden
// for a preflight with message, and carries no CORS headers.
func requireRefusedPreflight(t *testing.T, res *liveResponse, message string) {
	t.Helper()
	body := res.Body
	requireEqual(t, res.StatusCode, http.StatusForbidden, "refused preflight status")
	requireEqual(t, strings.Contains(body, "<Code>AccessForbidden</Code>"), true,
		"AccessForbidden in "+body)
	requireEqual(t, strings.Contains(body, "<Message>"+message), true, "message in "+body)
	requireEqual(t, res.Header.Get("Access-Control-Allow-Origin"), "", "no allow-origin")
}

// webRules are the CORS rules of the bucket "web": the dev origin may GET
// and PUT with content-type and any x-amz- header, any origin may GET,
// and any subdomain of example.test may DELETE.
var webRules = []types.CORSRule{
	{
		ID:             aws.String("dev"),
		AllowedOrigins: []string{devOrigin},
		AllowedMethods: []string{http.MethodGet, http.MethodPut},
		AllowedHeaders: []string{"content-type", "x-amz-*"},
		ExposeHeaders:  []string{"ETag", "x-amz-server-side-encryption"},
		MaxAgeSeconds:  aws.Int32(600),
	},
	{AllowedOrigins: []string{"*"}, AllowedMethods: []string{http.MethodGet}},
	{
		AllowedOrigins: []string{"http://*.example.test"},
		AllowedMethods: []string{http.MethodDelete},
	},
}

func TestCORSConfiguration(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "web")
	bucket := aws.String("web")

	_, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: bucket})
	requireCode(t, err, "NoSuchCORSConfiguration")
	requireResponse(t, err, http.StatusNotFound)

	_, err = c.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: bucket, CORSConfiguration: &types.CORSConfiguration{CORSRules: webRules},
	})
	requireNoErr(t, err, "put bucket cors")
	got, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: bucket})
	requireNoErr(t, err, "get bucket cors")
	requireEqual(t, len(got.CORSRules), 3, "rules")
	dev := got.CORSRules[0]
	requireEqual(t, aws.ToString(dev.ID), "dev", "rule ID")
	requireEqual(t, strings.Join(dev.AllowedMethods, ","), "GET,PUT", "methods")
	requireEqual(t, strings.Join(dev.AllowedHeaders, ","), "content-type,x-amz-*", "headers")
	requireEqual(t, strings.Join(dev.ExposeHeaders, ","), "ETag,x-amz-server-side-encryption",
		"expose headers")
	requireEqual(t, aws.ToInt32(dev.MaxAgeSeconds), 600, "max age")

	_, err = c.DeleteBucketCors(ctx, &s3.DeleteBucketCorsInput{Bucket: bucket})
	requireNoErr(t, err, "delete bucket cors")
	_, err = c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: bucket})
	requireCode(t, err, "NoSuchCORSConfiguration")
}

func TestCORSConfigurationValidation(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "web")
	bucket := aws.String("web")
	_, err := c.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: bucket, CORSConfiguration: &types.CORSConfiguration{CORSRules: webRules},
	})
	requireNoErr(t, err, "put bucket cors")

	invalid := map[string]types.CORSRule{
		"a method CORS does not take": {
			AllowedOrigins: []string{devOrigin}, AllowedMethods: []string{http.MethodPatch},
		},
		"two wildcards in an origin": {
			AllowedOrigins: []string{"http://*.*.example.test"},
			AllowedMethods: []string{http.MethodGet},
		},
		"two wildcards in a header": {
			AllowedOrigins: []string{devOrigin}, AllowedMethods: []string{http.MethodGet},
			AllowedHeaders: []string{"x-*-*"},
		},
		"a wildcard expose header": {
			AllowedOrigins: []string{devOrigin}, AllowedMethods: []string{http.MethodGet},
			ExposeHeaders: []string{"*"},
		},
		"an ID over 255 characters": {
			ID:             aws.String(strings.Repeat("i", 256)),
			AllowedOrigins: []string{devOrigin}, AllowedMethods: []string{http.MethodGet},
		},
		"a negative max age": {
			AllowedOrigins: []string{devOrigin}, AllowedMethods: []string{http.MethodGet},
			MaxAgeSeconds: aws.Int32(-1),
		},
	}
	for name, rule := range invalid {
		_, err = c.PutBucketCors(ctx, &s3.PutBucketCorsInput{
			Bucket: bucket, CORSConfiguration: &types.CORSConfiguration{
				CORSRules: []types.CORSRule{rule},
			},
		})
		requireCode(t, err, "InvalidRequest")
		requireResponse(t, err, http.StatusBadRequest)
		t.Logf("%s: refused", name)
	}
	many := make([]types.CORSRule, 101)
	for i := range many {
		many[i] = types.CORSRule{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}}
	}
	_, err = c.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: bucket, CORSConfiguration: &types.CORSConfiguration{CORSRules: many},
	})
	requireCode(t, err, "InvalidRequest")

	// The refused configurations left the stored one in place.
	got, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: bucket})
	requireNoErr(t, err, "get bucket cors after refusals")
	requireEqual(t, len(got.CORSRules), 3, "rules after refusals")
}

func TestCORSPreflight(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	createBucket(t, c, "web")
	put(t, c, "web", "k.txt", []byte("hello"))
	objectURL := l.URL + "/web/k.txt"

	// Without a configuration every preflight is refused, as is one to a
	// bucket that does not exist.
	res := preflight(t, objectURL, devOrigin, http.MethodGet, "")
	requireRefusedPreflight(t, res, "CORSResponse: CORS is not enabled for this bucket.")
	res = preflight(t, l.URL+"/missing/k.txt", devOrigin, http.MethodGet, "")
	requireRefusedPreflight(t, res, "CORSResponse: CORS is not enabled for this bucket.")

	_, err := c.PutBucketCors(t.Context(), &s3.PutBucketCorsInput{
		Bucket: aws.String("web"), CORSConfiguration: &types.CORSConfiguration{CORSRules: webRules},
	})
	requireNoErr(t, err, "put bucket cors")

	// The dev origin, a method and headers its rule allows: the requested
	// headers come back, the origin is echoed with credentials allowed.
	res = preflight(t, objectURL, devOrigin, http.MethodPut,
		"Content-Type, x-amz-meta-owner,x-amz-date")
	requireEqual(t, res.StatusCode, http.StatusOK, "allowed preflight status")
	h := res.Header
	requireEqual(t, h.Get("Access-Control-Allow-Origin"), devOrigin, "allow-origin")
	requireEqual(t, h.Get("Access-Control-Allow-Methods"), "GET, PUT", "allow-methods")
	requireEqual(t, h.Get("Access-Control-Allow-Headers"),
		"Content-Type, x-amz-meta-owner, x-amz-date", "allow-headers")
	requireEqual(t, h.Get("Access-Control-Expose-Headers"),
		"ETag, x-amz-server-side-encryption", "expose-headers")
	requireEqual(t, h.Get("Access-Control-Max-Age"), "600", "max-age")
	requireEqual(t, h.Get("Access-Control-Allow-Credentials"), "true", "allow-credentials")
	requireEqual(t, h.Get("Vary"), corsVary, "vary")

	// The bucket itself takes preflights too.
	res = preflight(t, l.URL+"/web", devOrigin, http.MethodGet, "")
	requireEqual(t, res.StatusCode, http.StatusOK, "bucket preflight status")

	// Any origin may GET, by the wildcard rule: "*" with no credentials.
	res = preflight(t, objectURL, otherOrigin, http.MethodGet, "")
	requireEqual(t, res.StatusCode, http.StatusOK, "wildcard preflight status")
	requireEqual(t, res.Header.Get("Access-Control-Allow-Origin"), "*", "wildcard allow-origin")
	requireEqual(t, res.Header.Get("Access-Control-Allow-Credentials"), "",
		"wildcard allow-credentials")
	requireEqual(t, res.Header.Get("Access-Control-Allow-Methods"), "GET", "wildcard methods")

	// A wildcard within an origin matches its subdomains.
	res = preflight(t, objectURL, "http://app.example.test", http.MethodDelete, "")
	requireEqual(t, res.StatusCode, http.StatusOK, "subdomain preflight status")
	requireEqual(t, res.Header.Get("Access-Control-Allow-Origin"), "http://app.example.test",
		"subdomain allow-origin")

	const notAllowed = "CORSResponse: This CORS request is not allowed."
	for name, p := range map[string][3]string{
		"a method no rule allows the origin": {otherOrigin, http.MethodPut, ""},
		"a method no rule allows":            {devOrigin, http.MethodPost, ""},
		"a header no rule allows":            {devOrigin, http.MethodPut, "authorization"},
		"a header the wildcard rule lacks":   {otherOrigin, http.MethodGet, "content-type"},
		"an origin no rule matches":          {"http://example.test", http.MethodDelete, ""},
	} {
		res = preflight(t, objectURL, p[0], p[1], p[2])
		requireRefusedPreflight(t, res, notAllowed)
		t.Logf("%s: refused", name)
	}
}

// Only a preflight is answered without a signature: an OPTIONS that is not
// one, and any other method, is verified as any request is.
func TestCORSOnlyPreflightIsUnsigned(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	createBucket(t, c, "web")
	_, err := c.PutBucketCors(t.Context(), &s3.PutBucketCorsInput{
		Bucket: aws.String("web"), CORSConfiguration: &types.CORSConfiguration{CORSRules: webRules},
	})
	requireNoErr(t, err, "put bucket cors")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions,
		l.URL+"/web/k.txt", nil)
	requireNoErr(t, err, "build options")
	req.Header.Set("Origin", devOrigin)
	res := sendLive(t, req)
	requireEqual(t, res.StatusCode, http.StatusForbidden, "unsigned OPTIONS without a method")
	requireEqual(t, strings.Contains(res.Body, "<Code>AccessDenied</Code>"), true, res.Body)

	// An unsigned GET from an allowed origin is refused as unsigned, and
	// the refusal carries the CORS headers, so the page can read it.
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, l.URL+"/web/k.txt", nil)
	requireNoErr(t, err, "build get")
	req.Header.Set("Origin", devOrigin)
	res = sendLive(t, req)
	requireEqual(t, res.StatusCode, http.StatusForbidden, "unsigned GET")
	requireEqual(t, strings.Contains(res.Body, "<Code>AccessDenied</Code>"), true, res.Body)
	requireEqual(t, res.Header.Get("Access-Control-Allow-Origin"), devOrigin,
		"allow-origin on the refusal")
}

// A browser upload to a presigned URL: the preflight the page's fetch()
// PUT sends, then the PUT, both with Origin, and a presigned GET of what
// it wrote.
func TestCORSPresignedUpload(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "web")
	_, err := c.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: aws.String("web"), CORSConfiguration: &types.CORSConfiguration{CORSRules: webRules},
	})
	requireNoErr(t, err, "put bucket cors")
	presigner := s3.NewPresignClient(c)
	putReq, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("web"), Key: aws.String("upload.txt"),
		ContentType: aws.String("text/plain"),
	})
	requireNoErr(t, err, "presign put")

	res := preflight(t, putReq.URL, devOrigin, http.MethodPut, "content-type")
	requireEqual(t, res.StatusCode, http.StatusOK, "upload preflight status")

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putReq.URL,
		strings.NewReader("from the browser"))
	requireNoErr(t, err, "build put")
	req.Header.Set("Origin", devOrigin)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	res = sendLive(t, req)
	requireEqual(t, res.StatusCode, http.StatusOK, "presigned put status: "+res.Body)
	h := res.Header
	requireEqual(t, h.Get("Access-Control-Allow-Origin"), devOrigin, "put allow-origin")
	requireEqual(t, h.Get("Access-Control-Allow-Methods"), "GET, PUT", "put allow-methods")
	requireEqual(t, h.Get("Access-Control-Expose-Headers"), "ETag, x-amz-server-side-encryption",
		"put expose-headers")
	requireEqual(t, h.Get("Access-Control-Allow-Credentials"), "true", "put allow-credentials")
	requireEqual(t, h.Get("Access-Control-Max-Age"), "600", "put max-age")
	requireEqual(t, h.Get("Vary"), corsVary, "put vary")
	requireEqual(t, h.Get("ETag"), md5Hex([]byte("from the browser")), "put etag")

	requireCrossOriginReads(t, presigner)
}

// requireCrossOriginReads checks a presigned GET of web/upload.txt from
// another origin, allowed by the wildcard rule, and a presigned DELETE no
// rule allows the dev origin.
func requireCrossOriginReads(t *testing.T, presigner *s3.PresignClient) {
	t.Helper()
	ctx := t.Context()
	getReq, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("web"), Key: aws.String("upload.txt"),
	})
	requireNoErr(t, err, "presign get")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, getReq.URL, nil)
	requireNoErr(t, err, "build get")
	req.Header.Set("Origin", otherOrigin)
	res := sendLive(t, req)
	requireEqual(t, res.StatusCode, http.StatusOK, "presigned get status")
	requireEqual(t, res.Body, "from the browser", "presigned get body")
	requireEqual(t, res.Header.Get("Access-Control-Allow-Origin"), "*", "get allow-origin")

	// A method no rule allows the origin is served, as CORS grants no
	// access and takes none away, but without CORS headers, so a browser
	// hides the response from the page.
	delReq, err := presigner.PresignDeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("web"), Key: aws.String("upload.txt"),
	})
	requireNoErr(t, err, "presign delete")
	req, err = http.NewRequestWithContext(ctx, http.MethodDelete, delReq.URL, nil)
	requireNoErr(t, err, "build delete")
	req.Header.Set("Origin", devOrigin)
	res = sendLive(t, req)
	requireEqual(t, res.StatusCode, http.StatusNoContent, "presigned delete status")
	requireEqual(t, res.Header.Get("Access-Control-Allow-Origin"), "", "delete allow-origin")
	requireEqual(t, res.Header.Get("Vary"), corsVary, "delete vary")
}

// A virtual-hosted request's bucket is its host's.
func TestCORSVirtualHost(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	createBucket(t, c, "web")
	_, err := c.PutBucketCors(t.Context(), &s3.PutBucketCorsInput{
		Bucket: aws.String("web"), CORSConfiguration: &types.CORSConfiguration{CORSRules: webRules},
	})
	requireNoErr(t, err, "put bucket cors")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, l.URL+"/k.txt", nil)
	requireNoErr(t, err, "build preflight")
	req.Host = "web.localhost"
	req.Header.Set("Origin", devOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	res := sendLive(t, req)
	requireEqual(t, res.StatusCode, http.StatusOK, "virtual-hosted preflight status")
	requireEqual(t, res.Header.Get("Access-Control-Allow-Origin"), devOrigin,
		"virtual-hosted allow-origin")
}

// A rule listing an origin before "*" answers that origin as itself, with
// credentials, and any other with "*".
func TestCORSListedOriginBeforeWildcard(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	createBucket(t, c, "web")
	_, err := c.PutBucketCors(t.Context(), &s3.PutBucketCorsInput{
		Bucket: aws.String("web"), CORSConfiguration: &types.CORSConfiguration{
			CORSRules: []types.CORSRule{{
				AllowedOrigins: []string{devOrigin, "*"}, AllowedMethods: []string{http.MethodGet},
			}},
		},
	})
	requireNoErr(t, err, "put bucket cors")
	res := preflight(t, l.URL+"/web/k", devOrigin, http.MethodGet, "")
	requireEqual(t, res.Header.Get("Access-Control-Allow-Origin"), devOrigin, "listed origin")
	requireEqual(t, res.Header.Get("Access-Control-Allow-Credentials"), "true",
		"listed origin credentials")
	res = preflight(t, l.URL+"/web/k", otherOrigin, http.MethodGet, "")
	requireEqual(t, res.Header.Get("Access-Control-Allow-Origin"), "*", "other origin")
	requireEqual(t, res.Header.Get("Access-Control-Allow-Credentials"), "",
		"other origin credentials")
}
