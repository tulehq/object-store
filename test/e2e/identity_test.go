//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	awsorg "github.com/aws/aws-sdk-go-v2/service/organizations"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// The directories of the identity and accounts checkouts to build, for a
// layout where they are not object-store's siblings.
const (
	identityDirEnv = "TULE_E2E_IDENTITY_DIR"
	accountsDirEnv = "TULE_E2E_ACCOUNTS_DIR"
)

// The root key every service in these tests shares: object-store's
// configured key and identity's and accounts' root user's. It is the AWS
// documentation's example key, as identity's own tests use: identity
// validates an AccessKeyId parameter against the IAM model's
// accessKeyIdType, at least 16 characters, so a short ID such as "test"
// could not be passed to an IAM or STS call that names a key.
//
//nolint:gosec // G101: the AWS documentation's example key, not a credential
const (
	rootKeyID  = "AKIAIOSFODNN7EXAMPLE"
	rootSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

// serviceTimeout bounds a sibling's build, startup and shutdown, and the
// requests made to it, so a hung service fails the test instead of the
// suite's timeout.
const serviceTimeout = 2 * time.Minute

// syncBuffer collects a process's output, safe to write from the process's
// copying goroutines and read from the test.
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p) //nolint:wrapcheck // bytes.Buffer.Write never fails
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// sibling is a Tule service built from its checkout and run as a process.
type sibling struct {
	cmd     *exec.Cmd
	exited  chan error
	out     *syncBuffer
	name    string
	URL     string
	stopped bool
}

// startSibling builds the service name from its checkout, runs it on a
// free port with a fresh store, the root key and env, and waits until it is
// ready. The process is stopped when the test ends, if the test has not
// stopped it.
func startSibling(t *testing.T, name, dirEnv string, env ...string) *sibling {
	t.Helper()

	bin := buildSibling(t, name, dirEnv)
	port := freePort(t)
	dir := t.TempDir()

	cmd := exec.Command(bin) //nolint:gosec // G204: the binary this test just built
	cmd.Dir = dir
	cmd.Env = append(envWithoutTule(),
		"TULE_GasEnv=testing",
		"TULE_Server_Port="+strconv.Itoa(port),
		"TULE_Store_Path="+filepath.Join(dir, name+".db"),
		"TULE_Auth_AccessKeyID="+rootKeyID,
		"TULE_Auth_SecretAccessKey="+rootSecret,
	)
	cmd.Env = append(cmd.Env, env...)
	s := &sibling{
		cmd:    cmd,
		exited: make(chan error, 1),
		out:    &syncBuffer{},
		name:   name,
		URL:    "http://127.0.0.1:" + strconv.Itoa(port),
	}
	cmd.Stdout = s.out
	cmd.Stderr = s.out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go func() { s.exited <- cmd.Wait() }()
	t.Cleanup(func() { s.stop(t) })

	s.waitReady(t)
	return s
}

// buildSibling builds the service name into a temporary directory from the
// checkout dirEnv names, by default object-store's sibling.
func buildSibling(t *testing.T, name, dirEnv string) string {
	t.Helper()

	// The test runs in object-store/test/e2e. CI sets the directory. A
	// missing checkout fails the test, so the cross-service test is never
	// silently left out.
	src, from := os.Getenv(dirEnv), dirEnv
	if src == "" {
		src, from = filepath.Join("..", "..", "..", name), "the default sibling path"
	}
	src, err := filepath.Abs(src)
	requireNoErr(t, err, name+" directory")
	//nolint:gosec // G703: the checkout the test is told to build
	if _, err = os.Stat(filepath.Join(src, "go.mod")); err != nil {
		t.Fatalf("no %s checkout at %s (from %s); set %s to a checkout of %s: %v",
			name, src, from, dirEnv, name, err)
	}

	bin := filepath.Join(t.TempDir(), "tule-"+name)
	ctx, cancel := context.WithTimeout(t.Context(), serviceTimeout)
	defer cancel()
	//nolint:gosec // G204: go build of the checkout into this test's directory
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd")
	build.Dir = src
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, out)
	}
	return bin
}

// envWithoutTule is this process's environment minus the TULE_ variables the
// tests set for object-store, so a sibling gets only its own.
func envWithoutTule() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TULE_") {
			env = append(env, kv)
		}
	}
	return env
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	requireNoErr(t, err, "find a free port")
	port := l.Addr().(*net.TCPAddr).Port
	requireNoErr(t, l.Close(), "release the free port")
	return port
}

// waitReady polls the service's readiness probe until it answers 200.
func (s *sibling) waitReady(t *testing.T) {
	t.Helper()

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(serviceTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-s.exited:
			t.Fatalf("%s exited before it was ready: %v\n%s", s.name, err, s.out.String())
		default:
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL+"/_ready", nil)
		requireNoErr(t, err, "build readiness request")
		if res, err := client.Do(req); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s was not ready within %v\n%s", s.name, serviceTimeout, s.out.String())
}

// stop interrupts the process, as a signal stops the service, and kills it
// if it does not exit in time. Stopping a stopped process does nothing.
func (s *sibling) stop(t *testing.T) {
	t.Helper()
	if s.stopped {
		return
	}
	s.stopped = true

	_ = s.cmd.Process.Signal(os.Interrupt)
	select {
	case err := <-s.exited:
		if err != nil {
			t.Logf("%s exited: %v", s.name, err)
		}
	case <-time.After(serviceTimeout):
		_ = s.cmd.Process.Kill()
		err := <-s.exited
		t.Errorf("%s did not stop on interrupt (%v)\n%s", s.name, err, s.out.String())
	}
}

// startWithIdentity starts object-store with the root key, accepting the keys
// the identity service at identityURL issued.
func startWithIdentity(t *testing.T, identityURL string) *httptest.Server {
	t.Helper()
	return startServer(t, false, map[string]string{
		"TULE_Auth_AccessKeyID":      rootKeyID,
		"TULE_Auth_SecretAccessKey":  rootSecret,
		"TULE_Auth_IdentityEndpoint": identityURL,
	})
}

func rootCreds() aws.Credentials {
	return aws.Credentials{AccessKeyID: rootKeyID, SecretAccessKey: rootSecret}
}

// s3As returns an S3 client for srv signing with creds. It does not retry,
// so a refusal or failure is seen as the server answered it.
func s3As(srv *httptest.Server, creds aws.Credentials) *s3.Client {
	return clientFor(srv, func(o *s3.Options) {
		o.Credentials = credentials.StaticCredentialsProvider{Value: creds}
		o.RetryMaxAttempts = 1
	})
}

func iamClient(url string) *iam.Client {
	return iam.New(iam.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(url),
		Credentials:      credentials.StaticCredentialsProvider{Value: rootCreds()},
		HTTPClient:       &http.Client{Timeout: serviceTimeout},
		RetryMaxAttempts: 1,
	})
}

// createUserKey creates the IAM user name and an access key for it, with
// the root key, and returns the key.
func createUserKey(t *testing.T, ic *iam.Client, name string) aws.Credentials {
	t.Helper()
	_, err := ic.CreateUser(t.Context(), &iam.CreateUserInput{UserName: aws.String(name)})
	requireNoErr(t, err, "create user "+name)
	out, err := ic.CreateAccessKey(t.Context(), &iam.CreateAccessKeyInput{
		UserName: aws.String(name),
	})
	requireNoErr(t, err, "create access key for "+name)
	return aws.Credentials{
		AccessKeyID:     aws.ToString(out.AccessKey.AccessKeyId),
		SecretAccessKey: aws.ToString(out.AccessKey.SecretAccessKey),
	}
}

// s3FullAccess is an identity policy allowing every S3 action, which the
// identity tests that exercise keys rather than permissions give their
// IAM users, since an identity-issued key with no policy is denied.
const s3FullAccess = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
	`"Action":"s3:*","Resource":"*"}]}`

// putUserPolicy sets the inline policy name, doc, on the IAM user, with the
// root key.
func putUserPolicy(t *testing.T, ic *iam.Client, user, name, doc string) {
	t.Helper()
	_, err := ic.PutUserPolicy(t.Context(), &iam.PutUserPolicyInput{
		UserName: aws.String(user), PolicyName: aws.String(name), PolicyDocument: aws.String(doc),
	})
	requireNoErr(t, err, "put policy "+name+" on "+user)
}

// requireWorks checks that c can write, read and presign a read of an
// object in bucket, which it creates.
func requireWorks(t *testing.T, srv *httptest.Server, c *s3.Client, bucket string) {
	t.Helper()
	createBucket(t, c, bucket)
	put(t, c, bucket, "k.txt", []byte("from "+bucket))
	_, body := get(t, c, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("k.txt")})
	requireEqual(t, string(body), "from "+bucket, "get body")

	pre, err := s3.NewPresignClient(c).PresignGetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("k.txt"),
	})
	requireNoErr(t, err, "presign get")
	status, body := getWith(t, srv.Client(), pre.URL)
	requireEqual(t, status, http.StatusOK, "presigned get status")
	requireEqual(t, string(body), "from "+bucket, "presigned get body")
}

// requireRootWorks checks that the root key still lists the buckets.
func requireRootWorks(t *testing.T, srv *httptest.Server) {
	t.Helper()
	_, err := s3As(srv, rootCreds()).ListBuckets(t.Context(), &s3.ListBucketsInput{})
	requireNoErr(t, err, "list buckets with the root key")
}

// TestIdentityUserKeys checks that an IAM user's access key from identity
// works against S3 while it is active, and is refused as S3 refuses a key it
// does not know once it is made inactive or deleted.
func TestIdentityUserKeys(t *testing.T) {
	identity := startSibling(t, "identity", identityDirEnv)
	srv := startWithIdentity(t, identity.URL)
	ic := iamClient(identity.URL)
	ctx := t.Context()

	key := createUserKey(t, ic, "dev")
	putUserPolicy(t, ic, "dev", "s3", s3FullAccess)
	user := s3As(srv, key)
	requireWorks(t, srv, user, "user-bucket")
	requireRootWorks(t, srv)

	// Every key shares the data: the root key reads the IAM user's object.
	_, body := get(t, s3As(srv, rootCreds()), &s3.GetObjectInput{
		Bucket: aws.String("user-bucket"), Key: aws.String("k.txt"),
	})
	requireEqual(t, string(body), "from user-bucket", "root reads the IAM user's object")

	_, err := ic.UpdateAccessKey(ctx, &iam.UpdateAccessKeyInput{
		UserName: aws.String("dev"), AccessKeyId: aws.String(key.AccessKeyID),
		Status: iamtypes.StatusTypeInactive,
	})
	requireNoErr(t, err, "make the key inactive")
	_, err = user.ListBuckets(ctx, &s3.ListBucketsInput{})
	requireCode(t, err, "InvalidAccessKeyId")
	requireResponse(t, err, http.StatusForbidden)
	requireRootWorks(t, srv)

	_, err = ic.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{
		UserName: aws.String("dev"), AccessKeyId: aws.String(key.AccessKeyID),
	})
	requireNoErr(t, err, "delete the key")
	_, err = user.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("user-bucket"), Key: aws.String("k.txt"),
	})
	requireCode(t, err, "InvalidAccessKeyId")
	requireResponse(t, err, http.StatusForbidden)
	requireRootWorks(t, srv)
}

// startMemberAccount starts accounts with an organization and one member
// account, and identity using it, and returns the member account's ID and
// identity's URL.
func startMemberAccount(t *testing.T) (string, string) {
	t.Helper()
	accounts := startSibling(t, "accounts", accountsDirEnv)
	oc := awsorg.New(awsorg.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(accounts.URL),
		Credentials:      credentials.StaticCredentialsProvider{Value: rootCreds()},
		HTTPClient:       &http.Client{Timeout: serviceTimeout},
		RetryMaxAttempts: 1,
	})
	ctx := t.Context()
	_, err := oc.CreateOrganization(ctx, &awsorg.CreateOrganizationInput{})
	requireNoErr(t, err, "create organization")
	created, err := oc.CreateAccount(ctx, &awsorg.CreateAccountInput{
		AccountName: aws.String("Member"), Email: aws.String("member@example.com"),
	})
	requireNoErr(t, err, "create account")
	st := created.CreateAccountStatus
	if st == nil || st.State != orgtypes.CreateAccountStateSucceeded {
		t.Fatalf("create account: %+v", st)
	}
	account := aws.ToString(st.AccountId)

	identity := startSibling(t, "identity", identityDirEnv,
		"TULE_Identity_AccountsEndpoint="+accounts.URL)
	return account, identity.URL
}

// TestIdentitySessionKeys checks that the credentials of an AssumeRole
// session into a member account are verified with their token, and refused
// without it or with another. Verified, the session is another account's
// principal: it lists its own account's buckets, none, and a bucket of the
// server's account needs a bucket policy granting it, which
// TestBucketPolicyCrossAccount covers. (Before bucket policies were
// evaluated, it was refused every request, ListBuckets included.)
func TestIdentitySessionKeys(t *testing.T) {
	account, identityURL := startMemberAccount(t)
	srv := startWithIdentity(t, identityURL)
	ctx := t.Context()

	sc := sts.New(sts.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(identityURL),
		Credentials:      credentials.StaticCredentialsProvider{Value: rootCreds()},
		HTTPClient:       &http.Client{Timeout: serviceTimeout},
		RetryMaxAttempts: 1,
	})
	role := "arn:aws:iam::" + account + ":role/OrganizationAccountAccessRole"
	assumed, err := sc.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(role),
		RoleSessionName: aws.String("e2e"),
	})
	requireNoErr(t, err, "assume role")
	session := aws.Credentials{
		AccessKeyID:     aws.ToString(assumed.Credentials.AccessKeyId),
		SecretAccessKey: aws.ToString(assumed.Credentials.SecretAccessKey),
		SessionToken:    aws.ToString(assumed.Credentials.SessionToken),
	}

	createBucket(t, s3As(srv, rootCreds()), "mgmt")
	listed, err := s3As(srv, session).ListBuckets(ctx, &s3.ListBucketsInput{})
	requireNoErr(t, err, "member session list buckets")
	requireEqual(t, len(listed.Buckets), 0, "buckets the member account owns")
	_, err = s3As(srv, session).ListObjectsV2(ctx,
		&s3.ListObjectsV2Input{Bucket: aws.String("mgmt")})
	requireAPIError(t, err, "AccessDenied", "User: arn:aws:sts::"+account+
		":assumed-role/OrganizationAccountAccessRole/e2e is not authorized to perform: "+
		"s3:ListBucket on resource: \"arn:aws:s3:::mgmt\" because no resource-based policy "+
		"allows the s3:ListBucket action")
	requireResponse(t, err, http.StatusForbidden)
	requireRootWorks(t, srv)

	tampered := session
	tampered.SessionToken = flipLast(session.SessionToken)
	_, err = s3As(srv, tampered).ListBuckets(ctx, &s3.ListBucketsInput{})
	requireAPIError(t, err, "InvalidToken", "The provided token is malformed or otherwise invalid.")
	requireResponse(t, err, http.StatusBadRequest)

	// A session's key without its token is unknown, as on AWS.
	untokened := session
	untokened.SessionToken = ""
	_, err = s3As(srv, untokened).ListBuckets(ctx, &s3.ListBucketsInput{})
	requireCode(t, err, "InvalidAccessKeyId")
	requireRootWorks(t, srv)
}

// flipLast returns s with its last character replaced by another of the
// same kind, so a token keeps its form but is not the session's.
func flipLast(s string) string {
	last := s[len(s)-1]
	repl := byte('A')
	if last == 'A' {
		repl = 'B'
	}
	return s[:len(s)-1] + string(repl)
}

// TestIdentityDown checks that with identity unreachable a request signed
// with one of its keys fails as an internal error, not as an unknown key or
// a valid one, while the root key keeps working.
func TestIdentityDown(t *testing.T) {
	identity := startSibling(t, "identity", identityDirEnv)
	srv := startWithIdentity(t, identity.URL)
	ic := iamClient(identity.URL)
	key := createUserKey(t, ic, "dev")
	putUserPolicy(t, ic, "dev", "s3", s3FullAccess)
	user := s3As(srv, key)
	createBucket(t, user, "before")

	identity.stop(t)

	_, err := user.ListBuckets(t.Context(), &s3.ListBucketsInput{})
	requireCode(t, err, "InternalError")
	requireResponse(t, err, http.StatusInternalServerError)
	requireRootWorks(t, srv)
}
