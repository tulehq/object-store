//go:build e2e

package e2e

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gasmod/gas/config"

	"tule.dev/object-store/internal/app"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/services/notify"
)

// liveServer is object-store run as the binary runs it: the app's own
// HTTP server (App.Serve) listening on a free port, called over the
// network.
type liveServer struct {
	httpClient *http.Client
	// stop stops the server, once; the test's end calls it too.
	stop func()

	// URL is the server's base URL.
	URL string
	// Addr is the host:port URL points at.
	Addr string
	// Root is the storage root it serves.
	Root string
}

// Client returns an HTTP client for l, which trusts its certificate when
// it serves TLS. Its idle connections are closed when the test ends.
func (l *liveServer) Client() *http.Client { return l.httpClient }

// liveOptions are the settings of a live server that the environment
// cannot carry.
type liveOptions struct {
	// clock, when set, is the clock the app stamps what it stores with
	// and runs the lifecycle on (app.Config.SetClock).
	clock contracts.Clock
	// root, when set, is the storage root to serve, rather than a fresh
	// one.
	root string
	// notifyTiming is the event deliverer's timing
	// (app.Config.SetNotificationTiming).
	notifyTiming notify.Settings
}

// startLive starts the app on a fresh storage root and a free port, with
// env setting extra TULE_ variables over the test/test credentials, waits
// until it is ready, and stops it when the test ends.
func startLive(t *testing.T, env map[string]string) *liveServer {
	t.Helper()
	return startLiveWith(t, env, liveOptions{})
}

// startLiveWith is startLive with opts.
func startLiveWith(t *testing.T, env map[string]string, opts liveOptions) *liveServer {
	t.Helper()

	port := freePort(t)
	root := opts.root
	if root == "" {
		root = t.TempDir()
	}
	t.Setenv("TULE_GasEnv", "testing")
	t.Setenv("TULE_Storage_RootDir", root)
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
	if opts.clock != nil {
		cfg.SetClock(opts.clock)
	}
	cfg.SetNotificationTiming(opts.notifyTiming)
	a, err := app.New(cfg)
	requireNoErr(t, err, "new app")
	requireNoErr(t, a.Start(context.Background()), "start app")

	served := make(chan error, 1)
	go func() { served <- a.Serve() }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			if sErr := a.Stop(context.Background()); sErr != nil {
				t.Errorf("stop: %v", sErr)
			}
			if sErr := <-served; sErr != nil {
				t.Errorf("serve: %v", sErr)
			}
		})
	}
	t.Cleanup(stop)

	addr := "127.0.0.1:" + strconv.Itoa(port)
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	l := &liveServer{
		URL: "http://" + addr, Addr: addr, Root: root, stop: stop,
		httpClient: &http.Client{Transport: tr},
	}
	l.waitReady(t, served)
	return l
}

// startLiveWithIdentity starts the app with the root key, accepting the
// keys the identity service at identityURL issued.
func startLiveWithIdentity(t *testing.T, identityURL string) *liveServer {
	t.Helper()
	return startLive(t, identityEnv(identityURL))
}

// identityEnv is the environment of startLiveWithIdentity.
func identityEnv(identityURL string) map[string]string {
	return map[string]string{
		"TULE_Auth_AccessKeyID":      rootKeyID,
		"TULE_Auth_SecretAccessKey":  rootSecret,
		"TULE_Auth_IdentityEndpoint": identityURL,
	}
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
