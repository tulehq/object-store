//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// versionIDRefusal is the answer to method with query, a subresource
// with ?versionId=X: 501 naming ?versionId, or, as DELETE serves neither
// ?acl nor ?partNumber, naming that subresource; and for a GET of
// ?partNumber, a GetObject of version X, 400 for an ID the server does not
// issue.
func versionIDRefusal(method, query string) (int, string, string) {
	if method == http.MethodGet && query == "partNumber=1&versionId=X" {
		return http.StatusBadRequest, "InvalidArgument", "Invalid version id specified"
	}
	refused := "versionId"
	if method == http.MethodDelete && query != "tagging&versionId=X" {
		refused = strings.SplitN(strings.SplitN(query, "&", 2)[0], "=", 2)[0]
	}
	return http.StatusNotImplemented, "NotImplemented",
		"The ?" + refused + " subresource is not supported."
}

// TestVersionIDWithSubresource checks that ?versionId with a subresource
// not served with it is refused with 501 NotImplemented, rather than
// ignored so the request acts on the current object. A GET of
// ?partNumber with ?versionId is a GetObject of that version's part, so it
// reads version X, which is not one the server issues.
func TestVersionIDWithSubresource(t *testing.T) {
	srv := startLive(t, nil)
	c := clientFor(srv)
	ctx := context.Background()
	createBucket(t, c, "versioned")
	bucket, key := aws.String("versioned"), aws.String("k")
	put(t, c, "versioned", "k", []byte("original"))
	_, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: bucket,
		Key:    key,
		Tagging: &types.Tagging{
			TagSet: []types.Tag{{Key: aws.String("keep"), Value: aws.String("me")}},
		},
	})
	requireNoErr(t, err, "tag object")

	tagging := []byte(
		`<Tagging><TagSet><Tag><Key>new</Key><Value>v</Value></Tag></TagSet></Tagging>`,
	)
	for _, query := range []string{"tagging&versionId=X", "acl&versionId=X", "partNumber=1&versionId=X"} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			req, rErr := http.NewRequestWithContext(ctx, method, srv.URL+"/versioned/k?"+query, nil)
			requireNoErr(t, rErr, "build request")
			hash := emptyHash
			switch {
			case method == http.MethodPut && query == "tagging&versionId=X":
				// A PutObjectTagging the server would act on without
				// ?versionId.
				req.Body = io.NopCloser(bytes.NewReader(tagging))
				req.ContentLength = int64(len(tagging))
				hash = hexSHA256(string(tagging))
			case method == http.MethodPut && query == "acl&versionId=X":
				req.Header.Set("X-Amz-Acl", "public-read")
			}
			req.Header.Set("X-Amz-Content-Sha256", hash)
			sErr := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }).
				SignHTTP(ctx, testCreds, req, hash, "s3", "us-east-1", time.Now())
			requireNoErr(t, sErr, "sign request")
			status, e := send(t, srv.Client(), req)
			wantStatus, code, message := versionIDRefusal(method, query)
			requireS3Error(t, status, e, wantStatus, code, message)
		}
	}

	_, body := get(t, c, &s3.GetObjectInput{Bucket: bucket, Key: key})
	requireEqual(t, string(body), "original", "object untouched")
	tags, err := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "get tagging")
	requireEqual(t, len(tags.TagSet), 1, "tag count")
	requireEqual(t, aws.ToString(tags.TagSet[0].Key), "keep", "tag key")
	acl, err := c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: bucket, Key: key})
	requireNoErr(t, err, "get acl")
	requireEqual(t, len(acl.Grants), 1, "grant count, the owner's only")
}
