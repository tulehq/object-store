package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	awshttp "github.com/aws/smithy-go/transport/http"
	"github.com/gasmod/gas/config"
	"tule.dev/identity/keysource"
	"tule.dev/sigv4"

	"tule.dev/object-store/internal/authz"
)

//nolint:gosec // G101: test keys
const (
	testRootID     = "AKIAIOSFODNN7EXAMPLE"
	testRootSecret = "root-secret"
	testUserID     = "AKIAUSERKEYEXAMPLE01"
	testUserSecret = "user-secret"
	testUserARN    = "arn:aws:iam::000000000000:user/dev"
)

// fakeIdentity is a signing-key endpoint that knows one IAM user key, with
// the policies set on it.
type fakeIdentity struct {
	policies []json.RawMessage
	mu       sync.Mutex
}

func (f *fakeIdentity) setPolicies(docs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies = nil
	for _, d := range docs {
		f.policies = append(f.policies, json.RawMessage(d))
	}
}

func (f *fakeIdentity) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req keysource.Request
	if r.URL.Path != keysource.Path || json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.AccessKeyID != testUserID {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(keysource.ErrorResponse{
			Code: keysource.CodeUnknownAccessKey, Message: "unknown",
		})
		return
	}
	f.mu.Lock()
	policies := f.policies
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(keysource.Response{
		SigningKey: sigv4.DeriveSigningKey(testUserSecret, req.Date, req.Region, req.Service),
		Principal: keysource.Principal{
			AccountID: "000000000000", ARN: testUserARN, PrincipalARN: testUserARN,
			UserID: "AIDAUSEREXAMPLE00001", PrincipalType: "User", AccessKeyID: testUserID,
			Policies: policies,
		},
		Version: keysource.Version,
	})
}

// startAuthzApp runs the app with an identity endpoint served by f, and
// returns S3 clients for the root key and the IAM user dev's key.
func startAuthzApp(t *testing.T, f *fakeIdentity) (root, user *s3.Client) {
	t.Helper()
	root, user, _ = startAuthzServer(t, f)
	return root, user
}

// startAuthzServer is startAuthzApp, also returning the server's URL.
func startAuthzServer(t *testing.T, f *fakeIdentity) (root, user *s3.Client, url string) {
	t.Helper()
	identity := httptest.NewServer(f)
	t.Cleanup(identity.Close)

	t.Setenv("TULE_GasEnv", "testing")
	t.Setenv("TULE_Storage_RootDir", t.TempDir())
	t.Setenv("TULE_Auth_AccessKeyID", testRootID)
	t.Setenv("TULE_Auth_SecretAccessKey", testRootSecret)
	t.Setenv("TULE_Auth_IdentityEndpoint", identity.URL)
	cfg, err := LoadConfigFrom(config.NewEnvProvider(
		config.WithEnvSeparator("_"), config.WithEnvPrefix("TULE_")))
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
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

	client := func(id, secret string) *s3.Client {
		return s3.New(s3.Options{
			Region:           "us-east-1",
			BaseEndpoint:     aws.String(srv.URL),
			UsePathStyle:     true,
			Credentials:      credentials.NewStaticCredentialsProvider(id, secret, ""),
			RetryMaxAttempts: 1,
		})
	}
	return client(testRootID, testRootSecret), client(testUserID, testUserSecret), srv.URL
}

// call makes one request of an operation, returning its error. For
// DeleteObjects, a key's error entry is returned as the error.
type call func(ctx context.Context, c *s3.Client) error

var (
	b   = aws.String("bkt")
	key = aws.String("k")
)

// calls makes a request of every routed operation, on the bucket bkt and the
// object k that setup creates.
var calls = map[authz.Operation]call{
	authz.ListBuckets: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.ListBuckets(ctx, &s3.ListBucketsInput{}))
	},
	authz.CreateBucket: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("new-bkt")}))
	},
	authz.HeadBucket: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: b}))
	},
	authz.DeleteBucket: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("gone-bkt")}))
	},
	authz.GetBucketLocation: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: b}))
	},
	authz.GetBucketVersioning: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: b}))
	},
	authz.ListObjects: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.ListObjects(ctx, &s3.ListObjectsInput{Bucket: b}))
	},
	authz.ListObjectsV2: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: b}))
	},
	authz.ListMultipartUploads: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: b}))
	},
	authz.DeleteObjects: func(ctx context.Context, c *s3.Client) error {
		out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: b, Delete: &s3types.Delete{
				Objects: []s3types.ObjectIdentifier{{Key: aws.String("del-many")}},
			},
		})
		if err != nil {
			return sdkErr(out, err)
		}
		if len(out.Errors) > 0 {
			return &smithy.GenericAPIError{
				Code:    aws.ToString(out.Errors[0].Code),
				Message: aws.ToString(out.Errors[0].Message),
			}
		}
		return nil
	},
	authz.GetObject: func(ctx context.Context, c *s3.Client) error {
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: b, Key: key})
		if err == nil {
			_ = out.Body.Close()
		}
		return sdkErr(out, err)
	},
	authz.HeadObject: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: b, Key: key}))
	},
	authz.PutObject: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.PutObject(ctx, &s3.PutObjectInput{
			Bucket: b, Key: aws.String("put"), Body: strings.NewReader("x"),
		}))
	},
	authz.CopyObject: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket: b, Key: aws.String("copy"), CopySource: aws.String("bkt/k"),
		}))
	},
	authz.DeleteObject: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: b, Key: aws.String("del")}))
	},
	authz.CreateMultipartUpload: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(
			c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: b, Key: key}),
		)
	},
	authz.UploadPart: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: b, Key: key, UploadId: aws.String("none"), PartNumber: aws.Int32(1),
			Body: strings.NewReader("x"),
		}))
	},
	authz.CompleteMultipartUpload: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: b, Key: key, UploadId: aws.String("none"),
			MultipartUpload: &s3types.CompletedMultipartUpload{
				Parts: []s3types.CompletedPart{{PartNumber: aws.Int32(1), ETag: aws.String("e")}},
			},
		}))
	},
	authz.AbortMultipartUpload: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: b, Key: key, UploadId: aws.String("none"),
		}))
	},
	authz.ListParts: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.ListParts(ctx, &s3.ListPartsInput{
			Bucket: b, Key: key, UploadId: aws.String("none"),
		}))
	},
	// The policy operations act on pol-bkt, so the policy they store
	// applies to no other operation's request.
	authz.PutBucketPolicy: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
			Bucket: aws.String("pol-bkt"),
			Policy: aws.String(`{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
				`"Principal":{"AWS":"111111111111"},"Action":"s3:GetObject",` +
				`"Resource":"arn:aws:s3:::pol-bkt/*"}}`),
		}))
	},
	authz.GetBucketPolicy: func(ctx context.Context, c *s3.Client) error {
		_, err := c.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String("pol-bkt")})
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucketPolicy" {
			return nil
		}
		return sdkErr(nil, err)
	},
	authz.DeleteBucketPolicy: func(ctx context.Context, c *s3.Client) error {
		return sdkErr(c.DeleteBucketPolicy(ctx,
			&s3.DeleteBucketPolicyInput{Bucket: aws.String("pol-bkt")}))
	},
}

// sdkErr returns an SDK call's error, wrapped; errors.As still finds its
// API and response errors.
func sdkErr(_ any, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("sdk: %w", err)
}

func setup(t *testing.T, root *s3.Client) {
	t.Helper()
	ctx := t.Context()
	for _, name := range []string{"bkt", "gone-bkt", "pol-bkt"} {
		if _, err := root.CreateBucket(
			ctx,
			&s3.CreateBucketInput{Bucket: aws.String(name)},
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := root.PutObject(ctx, &s3.PutObjectInput{
		Bucket: b, Key: key, Body: strings.NewReader("data"),
	}); err != nil {
		t.Fatal(err)
	}
}

// isAccessDenied reports whether err is a 403 AccessDenied, or, for a HEAD,
// which has no body, a 403.
func isAccessDenied(err error) (bool, string) {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "AccessDenied" {
		return true, apiErr.ErrorMessage()
	}
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusForbidden {
		return true, ""
	}
	return false, ""
}

// requireCallsCoverEntries checks that calls drives exactly the operations
// with an entry.
func requireCallsCoverEntries(t *testing.T, ops []authz.Operation) {
	t.Helper()
	for op := range calls {
		if !slices.Contains(ops, op) {
			t.Errorf("the test drives %s, which has no entry", op)
		}
	}
	for _, op := range ops {
		if calls[op] == nil {
			t.Errorf("%s has an entry but no request here", op)
		}
	}
}

// denialPrefix is how the AccessDenied message of op, denied action, starts.
// DeleteObjects answers a denied key with S3's plain "Access Denied", as the
// DeleteObjects API reference shows it.
func denialPrefix(op authz.Operation, action string) string {
	if op == authz.DeleteObjects {
		return "Access Denied"
	}
	return "User: " + testUserARN + " is not authorized to perform: " + action
}

// TestEveryRoutedOperationIsAuthorized drives a request of every operation
// in the mapping through its handler: denied, naming its first action, to a
// key with no policy; past authorization with a policy allowing exactly its
// actions; and allowed to the root key.
func TestEveryRoutedOperationIsAuthorized(t *testing.T) {
	ops := authz.Operations()
	requireCallsCoverEntries(t, ops)

	f := &fakeIdentity{}
	root, user := startAuthzApp(t, f)
	setup(t, root)

	for _, op := range ops {
		t.Run(string(op), func(t *testing.T) {
			actions := authz.Actions(op)
			f.setPolicies()
			denied, msg := isAccessDenied(calls[op](t.Context(), user))
			if !denied {
				t.Fatal("a key with no policy was not denied")
			}
			want := denialPrefix(op, actions[0])
			if msg != "" && !strings.HasPrefix(msg, want) {
				t.Fatalf("message %q, want it to start %q", msg, want)
			}

			doc := `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":["` +
				strings.Join(actions, `","`) + `"],"Resource":"*"}}`
			f.setPolicies(doc)
			if denied, msg = isAccessDenied(calls[op](t.Context(), user)); denied {
				t.Fatalf("denied with %v allowed: %s", actions, msg)
			}
			if denied, msg = isAccessDenied(calls[op](t.Context(), root)); denied {
				t.Fatalf("root denied: %s", msg)
			}
		})
	}
}
