package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	awshttp "github.com/aws/smithy-go/transport/http"
	"github.com/gasmod/gas/config"
)

func requireStatus(t *testing.T, err error, status int, code string) {
	t.Helper()
	var respErr *awshttp.ResponseError
	if !errors.As(err, &respErr) || respErr.HTTPStatusCode() != status {
		t.Fatalf("got %v, want status %d", err, status)
	}
	var apiErr smithy.APIError
	if code != "" && (!errors.As(err, &apiErr) || apiErr.ErrorCode() != code) {
		t.Fatalf("got %v, want %s", err, code)
	}
}

// A read of a missing key answers 404 only to a caller allowed s3:ListBucket
// on the bucket, and 403 otherwise; a HEAD's 403 has no body.
func TestAMissingKeyIsHiddenWithoutListBucket(t *testing.T) {
	f := &fakeIdentity{}
	root, user := startAuthzApp(t, f)
	setup(t, root)
	ctx := t.Context()
	get := &s3.GetObjectInput{Bucket: b, Key: aws.String("missing")}
	head := &s3.HeadObjectInput{Bucket: b, Key: aws.String("missing")}
	copyIn := &s3.CopyObjectInput{
		Bucket: b, Key: aws.String("dst"), CopySource: aws.String("bkt/missing"),
	}

	f.setPolicies(`{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
		`"Action":["s3:GetObject","s3:PutObject"],"Resource":"*"}}`)
	_, err := user.GetObject(ctx, get)
	requireStatus(t, err, http.StatusForbidden, "AccessDenied")
	_, err = user.HeadObject(ctx, head)
	requireStatus(t, err, http.StatusForbidden, "")
	_, err = user.CopyObject(ctx, copyIn)
	requireStatus(t, err, http.StatusForbidden, "AccessDenied")

	f.setPolicies(`{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
		`"Action":["s3:GetObject","s3:PutObject","s3:ListBucket"],"Resource":"*"}}`)
	_, err = user.GetObject(ctx, get)
	requireStatus(t, err, http.StatusNotFound, "NoSuchKey")
	_, err = user.HeadObject(ctx, head)
	requireStatus(t, err, http.StatusNotFound, "")
	_, err = user.CopyObject(ctx, copyIn)
	requireStatus(t, err, http.StatusNotFound, "NoSuchKey")

	_, err = root.GetObject(ctx, get)
	requireStatus(t, err, http.StatusNotFound, "NoSuchKey")
}

// Names longer than S3 allows are refused before evaluation, for every
// caller.
func TestOverlongNamesAreRefused(t *testing.T) {
	f := &fakeIdentity{}
	root, user := startAuthzApp(t, f)
	setup(t, root)
	f.setPolicies(`{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"s3:*",` +
		`"Resource":"*"}}`)
	long := aws.String(strings.Repeat("k", 1025))
	for _, c := range []*s3.Client{root, user} {
		_, err := c.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: b, Key: long, Body: strings.NewReader("x"),
		})
		requireStatus(t, err, http.StatusBadRequest, "KeyTooLongError")
		_, err = c.CopyObject(t.Context(), &s3.CopyObjectInput{
			Bucket: b, Key: aws.String("dst"), CopySource: aws.String("bkt/" + *long),
		})
		requireStatus(t, err, http.StatusBadRequest, "KeyTooLongError")
	}
}

// aws:RequestedRegion is the server's region: signing for another region
// does not step around a Deny on us-east-1.
func TestRequestedRegionIgnoresTheSignedRegion(t *testing.T) {
	f := &fakeIdentity{}
	root, user := startAuthzApp(t, f)
	setup(t, root)
	f.setPolicies(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*",` +
		`"Resource":"*"},{"Effect":"Deny","Action":"s3:*","Resource":"*","Condition":` +
		`{"StringEquals":{"aws:RequestedRegion":"us-east-1"}}}]}`)
	_, err := user.GetObject(t.Context(), &s3.GetObjectInput{Bucket: b, Key: key},
		func(o *s3.Options) { o.Region = "eu-west-1" })
	requireStatus(t, err, http.StatusForbidden, "AccessDenied")
}

// signedDo sends a request signed with the IAM user's key, as the SDK
// cannot send these.
func signedDo(t *testing.T, method, url string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(
		t.Context(),
		method,
		url,
		strings.NewReader(string(body)),
	)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", hash)
	err = v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }).SignHTTP(
		context.Background(),
		aws.Credentials{AccessKeyID: testUserID, SecretAccessKey: testUserSecret},
		req, hash, "s3", "us-east-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(out)
}

// Authorization and the handler read a repeated query parameter and the
// DeleteObjects document the same way, from the one parse: the first
// prefix, and one decoded document whose keys are both evaluated and
// deleted.
func TestAuthorizationAndTheHandlerReadOneDecoding(t *testing.T) {
	f := &fakeIdentity{}
	root, _, url := startAuthzServer(t, f)
	setup(t, root)
	for _, k := range []string{"public/a", "secret/a"} {
		if _, err := root.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: b, Key: aws.String(k), Body: strings.NewReader(k),
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.setPolicies(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":` +
		`"s3:ListBucket","Resource":"*","Condition":{"StringLike":{"s3:prefix":"public/*"}}},` +
		`{"Effect":"Allow","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::bkt/public/*"}]}`)

	// The signer sorts a parameter's values, so each pair is sent in the
	// order written.
	status, body := signedDo(t, http.MethodGet,
		url+"/bkt?list-type=2&prefix=public/&prefix=secret/", nil)
	if status != http.StatusOK || strings.Contains(body, "secret/a") ||
		!strings.Contains(body, "public/a") {
		t.Fatalf("listing with prefix public/ then secret/: %d %s", status, body)
	}
	status, _ = signedDo(t, http.MethodGet,
		url+"/bkt?list-type=2&prefix=other/&prefix=public/", nil)
	if status != http.StatusForbidden {
		t.Fatalf("listing with prefix other/ then public/: %d", status)
	}

	// An Object naming two keys is decoded once; whichever key the decoder
	// keeps is the one evaluated and the one deleted.
	doc := `<Delete><Object><Key>public/a</Key><Key>secret/a</Key></Object></Delete>`
	status, body = signedDo(t, http.MethodPost, url+"/bkt?delete", []byte(doc))
	if status != http.StatusOK {
		t.Fatalf("delete: %d %s", status, body)
	}
	_, err := root.HeadObject(
		t.Context(),
		&s3.HeadObjectInput{Bucket: b, Key: aws.String("secret/a")},
	)
	if err != nil {
		t.Fatalf("secret/a was deleted (%s): %v", body, err)
	}
}

// A static key under a configured account other than the default is still
// the root, standalone.
func TestTheStaticKeyIsRootOfTheConfiguredAccount(t *testing.T) {
	t.Setenv("TULE_GasEnv", "testing")
	t.Setenv("TULE_Storage_RootDir", t.TempDir())
	t.Setenv("TULE_Auth_AccessKeyID", testRootID)
	t.Setenv("TULE_Auth_SecretAccessKey", testRootSecret)
	t.Setenv("TULE_Auth_AccountID", "123456789012")
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
	c := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials:      credentials.NewStaticCredentialsProvider(testRootID, testRootSecret, ""),
		RetryMaxAttempts: 1,
	})
	setup(t, c)
	if _, err = c.ListBuckets(t.Context(), &s3.ListBucketsInput{}); err != nil {
		t.Fatal(err)
	}
}
