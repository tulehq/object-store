//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

var requestIDFormat = regexp.MustCompile(`^[0-9A-F]{16}$`)

// requestIDs is a response's ID pair.
type requestIDs struct{ request, host string }

// requireIDs checks h carries a well-formed ID pair and returns it.
func requireIDs(t *testing.T, h http.Header, what string) requestIDs {
	t.Helper()
	ids := requestIDs{h.Get("x-amz-request-id"), h.Get("x-amz-id-2")}
	if !requestIDFormat.MatchString(ids.request) {
		t.Errorf("%s: x-amz-request-id %q is not 16 uppercase hex characters", what, ids.request)
	}
	if raw, err := base64.StdEncoding.DecodeString(ids.host); err != nil || len(raw) != 48 {
		t.Errorf(
			"%s: x-amz-id-2 %q is not base64 of 48 bytes (%d, %v)",
			what,
			ids.host,
			len(raw),
			err,
		)
	}
	return ids
}

// requireErrorBodyIDs checks body is an S3 error whose RequestId and
// HostId are ids.
func requireErrorBodyIDs(t *testing.T, body []byte, ids requestIDs, what string) {
	t.Helper()
	var e struct {
		RequestID string `xml:"RequestId"`
		HostID    string `xml:"HostId"`
	}
	if err := xml.Unmarshal(body, &e); err != nil {
		t.Fatalf("%s: decode %q: %v", what, body, err)
	}
	requireEqual(t, e.RequestID, ids.request, what+": body RequestId")
	requireEqual(t, e.HostID, ids.host, what+": body HostId")
}

// wireCapture is what one SDK call's response carried on the wire, and
// the metadata the SDK reported for it.
type wireCapture struct {
	header   http.Header
	metadata middleware.Metadata
	body     []byte
}

// capture returns a per-call option that records the raw response before
// the SDK deserializes it, and the call's metadata, even when it fails.
func capture(c *wireCapture) func(*s3.Options) {
	return func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, func(st *middleware.Stack) error {
			if err := st.Initialize.Add(middleware.InitializeMiddlewareFunc(
				"captureMetadata",
				func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
					middleware.InitializeOutput, middleware.Metadata, error,
				) {
					out, md, err := next.HandleInitialize(ctx, in)
					c.metadata = md
					return out, md, err //nolint:wrapcheck // the test inspects the SDK error
				},
			), middleware.Before); err != nil {
				return fmt.Errorf("add captureMetadata: %w", err)
			}
			return st.Deserialize.Add(middleware.DeserializeMiddlewareFunc(
				"captureResponse",
				func(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (
					middleware.DeserializeOutput, middleware.Metadata, error,
				) {
					out, md, err := next.HandleDeserialize(ctx, in)
					if res, ok := out.RawResponse.(*smithyhttp.Response); ok {
						c.header = res.Header.Clone()
						body, rErr := io.ReadAll(res.Body)
						if rErr != nil {
							return out, md, fmt.Errorf("read the response body: %w", rErr)
						}
						_ = res.Body.Close()
						c.body = body
						res.Body = io.NopCloser(bytes.NewReader(body))
					}
					return out, md, err //nolint:wrapcheck // the test inspects the SDK error
				},
			), middleware.After)
		})
	}
}

// requireMetadataIDs checks the SDK read ids into md.
func requireMetadataIDs(t *testing.T, md middleware.Metadata, ids requestIDs, what string) {
	t.Helper()
	reqID, ok := awsmiddleware.GetRequestIDMetadata(md)
	requireEqual(t, ok, true, what+": request ID in the metadata")
	requireEqual(t, reqID, ids.request, what+": metadata request ID")
	hostID, ok := s3.GetHostIDMetadata(md)
	requireEqual(t, ok, true, what+": host ID in the metadata")
	requireEqual(t, hostID, ids.host, what+": metadata host ID")
}

// TestRequestIDs checks that every response carries x-amz-request-id and
// x-amz-id-2, that the SDK reads them, that an error body's RequestId and
// HostId are the same pair, and that each request gets its own.
func TestRequestIDs(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "ids")

	var ok wireCapture
	out, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("ids"), Key: aws.String("k"), Body: strings.NewReader("x"),
	}, capture(&ok))
	requireNoErr(t, err, "put")
	okIDs := requireIDs(t, ok.header, "put")
	requireMetadataIDs(t, out.ResultMetadata, okIDs, "put result")

	var failed wireCapture
	_, err = c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("ids"), Key: aws.String("missing"),
	}, capture(&failed))
	requireCode(t, err, "NoSuchKey")
	failedIDs := requireIDs(t, failed.header, "failed get")
	requireMetadataIDs(t, failed.metadata, failedIDs, "failed get")
	requireErrorBodyIDs(t, failed.body, failedIDs, "failed get")
	if failedIDs.request == okIDs.request || failedIDs.host == okIDs.host {
		t.Errorf("two requests share IDs: %+v and %+v", okIDs, failedIDs)
	}

	// The router's own answers carry them too.
	var refused wireCapture
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("ids"), Key: aws.String("k"),
	}, capture(&refused), editRequest(func(r *smithyhttp.Request) { r.Method = http.MethodPatch }))
	requireResponse(t, err, http.StatusMethodNotAllowed)
	requireIDs(t, refused.header, "405")
}

// TestRequestIDsPresigned checks presigned requests, which a browser or
// curl sends with no SDK, answer the pair on success and in errors.
func TestRequestIDsPresigned(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "ids")
	put(t, c, "ids", "k", []byte("x"))
	presigner := s3.NewPresignClient(c)

	for _, tc := range []struct {
		key    string
		status int
	}{{"k", http.StatusOK}, {"missing", http.StatusNotFound}} {
		get, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String("ids"), Key: aws.String(tc.key),
		})
		requireNoErr(t, err, "presign get "+tc.key)
		req, err := http.NewRequestWithContext(ctx, get.Method, get.URL, nil)
		requireNoErr(t, err, "build get "+tc.key)
		res := sendLive(t, req)
		requireEqual(t, res.StatusCode, tc.status, "presigned get "+tc.key+": "+res.Body)
		ids := requireIDs(t, res.Header, "presigned get "+tc.key)
		if tc.status != http.StatusOK {
			requireErrorBodyIDs(t, []byte(res.Body), ids, "presigned get "+tc.key)
		}
	}
}

// TestRequestIDsUnauthenticated checks responses to unsigned requests:
// an allowed and a refused CORS preflight, and an anonymous GET.
func TestRequestIDsUnauthenticated(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "web")
	_, err := c.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: aws.String("web"), CORSConfiguration: &types.CORSConfiguration{CORSRules: webRules},
	})
	requireNoErr(t, err, "put bucket cors")

	res := preflight(t, l.URL+"/web/k", devOrigin, http.MethodPut, "content-type")
	requireEqual(t, res.StatusCode, http.StatusOK, "allowed preflight: "+res.Body)
	requireIDs(t, res.Header, "allowed preflight")

	res = preflight(t, l.URL+"/web/k", otherOrigin, http.MethodPut, "")
	requireRefusedPreflight(t, res, "CORSResponse: This CORS request is not allowed.")
	requireErrorBodyIDs(t, []byte(res.Body), requireIDs(t, res.Header, "refused preflight"),
		"refused preflight")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.URL+"/web/k", nil)
	requireNoErr(t, err, "build anonymous get")
	res = sendLive(t, req)
	requireEqual(t, res.StatusCode, http.StatusForbidden, "anonymous get: "+res.Body)
	requireErrorBodyIDs(t, []byte(res.Body), requireIDs(t, res.Header, "anonymous get"),
		"anonymous get")
}
