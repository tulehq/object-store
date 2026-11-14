//go:build e2e

package e2e

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gasmod/gas/config"

	"tule.dev/object-store/internal/app"
)

// liveServer is object-store run as the binary runs it: the app's own
// HTTP server (App.Serve) listening on a free port, called over the
// network, rather than its handler served by httptest.
type liveServer struct {
	// URL is the server's base URL.
	URL string
}

// startLive starts the app on a fresh storage root and a free port, with
// env setting extra TULE_ variables over the test/test credentials, waits
// until it is ready, and stops it when the test ends.
func startLive(t *testing.T, env map[string]string) *liveServer {
	t.Helper()

	port := freePort(t)
	t.Setenv("TULE_GasEnv", "testing")
	t.Setenv("TULE_Storage_RootDir", t.TempDir())
	t.Setenv("TULE_Auth_AccessKeyID", "test")
	t.Setenv("TULE_Auth_SecretAccessKey", "test")
	t.Setenv("TULE_Server_Port", strconv.Itoa(port))
	for k, v := range env {
		t.Setenv(k, v)
	}

	cfg, err := app.LoadConfigFrom(config.NewEnvProvider(
		config.WithEnvSeparator("_"),
		config.WithEnvPrefix("TULE_"),
	))
	requireNoErr(t, err, "load config")
	a, err := app.New(cfg)
	requireNoErr(t, err, "new app")
	requireNoErr(t, a.Start(context.Background()), "start app")

	served := make(chan error, 1)
	go func() { served <- a.Serve() }()
	t.Cleanup(func() {
		if sErr := a.Stop(context.Background()); sErr != nil {
			t.Errorf("stop: %v", sErr)
		}
		if sErr := <-served; sErr != nil {
			t.Errorf("serve: %v", sErr)
		}
	})

	l := &liveServer{URL: "http://127.0.0.1:" + strconv.Itoa(port)}
	l.waitReady(t, served)
	return l
}

// startLiveWithIdentity starts the app with the root key, accepting the
// keys the identity service at identityURL issued.
func startLiveWithIdentity(t *testing.T, identityURL string) *liveServer {
	t.Helper()
	return startLive(t, map[string]string{
		"TULE_Auth_AccessKeyID":      rootKeyID,
		"TULE_Auth_SecretAccessKey":  rootSecret,
		"TULE_Auth_IdentityEndpoint": identityURL,
	})
}

// waitReady polls the readiness probe until it answers 200, failing if
// the server stops serving first.
func (l *liveServer) waitReady(t *testing.T, served chan error) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-served:
			served <- err
			t.Fatalf("the server stopped before it was ready: %v", err)
		default:
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, l.URL+"/_ready", nil)
		requireNoErr(t, err, "build readiness request")
		if res, dErr := client.Do(req); dErr == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the server was not ready in time")
}

// client returns a path-style S3 client for l signing with creds, which
// does not retry, so a refusal is seen as the server answered it.
func (l *liveServer) client(creds aws.Credentials) *s3.Client {
	return s3.New(s3.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(l.URL),
		UsePathStyle:     true,
		Credentials:      credentials.StaticCredentialsProvider{Value: creds},
		HTTPClient:       &http.Client{Timeout: time.Minute},
		RetryMaxAttempts: 1,
	})
}

// testClient is client with the test/test credentials of a standalone
// server.
func (l *liveServer) testClient() *s3.Client {
	return l.client(aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"})
}

// liveResponse is a response with its body read.
type liveResponse struct {
	Header     http.Header
	Body       string
	StatusCode int
}

// sendLive makes req and returns its response with the body read.
func sendLive(t *testing.T, req *http.Request) *liveResponse {
	t.Helper()
	res, err := (&http.Client{Timeout: time.Minute}).Do(req)
	requireNoErr(t, err, req.Method+" "+req.URL.String())
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	requireNoErr(t, err, "read the response body")
	return &liveResponse{Header: res.Header, Body: string(body), StatusCode: res.StatusCode}
}
