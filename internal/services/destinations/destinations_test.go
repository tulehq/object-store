package destinations

import (
	"crypto/md5" //nolint:gosec // SQS's digest of the body, which the fake answers
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"tule.dev/functions/serviceinvoke"
	"tule.dev/queues/servicesend"

	"tule.dev/object-store/internal/contracts"
)

// The fakes below stand in for the queues and functions endpoints, so the
// error mapping can be driven through the released clients; the end-to-end
// tests run the real services.

const (
	queueARN    = "arn:aws:sqs:us-east-1:000000000000:events"
	functionARN = "arn:aws:lambda:us-east-1:000000000000:function:recorder"
)

var src = contracts.SourceRef{ARN: "arn:aws:s3:::photos", Account: "000000000000"}

// endpoint is a fake service endpoint that records the request it got and
// answers status and body; a 200 of the queues endpoint answers the body's
// digest.
type endpoint struct {
	got    map[string]any
	auth   string
	body   string
	status int
	mu     sync.Mutex
}

func (e *endpoint) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the request: %v", err)
		}
		var got map[string]any
		if err = json.Unmarshal(raw, &got); err != nil {
			t.Errorf("decode the request: %v", err)
		}
		e.mu.Lock()
		e.got, e.auth = got, r.Header.Get("Authorization")
		status, body := e.status, e.body
		e.mu.Unlock()
		if r.URL.Path == servicesend.Path && status == http.StatusOK {
			msg, _ := got["MessageBody"].(string)
			sum := md5.Sum([]byte(msg)) //nolint:gosec // SQS's digest
			body = `{"MessageId":"m-1","MD5OfMessageBody":"` + hex.EncodeToString(sum[:]) + `"}`
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (e *endpoint) request() (map[string]any, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.got, e.auth
}

func newClients(t *testing.T, queues, functions *endpoint) *Clients {
	t.Helper()
	var qc *servicesend.Client
	if queues != nil {
		c, err := servicesend.New(servicesend.Config{
			Endpoint: queues.serve(t).URL, Region: contracts.Region,
			AccessKeyID: "root", SecretAccessKey: "secret",
		})
		if err != nil {
			t.Fatal(err)
		}
		qc = c
	}
	var fc *serviceinvoke.Client
	if functions != nil {
		c, err := serviceinvoke.New(serviceinvoke.Config{
			Endpoint: functions.serve(t).URL, Region: contracts.Region,
			AccessKeyID: "root", SecretAccessKey: "secret",
		})
		if err != nil {
			t.Fatal(err)
		}
		fc = c
	}
	return New(qc, fc)
}

func requireIs(t *testing.T, err, want error) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Fatalf("got %v, want no error", err)
		}
		return
	}
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

// requireNoContractsError fails if err is nil or one of the contracts
// errors: it must be a failure sending again may not have.
func requireNoContractsError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("got no error")
	}
	for _, e := range []error{
		contracts.ErrDestinationUnconfigured, contracts.ErrDestinationNotFound,
		contracts.ErrDestinationDenied, contracts.ErrDestinationRejected,
	} {
		if errors.Is(err, e) {
			t.Fatalf("got %v, want an error that is none of the contracts errors", err)
		}
	}
}

// A queue is sent the message as s3.amazonaws.com, with the bucket as
// aws:SourceArn and its owner as aws:SourceAccount, signed for sqs; the
// test event and an event message are sent alike.
func TestAQueueIsSentTheMessageAsS3(t *testing.T) {
	q := &endpoint{status: http.StatusOK}
	c := newClients(t, q, nil)
	d := contracts.Destination{Type: contracts.DestinationQueue, ARN: queueARN}
	for name, call := range map[string]func([]byte) error{
		"Validate": func(b []byte) error { return c.Validate(t.Context(), d, b, src) },
		"Deliver":  func(b []byte) error { return c.Deliver(t.Context(), d, b, src) },
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"Records":[{"eventName":"` + name + `"}]}`
			requireIs(t, call([]byte(body)), nil)
			got, auth := q.request()
			want := map[string]any{
				"QueueArn": queueARN, "ServicePrincipal": "s3.amazonaws.com",
				"SourceArn": src.ARN, "SourceAccount": src.Account, "MessageBody": body,
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v", k, got[k], v)
				}
			}
			if !strings.Contains(auth, "/us-east-1/sqs/aws4_request") {
				t.Errorf("Authorization %q is not signed for sqs in us-east-1", auth)
			}
		})
	}
}

// A function is checked with a dry run, which sends no payload, and sent
// an event message as the payload of an Event invocation, as
// s3.amazonaws.com for the bucket and its owner, signed for lambda.
func TestAFunctionIsDryRunAndInvokedAsAnEvent(t *testing.T) {
	f := &endpoint{}
	c := newClients(t, nil, f)
	d := contracts.Destination{Type: contracts.DestinationFunction, ARN: functionARN}
	body := `{"Records":[]}`

	f.status = http.StatusNoContent
	requireIs(t, c.Validate(t.Context(), d, []byte(`{"Event":"s3:TestEvent"}`), src), nil)
	got, auth := f.request()
	if got["InvocationType"] != "DryRun" || got["Payload"] != nil {
		t.Errorf("Validate sent %v, want a DryRun with no payload", got)
	}
	if !strings.Contains(auth, "/us-east-1/lambda/aws4_request") {
		t.Errorf("Authorization %q is not signed for lambda in us-east-1", auth)
	}

	f.mu.Lock()
	f.status, f.body = http.StatusAccepted, `{"StatusCode":202}`
	f.mu.Unlock()
	requireIs(t, c.Deliver(t.Context(), d, []byte(body), src), nil)
	got, _ = f.request()
	payload, _ := json.Marshal(got["Payload"])
	if got["InvocationType"] != "Event" || string(payload) != body {
		t.Errorf("Deliver sent %v, want an Event with payload %s", got, body)
	}
	for k, v := range map[string]any{
		"FunctionName": functionARN, "ServicePrincipal": "s3.amazonaws.com",
		"SourceArn": src.ARN, "SourceAccount": src.Account,
	} {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

// The refusals of the queues endpoint map to the contracts errors; any
// other answer is none of them.
func TestQueueRefusalsMapToTheContractsErrors(t *testing.T) {
	tests := []struct {
		want   error
		name   string
		body   string
		status int
	}{
		{
			name: "not found", status: http.StatusNotFound,
			body: `{"Code":"ResourceNotFound","Message":"no queue"}`,
			want: contracts.ErrDestinationNotFound,
		},
		{
			name: "denied", status: http.StatusForbidden,
			body: `{"Code":"AccessDenied","Message":"denied"}`,
			want: contracts.ErrDestinationDenied,
		},
		{
			name: "invalid message", status: http.StatusBadRequest,
			body: `{"Code":"InvalidMessage","Message":"too long"}`,
			want: contracts.ErrDestinationRejected,
		},
		{
			name: "invalid request", status: http.StatusBadRequest,
			body: `{"Code":"InvalidRequest","Message":"bad"}`,
		},
		{
			name: "unknown key", status: http.StatusForbidden,
			body: `{"Code":"UnknownAccessKey","Message":"who"}`,
		},
		{
			name: "failure", status: http.StatusInternalServerError,
			body: `{"Code":"InternalFailure","Message":"oops"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClients(t, &endpoint{status: tt.status, body: tt.body}, nil)
			err := c.Deliver(t.Context(),
				contracts.Destination{Type: contracts.DestinationQueue, ARN: queueARN},
				[]byte(`{}`), src)
			if tt.want == nil {
				requireNoContractsError(t, err)
				return
			}
			requireIs(t, err, tt.want)
		})
	}
}

// The refusals of the functions endpoint map to the contracts errors: a
// refused payload (400, 413) and a result that cannot be answered (502)
// are rejections; a function at its container cap and a failure are none
// of them.
func TestFunctionRefusalsMapToTheContractsErrors(t *testing.T) {
	tests := []struct {
		want   error
		name   string
		code   string
		status int
	}{
		{
			name: "not found", status: http.StatusNotFound, code: "ResourceNotFound",
			want: contracts.ErrDestinationNotFound,
		},
		{
			name: "denied", status: http.StatusForbidden, code: "AccessDenied",
			want: contracts.ErrDestinationDenied,
		},
		{
			name: "invalid request", status: http.StatusBadRequest, code: "InvalidRequest",
			want: contracts.ErrDestinationRejected,
		},
		{
			name: "too large", status: http.StatusRequestEntityTooLarge, code: "RequestTooLarge",
			want: contracts.ErrDestinationRejected,
		},
		{
			name: "invalid response", status: http.StatusBadGateway,
			code: "InvalidFunctionResponse", want: contracts.ErrDestinationRejected,
		},
		{name: "too many", status: http.StatusTooManyRequests, code: "TooManyRequests"},
		{name: "failure", status: http.StatusInternalServerError, code: "InternalFailure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClients(t, nil, &endpoint{
				status: tt.status, body: `{"Code":"` + tt.code + `","Message":"m"}`,
			})
			d := contracts.Destination{Type: contracts.DestinationFunction, ARN: functionARN}
			for _, err := range []error{
				c.Validate(t.Context(), d, nil, src),
				c.Deliver(t.Context(), d, []byte(`{}`), src),
			} {
				if tt.want == nil {
					requireNoContractsError(t, err)
					continue
				}
				requireIs(t, err, tt.want)
			}
		})
	}
}

// A service without an endpoint has no destination, and a topic never
// exists.
func TestUnconfiguredServicesAndTopics(t *testing.T) {
	c := New(nil, nil)
	for _, tt := range []struct {
		want error
		d    contracts.Destination
	}{
		{
			contracts.ErrDestinationUnconfigured,
			contracts.Destination{Type: contracts.DestinationQueue, ARN: queueARN},
		},
		{
			contracts.ErrDestinationUnconfigured,
			contracts.Destination{Type: contracts.DestinationFunction, ARN: functionARN},
		},
		{contracts.ErrDestinationNotFound, contracts.Destination{
			Type: contracts.DestinationTopic, ARN: "arn:aws:sns:us-east-1:000000000000:t",
		}},
	} {
		requireIs(t, c.Validate(t.Context(), tt.d, []byte(`{}`), src), tt.want)
		requireIs(t, c.Deliver(t.Context(), tt.d, []byte(`{}`), src), tt.want)
	}
}

// A transport failure is none of the contracts errors.
func TestATransportFailureIsNoContractsError(t *testing.T) {
	c := New(closedClients(t))
	requireNoContractsError(t, c.Validate(t.Context(),
		contracts.Destination{Type: contracts.DestinationQueue, ARN: queueARN}, []byte(`{}`), src))
	requireNoContractsError(t, c.Validate(t.Context(),
		contracts.Destination{Type: contracts.DestinationFunction, ARN: functionARN}, nil, src))
}

// closedClients returns clients of servers already closed.
func closedClients(t *testing.T) (*servicesend.Client, *serviceinvoke.Client) {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	qc, err := servicesend.New(servicesend.Config{
		Endpoint: url, Region: contracts.Region, AccessKeyID: "root", SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	fc, err := serviceinvoke.New(serviceinvoke.Config{
		Endpoint: url, Region: contracts.Region, AccessKeyID: "root", SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return qc, fc
}

// A function that cannot be invoked in its state answers 409 only after
// its policy allowed the service, so it validates, as S3 validates a
// function by its permission alone; delivering to it fails, and may not
// next time.
func TestAFunctionInAStateThatCannotBeInvokedValidates(t *testing.T) {
	c := newClients(t, nil, &endpoint{
		status: http.StatusConflict, body: `{"Code":"ResourceConflict","Message":"Pending"}`,
	})
	d := contracts.Destination{Type: contracts.DestinationFunction, ARN: functionARN}
	requireIs(t, c.Validate(t.Context(), d, nil, src), nil)
	err := c.Deliver(t.Context(), d, []byte(`{}`), src)
	requireNoContractsError(t, err)
	requireIs(t, err, serviceinvoke.ErrConflict)
}
