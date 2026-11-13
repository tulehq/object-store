package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// A request authorized by a bucket's policy does not act on a bucket
// deleted and created again under its name while the request was in
// flight. dev is granted s3:PutBucketPolicy by race's policy alone and
// sends its policy slowly: the server asks for the body (100 Continue)
// only once the request is authorized, and race is deleted and created
// again before the body is sent. The new race must not get the policy.
func TestARecreatedBucketIsNotActedOnWithTheOldGrant(t *testing.T) {
	f := &fakeIdentity{}
	root, _, url := startAuthzServer(t, f)
	ctx := t.Context()
	race := aws.String("race")
	if _, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: race}); err != nil {
		t.Fatal(err)
	}
	if _, err := root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: race,
		Policy: aws.String(`{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
			`"Principal":{"AWS":"` + testUserARN + `"},"Action":"s3:PutBucketPolicy",` +
			`"Resource":"arn:aws:s3:::race"}}`),
	}); err != nil {
		t.Fatal(err)
	}

	public := `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Principal":"*",` +
		`"Action":"s3:GetObject","Resource":"arn:aws:s3:::race/*"}}`
	authorized, send, result := slowPut(t, url+"/race?policy", public)

	select {
	case <-authorized:
	case r := <-result:
		t.Fatalf("answered before reading the body: %+v", r)
	}
	if _, err := root.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: race}); err != nil {
		t.Fatal(err)
	}
	if _, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: race}); err != nil {
		t.Fatal(err)
	}
	send()

	r := <-result
	if r.err != nil || r.status != http.StatusNotFound ||
		!strings.Contains(r.body, "NoSuchBucket") {
		t.Fatalf("got %+v, want 404 NoSuchBucket", r)
	}
	_, err := root.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: race})
	requireStatus(t, err, http.StatusNotFound, "NoSuchBucketPolicy")
}

type slowResult struct {
	err    error
	body   string
	status int
}

// slowPut sends a PUT of body to url signed with dev's key as
// UNSIGNED-PAYLOAD, with Expect: 100-continue. authorized is closed when
// the server asks for the body, which it does once it has authorized the
// request; the body is sent only when send is called. result receives the
// response.
func slowPut(
	t *testing.T,
	url, body string,
) (authorized <-chan struct{}, send func(), result <-chan slowResult) {
	t.Helper()
	pr, pw := io.Pipe()
	asked := make(chan struct{})
	trace := &httptrace.ClientTrace{Got100Continue: func() { close(asked) }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace),
		http.MethodPut, url, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Expect", "100-continue")
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	if err = v4.NewSigner().SignHTTP(context.Background(),
		aws.Credentials{AccessKeyID: testUserID, SecretAccessKey: testUserSecret},
		req, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{ExpectContinueTimeout: time.Minute}}
	out := make(chan slowResult, 1)
	go func() {
		res, dErr := client.Do(req)
		if dErr != nil {
			out <- slowResult{err: dErr}
			return
		}
		defer func() { _ = res.Body.Close() }()
		b, rErr := io.ReadAll(res.Body)
		out <- slowResult{err: rErr, body: string(b), status: res.StatusCode}
	}()
	send = func() {
		go func() {
			_, _ = io.Copy(pw, strings.NewReader(body))
			_ = pw.Close()
		}()
	}
	return asked, send, out
}

// A write in flight holds its bucket: DeleteBucket waits for it, without
// blocking the bucket's other requests meanwhile, so the object lands in
// the bucket that was authorized and the delete then finds it not empty,
// rather than the object landing in a bucket created again.
func TestDeleteBucketWaitsForAWriteInFlight(t *testing.T) {
	f := &fakeIdentity{}
	root, _, url := startAuthzServer(t, f)
	ctx := t.Context()
	race := aws.String("race")
	if _, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: race}); err != nil {
		t.Fatal(err)
	}
	f.setPolicies(`{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
		`"Action":"s3:PutObject","Resource":"arn:aws:s3:::race/*"}}`)
	authorized, send, result := slowPut(t, url+"/race/k", "data")
	<-authorized

	deleted := make(chan error, 1)
	go func() {
		_, err := root.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: race})
		deleted <- err
	}()
	select {
	case err := <-deleted:
		t.Fatalf("DeleteBucket did not wait for the write: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// The waiting delete registers no writer, so the bucket still answers.
	headed := make(chan error, 1)
	go func() {
		_, err := root.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: race})
		headed <- err
	}()
	select {
	case err := <-headed:
		if err != nil {
			t.Fatalf("HeadBucket while the delete waits: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("HeadBucket blocked behind the waiting delete")
	}
	send()
	if r := <-result; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("put: %+v", r)
	}
	requireStatus(t, <-deleted, http.StatusConflict, "BucketNotEmpty")
}
