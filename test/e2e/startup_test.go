//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestStartupBuckets(t *testing.T) {
	root := t.TempDir()
	env := map[string]string{
		"TULE_Storage_RootDir": root,
		"TULE_Storage_Buckets": "uploads, assets",
	}
	ctx := context.Background()

	c := clientFor(startServer(t, false, env))
	put(t, c, "uploads", "kept.txt", []byte("kept"))

	// A restart on the same root finds the buckets already there.
	c = clientFor(startServer(t, false, env))
	list, err := c.ListBuckets(ctx, &s3.ListBucketsInput{})
	requireNoErr(t, err, "list buckets")
	names := make([]string, 0, len(list.Buckets))
	for _, b := range list.Buckets {
		names = append(names, aws.ToString(b.Name))
	}
	slices.Sort(names)
	requireEqual(t, slices.Equal(names, []string{"assets", "uploads"}), true, "startup buckets")
	_, body := get(t, c, &s3.GetObjectInput{
		Bucket: aws.String("uploads"), Key: aws.String("kept.txt"),
	})
	requireEqual(t, string(body), "kept", "object survives restart")
}

func TestProbes(t *testing.T) {
	srv := startServer(t, false, nil)

	for _, path := range []string{"/_health", "/_ready"} {
		status, _ := getWith(t, srv.Client(), srv.URL+path)
		requireEqual(t, status, http.StatusOK, "unsigned "+path)
	}

	// On a virtual host the probe path is an object key, so it stays signed.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/_health", nil)
	requireNoErr(t, err, "build request")
	req.Host = "probe.localhost"
	res, err := srv.Client().Do(req)
	requireNoErr(t, err, "virtual-hosted probe path")
	_ = res.Body.Close()
	requireEqual(t, res.StatusCode, http.StatusForbidden, "virtual-hosted probe path")
}
