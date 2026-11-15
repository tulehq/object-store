package app

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/gasmod/gas"
	"github.com/gasmod/gas/config"
)

// warnLog records the Warn events a recLogger sends, each as its message
// followed by its fields as key=value.
type warnLog struct {
	warns []string
	mu    sync.Mutex
}

func (l *warnLog) add(w string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, w)
}

// all returns the recorded Warn events.
func (l *warnLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.warns...)
}

// count returns how many recorded Warn events hold every one of parts.
func (l *warnLog) count(parts ...string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, w := range l.warns {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(w, p)
		}
		if all {
			n++
		}
	}
	return n
}

// recLogger is a gas.Logger that records its Warn events, its own and
// those of the loggers derived from it, and drops every other.
type recLogger struct {
	gas.NopLogger
	log *warnLog
}

func (l *recLogger) Warn(msg string) gas.LogEvent { return &recEvent{log: l.log, line: msg} }

func (l *recLogger) With() gas.LoggerContext { return &recContext{l: l} }

// recContext derives a recLogger that records into the same log; the
// fields given are dropped.
type recContext struct{ l *recLogger }

func (c *recContext) Str(string, string) gas.LoggerContext             { return c }
func (c *recContext) Int(string, int) gas.LoggerContext                { return c }
func (c *recContext) Int64(string, int64) gas.LoggerContext            { return c }
func (c *recContext) Float64(string, float64) gas.LoggerContext        { return c }
func (c *recContext) Bool(string, bool) gas.LoggerContext              { return c }
func (c *recContext) Err(string, error) gas.LoggerContext              { return c }
func (c *recContext) Duration(string, time.Duration) gas.LoggerContext { return c }
func (c *recContext) Any(string, any) gas.LoggerContext                { return c }
func (c *recContext) Logger() gas.Logger                               { return c.l }

// recEvent is a Warn event of a recLogger, recorded when it is sent with
// its string and error fields.
type recEvent struct {
	log  *warnLog
	line string
}

func (e *recEvent) Str(k, v string) gas.LogEvent {
	e.line += " " + k + "=" + v
	return e
}

func (e *recEvent) Err(k string, v error) gas.LogEvent {
	e.line += " " + k + "=" + v.Error()
	return e
}

func (e *recEvent) Int(string, int) gas.LogEvent                { return e }
func (e *recEvent) Int64(string, int64) gas.LogEvent            { return e }
func (e *recEvent) Float64(string, float64) gas.LogEvent        { return e }
func (e *recEvent) Bool(string, bool) gas.LogEvent              { return e }
func (e *recEvent) Duration(string, time.Duration) gas.LogEvent { return e }
func (e *recEvent) Any(string, any) gas.LogEvent                { return e }
func (e *recEvent) Send()                                       { e.log.add(e.line) }

// startLoggedApp starts the app on a fresh storage root, with the static
// root key and every service logging to a recLogger, and returns a client
// of the root key, the storage root and the log of Warn events.
func startLoggedApp(t *testing.T) (*s3.Client, string, *warnLog) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("TULE_GasEnv", "testing")
	t.Setenv("TULE_Storage_RootDir", root)
	t.Setenv("TULE_Auth_AccessKeyID", testRootID)
	t.Setenv("TULE_Auth_SecretAccessKey", testRootSecret)
	cfg, err := LoadConfigFrom(config.NewEnvProvider(
		config.WithEnvSeparator("_"), config.WithEnvPrefix("TULE_")))
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	log := &warnLog{}
	a.RegisterServiceInstance[gas.Logger](&recLogger{log: log})
	if err = a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(func() {
		srv.Close()
		if sErr := a.Shutdown(context.Background()); sErr != nil {
			t.Errorf("shutdown: %v", sErr)
		}
	})
	return s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(
			testRootID, testRootSecret, ""),
		RetryMaxAttempts: 1,
	}), root, log
}

// damageLifecycle creates bucket with a lifecycle configuration that
// expires its objects and aborts its uploads, then overwrites the stored
// configuration under root with bytes that do not decode.
func damageLifecycle(t *testing.T, c *s3.Client, root, bucket string) {
	t.Helper()
	ctx := t.Context()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(bucket),
		LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{
			Rules: []s3types.LifecycleRule{{
				ID: aws.String("all"), Status: s3types.ExpirationStatusEnabled,
				Filter:     &s3types.LifecycleRuleFilter{},
				Expiration: &s3types.LifecycleExpiration{Days: aws.Int32(1)},
				AbortIncompleteMultipartUpload: &s3types.AbortIncompleteMultipartUpload{
					DaysAfterInitiation: aws.Int32(1),
				},
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, ".tule", "config", "lifecycle", bucket)
	if err := os.WriteFile(file, []byte("{damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireNoLifecycleHeader fails unless err is nil and the response has
// no lifecycle header, which none reports as true.
func requireNoLifecycleHeader(t *testing.T, op string, none bool, err error) {
	t.Helper()
	if err != nil || !none {
		t.Fatalf("%s: error %v, lifecycle headers left out %t; want no error and none",
			op, err, none)
	}
}

// A lifecycle configuration that cannot be decoded fails none of its
// bucket's object operations: each is answered without its lifecycle
// headers, and the damage is logged at Warn with the bucket.
func TestADamagedLifecycleConfigurationIsLoggedAndLeftOut(t *testing.T) {
	c, root, log := startLoggedApp(t)
	ctx := t.Context()
	damageLifecycle(t, c, root, "dmg")
	bucket, obj := aws.String("dmg"), aws.String("logs/a")

	put, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: obj, Body: strings.NewReader("a"),
	})
	requireNoLifecycleHeader(t, "PutObject", err == nil && put.Expiration == nil, err)
	get, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: obj})
	requireNoLifecycleHeader(t, "GetObject", err == nil && get.Expiration == nil, err)
	if cErr := get.Body.Close(); cErr != nil {
		t.Fatal(cErr)
	}
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: obj})
	requireNoLifecycleHeader(t, "HeadObject", err == nil && head.Expiration == nil, err)
	up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: bucket, Key: obj,
	})
	requireNoLifecycleHeader(t, "CreateMultipartUpload",
		err == nil && up.AbortDate == nil && up.AbortRuleId == nil, err)
	parts, err := c.ListParts(ctx, &s3.ListPartsInput{
		Bucket: bucket, Key: obj, UploadId: up.UploadId,
	})
	requireNoLifecycleHeader(t, "ListParts",
		err == nil && parts.AbortDate == nil && parts.AbortRuleId == nil, err)

	const msg = "left out the lifecycle headers"
	if n := log.count(msg, "bucket=dmg", "error="); n != 5 {
		t.Fatalf("%d Warn events %q for bucket dmg, want one per request, 5; log %q",
			n, msg, log.all())
	}
}
