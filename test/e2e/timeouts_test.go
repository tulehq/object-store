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
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// slowReader hands out data with a pause every chunk bytes, so a request body
// takes a known time to arrive however small the reads it is drained with. It
// is deliberately not an io.Seeker: these bodies are sent to presigned URLs by
// hand, not by the SDK.
type slowReader struct {
	data  []byte
	off   int
	next  int
	chunk int
	pause time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	if r.off >= r.next {
		if r.off > 0 {
			time.Sleep(r.pause)
		}
		r.next += r.chunk
	}
	n := min(min(r.next-r.off, len(p)), len(r.data)-r.off)
	copy(p, r.data[r.off:r.off+n])
	r.off += n
	return n, nil
}

// putSlowly uploads body to a presigned PUT URL over len(body)/chunk pauses.
// It returns the response status, or an error when the server hangs up.
func putSlowly(
	t *testing.T,
	srv *server,
	c *s3.Client,
	bucket, key string,
	body []byte,
	chunk int,
	pause time.Duration,
) (int, error) {
	t.Helper()

	presigned, err := s3.NewPresignClient(c).PresignPutObject(context.Background(),
		&s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	requireNoErr(t, err, "presign put")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut,
		presigned.URL, &slowReader{data: body, chunk: chunk, next: chunk, pause: pause})
	requireNoErr(t, err, "build put request")
	req.ContentLength = int64(len(body))

	res, err := srv.Client().Do(req)
	if err != nil {
		return 0, fmt.Errorf("put %s/%s: %w", bucket, key, err)
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode, nil
}

// TestConfiguredReadTimeoutStopsASlowUpload pins down what a read timeout on
// the server does to an upload, and so what gas's 5s default did to every
// upload slower than it before the defaults were set here: the body never
// finishes arriving and no object is stored.
func TestConfiguredReadTimeoutStopsASlowUpload(t *testing.T) {
	srv := startServer(t, false, map[string]string{"TULE_Server_ReadTimeout": "1s"})
	c := clientFor(srv)
	createBucket(t, c, "slow")

	// 2 MiB over about 3s, well past the 1s the server is allowed to spend
	// reading it.
	body := bytes.Repeat([]byte("s"), 2<<20)
	status, err := putSlowly(
		t, srv, c, "slow", "cut-off.bin", body, len(body)/8, 400*time.Millisecond,
	)
	if err == nil && status < http.StatusBadRequest {
		t.Fatalf("slow upload finished with status %d, want it cut off", status)
	}

	_, err = c.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String("slow"), Key: aws.String("cut-off.bin"),
	})
	requireAs[*types.NotFound](t, err)
}

// TestSlowUploadIsNotTimedOut is the regression test for the shipped read
// timeout: an upload that takes longer to arrive than gas's 5s default has to
// complete. It talks to the real server over a socket, which is the only
// place that deadline exists.
func TestSlowUploadIsNotTimedOut(t *testing.T) {
	if testing.Short() {
		t.Skip("streams a body for over 5s")
	}

	srv := startServer(t, false, nil)
	c := clientFor(srv)
	createBucket(t, c, "stream")

	// 4 MiB over about 6.5s, past the 5s read default.
	body := bytes.Repeat([]byte("abcd"), 1<<20)
	status, err := putSlowly(
		t, srv, c, "stream", "slow.bin", body, len(body)/14, 500*time.Millisecond,
	)
	requireNoErr(t, err, "slow upload")
	requireEqual(t, status, http.StatusOK, "slow upload status")

	head, err := c.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String("stream"), Key: aws.String("slow.bin"),
	})
	requireNoErr(t, err, "head slow upload")
	requireEqual(t, aws.ToInt64(head.ContentLength), int64(len(body)), "stored size")
	requireEqual(t, aws.ToString(head.ETag), md5Hex(body), "stored bytes")
}

// TestSlowDownloadIsNotTimedOut is the regression test for the shipped write
// timeout: a download the client is slow to read has to complete.
//
// net/http sets the write deadline once, when the request is read, so what
// breaches it is a client that stops reading mid-body while the server is
// blocked on a full socket buffer. The object is far larger than the buffers
// between the two ends, so the server really is blocked for the length of the
// stall; pacing a smaller body evenly proves nothing, because the kernel
// buffers its tail and the server finishes writing before the deadline.
func TestSlowDownloadIsNotTimedOut(t *testing.T) {
	if testing.Short() {
		t.Skip("stalls a download for over 10s")
	}

	srv := startServer(t, false, nil)
	c := clientFor(srv)
	createBucket(t, c, "stream")

	body := bytes.Repeat([]byte("abcd"), 8<<20) // 32 MiB
	put(t, c, "stream", "big.bin", body)

	out, err := c.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("stream"), Key: aws.String("big.bin"),
	})
	requireNoErr(t, err, "get big object")
	defer func() { _ = out.Body.Close() }()

	// 11s of not reading, past the 10s write default.
	n, sum, err := stallThenDrain(out.Body, 4<<20, 11*time.Second)
	requireNoErr(t, err, "stalled download")
	requireEqual(t, n, int64(len(body)), "downloaded size")
	requireEqual(t, sum, md5Hex(body), "downloaded bytes")
}

// stallThenDrain reads one chunk from r, waits stall with the body left
// unread, and then reads the rest at full speed. It returns how many bytes
// arrived and their quoted MD5.
func stallThenDrain(r io.Reader, chunk int, stall time.Duration) (int64, string, error) {
	h := md5.New() //nolint:gosec // comparing against an S3 ETag
	buf := make([]byte, chunk)

	n, err := io.ReadFull(r, buf)
	h.Write(buf[:n])
	if err != nil {
		return int64(n), "", err
	}
	time.Sleep(stall)

	rest, err := io.Copy(h, r)
	if err != nil {
		return int64(n) + rest, "", fmt.Errorf("read the rest of the body: %w", err)
	}
	return int64(n) + rest, `"` + hex.EncodeToString(h.Sum(nil)) + `"`, nil
}

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
