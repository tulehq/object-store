//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // S3 ETags are MD5 digests
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// drainSlowly reads r chunk bytes at a time, pausing between them, and
// returns how many bytes arrived and their quoted MD5. It discards the body
// rather than keeping it, so a body far larger than the socket buffers stays
// cheap to read.
func drainSlowly(r io.Reader, chunk int, pause time.Duration) (int64, string, error) {
	h := md5.New() //nolint:gosec // comparing against an S3 ETag
	buf := make([]byte, chunk)
	var total int64
	for {
		n, err := io.ReadFull(r, buf)
		h.Write(buf[:n])
		total += int64(n)
		switch {
		case err == nil:
			time.Sleep(pause)
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			return total, `"` + hex.EncodeToString(h.Sum(nil)) + `"`, nil
		default:
			return total, "", fmt.Errorf("read the body: %w", err)
		}
	}
}

// TestGracefulShutdownDrainsInFlightRequests covers the shutdown path the
// binary takes on a signal: a download already under way is served to its
// last byte, and the listener stops accepting once it is over.
func TestGracefulShutdownDrainsInFlightRequests(t *testing.T) {
	srv := startServer(t, false, nil)
	c := clientFor(srv)
	createBucket(t, c, "drain")

	body := bytes.Repeat([]byte("drain"), 2<<20)
	put(t, c, "drain", "big.bin", body)

	out, err := c.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("drain"), Key: aws.String("big.bin"),
	})
	requireNoErr(t, err, "get big object")
	defer func() { _ = out.Body.Close() }()

	first := make([]byte, 64<<10)
	n, err := io.ReadFull(out.Body, first)
	requireNoErr(t, err, "read the first chunk")

	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()

	rest, _, err := drainSlowly(out.Body, 256<<10, 5*time.Millisecond)
	requireNoErr(t, err, "read the rest through shutdown")
	requireEqual(t, int64(n)+rest, int64(len(body)), "bytes delivered across shutdown")

	requireNoErr(t, <-stopped, "graceful shutdown")

	_, err = c.ListBuckets(context.Background(), &s3.ListBucketsInput{})
	if err == nil {
		t.Fatal("the server still answered after shutdown")
	}
}
