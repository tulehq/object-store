//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// The notification tests run object-store against real queues and
// functions services, built from their checkouts and run as processes,
// as secret-store's rotation tests run functions. The functions service
// runs on the local Docker daemon. These tests only validate destinations,
// which never runs a function (a dry run), so the function image is built
// FROM scratch around a single file, and nothing is pulled.

// The directories of the queues and functions checkouts to build, for a
// layout where they are not object-store's siblings.
const (
	queuesDirEnv    = "TULE_E2E_QUEUES_DIR"
	functionsDirEnv = "TULE_E2E_FUNCTIONS_DIR"
)

// account is the account every service here serves, object-store's
// default TULE_Auth_AccountID and queues' and functions' default account.
const account = "000000000000"

// runID tells this run's image apart from any other run's.
var runID = strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())

// functionImage is the image the functions here are created from.
var functionImage = "tule-object-store-e2e/function:" + runID

// functionImageBuild builds the image once, for the first test that needs
// it.
var functionImageBuild struct {
	err  error
	once sync.Once
	done bool
}

func TestMain(m *testing.M) {
	code := m.Run()
	if functionImageBuild.done {
		removeFunctionImage()
	}
	os.Exit(code)
}

// requireFunctionImage builds the function image, once.
func requireFunctionImage(t *testing.T) {
	t.Helper()
	functionImageBuild.once.Do(func() {
		functionImageBuild.done = true
		functionImageBuild.err = buildFunctionImage()
	})
	requireNoErr(t, functionImageBuild.err, "build the function image")
}

// buildFunctionImage builds an image FROM scratch holding one file, never
// pulling. The functions service only asks Docker that the image exists
// before it makes the function Active, and a dry run never starts it.
func buildFunctionImage() error {
	dir, err := os.MkdirTemp("", "tule-object-store-e2e-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err = os.WriteFile(filepath.Join(dir, "README"),
		[]byte("A function that is only ever dry-run.\n"), 0o600); err != nil {
		return fmt.Errorf("README: %w", err)
	}
	dockerfile := "FROM scratch\nCOPY README /README\n"
	if err = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		return fmt.Errorf("Dockerfile: %w", err)
	}
	_, err = docker(context.Background(), "build", "--pull=false", "-q",
		"--label", "dev.tule.e2e.run="+runID, "-t", functionImage, dir)
	return err
}

// removeFunctionImage removes the image, and any container left from it.
func removeFunctionImage() {
	ctx := context.Background()
	ids, _ := docker(ctx, "ps", "-a", "-q", "--filter", "ancestor="+functionImage)
	if f := strings.Fields(ids); len(f) > 0 {
		fmt.Fprintf(os.Stderr, "e2e: removing %d containers left from %s\n", len(f), functionImage)
		_, _ = docker(ctx, append([]string{"rm", "-f"}, f...)...)
	}
	_, _ = docker(ctx, "rmi", "-f", functionImage)
}

// docker runs the docker CLI against the local daemon and returns its
// standard output.
func docker(ctx context.Context, args ...string) (string, error) {
	//nolint:gosec // G204: the docker CLI, with this suite's own arguments
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}

// notificationStack is queues, functions and object-store wired together
// through TULE_Storage_QueuesEndpoint and TULE_Storage_FunctionsEndpoint,
// all on the root key, with clients signed with it.
type notificationStack struct {
	srv       *liveServer
	s3        *s3.Client
	sqs       *sqs.Client
	lambda    *lambda.Client
	functions *sibling
}

// startNotificationStack starts queues and functions standalone, and
// object-store pointed at both.
func startNotificationStack(t *testing.T) *notificationStack {
	t.Helper()
	queues := startSibling(t, "queues", queuesDirEnv)
	functions := startSibling(t, "functions", functionsDirEnv,
		"TULE_Functions_RuntimeAPIPort=0")
	srv := startLive(t, map[string]string{
		"TULE_Auth_AccessKeyID":          rootKeyID,
		"TULE_Auth_SecretAccessKey":      rootSecret,
		"TULE_Storage_QueuesEndpoint":    queues.URL,
		"TULE_Storage_FunctionsEndpoint": functions.URL,
	})
	creds := credentials.StaticCredentialsProvider{Value: rootCreds()}
	return &notificationStack{
		srv: srv, s3: srv.client(rootCreds()), functions: functions,
		sqs: sqs.New(sqs.Options{
			Region: "us-east-1", BaseEndpoint: aws.String(queues.URL), Credentials: creds,
			HTTPClient: &http.Client{Timeout: serviceTimeout}, RetryMaxAttempts: 1,
		}),
		lambda: lambda.New(lambda.Options{
			Region: "us-east-1", BaseEndpoint: aws.String(functions.URL), Credentials: creds,
			HTTPClient: &http.Client{Timeout: serviceTimeout}, RetryMaxAttempts: 1,
		}),
	}
}

// queueARN is the ARN of the queue name.
func queueARN(name string) string {
	return "arn:aws:sqs:us-east-1:" + account + ":" + name
}

// functionARN is the ARN of the function name.
func functionARN(name string) string {
	return "arn:aws:lambda:us-east-1:" + account + ":function:" + name
}

// s3QueuePolicy is the queue policy the S3 User Guide gives ("Granting
// permissions to publish event notification messages to a destination"),
// for queue and bucket.
func s3QueuePolicy(queue, bucket string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
		`"Principal":{"Service":"s3.amazonaws.com"},"Action":["SQS:SendMessage"],` +
		`"Resource":"` + queueARN(queue) + `","Condition":{` +
		`"ArnLike":{"aws:SourceArn":"arn:aws:s3:*:*:` + bucket + `"},` +
		`"StringEquals":{"aws:SourceAccount":"` + account + `"}}}]}`
}

// createQueue creates the queue name, with policy when it is set, and
// returns its URL.
func (s *notificationStack) createQueue(t *testing.T, name, policy string) string {
	t.Helper()
	in := &sqs.CreateQueueInput{QueueName: aws.String(name)}
	if policy != "" {
		in.Attributes = map[string]string{string(sqstypes.QueueAttributeNamePolicy): policy}
	}
	out, err := s.sqs.CreateQueue(t.Context(), in)
	requireNoErr(t, err, "create queue "+name)
	return aws.ToString(out.QueueUrl)
}

// receiveAll returns the messages in the queue at queueURL, waiting up to
// a second for the first.
func (s *notificationStack) receiveAll(t *testing.T, queueURL string) []sqstypes.Message {
	t.Helper()
	out, err := s.sqs.ReceiveMessage(t.Context(), &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
		MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{
			sqstypes.MessageSystemAttributeNameAll,
		},
	})
	requireNoErr(t, err, "receive from "+queueURL)
	return out.Messages
}

// createFunction creates the function name from the function image and
// waits until it is Active.
func (s *notificationStack) createFunction(t *testing.T, name string) {
	t.Helper()
	requireFunctionImage(t)
	arch := lambdatypes.ArchitectureX8664
	if runtime.GOARCH == "arm64" {
		arch = lambdatypes.ArchitectureArm64
	}
	_, err := s.lambda.CreateFunction(t.Context(), &lambda.CreateFunctionInput{
		FunctionName:  aws.String(name),
		Role:          aws.String("arn:aws:iam::" + account + ":role/function"),
		PackageType:   lambdatypes.PackageTypeImage,
		Code:          &lambdatypes.FunctionCode{ImageUri: aws.String(functionImage)},
		Architectures: []lambdatypes.Architecture{arch},
	})
	requireNoErr(t, err, "create function "+name)
	w := lambda.NewFunctionActiveV2Waiter(s.lambda, func(o *lambda.FunctionActiveV2WaiterOptions) {
		o.MinDelay, o.MaxDelay = 100*time.Millisecond, time.Second
	})
	requireNoErr(t, w.Wait(t.Context(), &lambda.GetFunctionInput{FunctionName: aws.String(name)},
		serviceTimeout), "wait for "+name+" to be Active")
}

// createFailedFunction creates the function name from an image Docker
// does not have, which the functions service makes Failed, and waits
// until it is.
func (s *notificationStack) createFailedFunction(t *testing.T, name string) {
	t.Helper()
	_, err := s.lambda.CreateFunction(t.Context(), &lambda.CreateFunctionInput{
		FunctionName: aws.String(name),
		Role:         aws.String("arn:aws:iam::" + account + ":role/function"),
		PackageType:  lambdatypes.PackageTypeImage,
		Code: &lambdatypes.FunctionCode{
			ImageUri: aws.String("tule-object-store-e2e/missing:" + runID),
		},
	})
	requireNoErr(t, err, "create function "+name)
	deadline := time.Now().Add(serviceTimeout)
	for {
		out, gErr := s.lambda.GetFunction(t.Context(),
			&lambda.GetFunctionInput{FunctionName: aws.String(name)})
		requireNoErr(t, gErr, "get function "+name)
		if out.Configuration.State == lambdatypes.StateFailed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is %s, not Failed, after %v", name, out.Configuration.State,
				serviceTimeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// allowS3 adds the permission the S3 User Guide's Lambda destinations
// need: lambda:InvokeFunction for s3.amazonaws.com from bucket and its
// owner's account, as `aws lambda add-permission --principal
// s3.amazonaws.com --source-arn ... --source-account ...` adds it.
func (s *notificationStack) allowS3(t *testing.T, function, bucket string) {
	t.Helper()
	_, err := s.lambda.AddPermission(t.Context(), &lambda.AddPermissionInput{
		FunctionName:  aws.String(function),
		Action:        aws.String("lambda:InvokeFunction"),
		StatementId:   aws.String("S3"),
		Principal:     aws.String("s3.amazonaws.com"),
		SourceArn:     aws.String("arn:aws:s3:::" + bucket),
		SourceAccount: aws.String(account),
	})
	requireNoErr(t, err, "allow S3 to invoke "+function)
}

// invocations returns how many invocations of function the functions
// service's retrieval API holds, read with the root key.
func (s *notificationStack) invocations(t *testing.T, function string) int {
	t.Helper()
	u := s.functions.URL + "/_functions/invocations?" + url.Values{
		"function": {function}, "limit": {"100"},
	}.Encode()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, http.NoBody)
	requireNoErr(t, err, "build the invocations request")
	sum := sha256.Sum256(nil)
	requireNoErr(t, v4.NewSigner().SignHTTP(t.Context(), rootCreds(), req,
		hex.EncodeToString(sum[:]), "lambda", "us-east-1", time.Now()), "sign the request")
	res, err := (&http.Client{Timeout: serviceTimeout}).Do(req)
	requireNoErr(t, err, "GET the invocations")
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	requireNoErr(t, err, "read the invocations")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET the invocations: %d %s", res.StatusCode, body)
	}
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	requireNoErr(t, json.Unmarshal(body, &page), "decode the invocations")
	return len(page.Items)
}

// notifyBucket is the bucket the notification tests configure.
const notifyBucket = "photos"

// notificationRequest sends a request of method to notifyBucket's
// ?notification on l, with body, signed with creds and the headers in
// header, which are signed too, and returns the response.
func notificationRequest(
	t *testing.T,
	l *liveServer,
	creds aws.Credentials,
	method, body string,
	header map[string]string,
) *liveResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method,
		l.URL+"/"+notifyBucket+"?notification", strings.NewReader(body))
	requireNoErr(t, err, "build request")
	sum := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", hash)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	err = v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }).
		SignHTTP(t.Context(), creds, req, hash, "s3", "us-east-1", time.Now())
	requireNoErr(t, err, "sign request")
	return sendLive(t, req)
}

// argumentError is an S3 error document, with the first two numbered
// arguments of an InvalidArgument about destinations.
type argumentError struct {
	Code           string
	Message        string
	ArgumentName1  string
	ArgumentValue1 string
	ArgumentName2  string
	ArgumentValue2 string
}

// requireBadRequest fails unless res is a 400 with an error document of
// code and message, and returns the document.
func requireBadRequest(t *testing.T, res *liveResponse, code, message string) argumentError {
	t.Helper()
	var e argumentError
	if res.StatusCode != http.StatusBadRequest || xml.Unmarshal([]byte(res.Body), &e) != nil ||
		e.Code != code || e.Message != message {
		t.Fatalf("got %d %s, want 400 %s %q", res.StatusCode, res.Body, code, message)
	}
	return e
}
