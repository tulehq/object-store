package bucket

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/notification"
)

const (
	testQueue    = "arn:aws:sqs:us-east-1:000000000000:events"
	testFunction = "arn:aws:lambda:us-east-1:000000000000:function:recorder"
	testTopic    = "arn:aws:sns:us-east-1:000000000000:alerts"
)

// Each destination is validated once, topics, then queues, then
// functions, each in document order.
func TestDestinationsOfNamesEachDestinationOnce(t *testing.T) {
	cfg := &notification.Configuration{
		Queues: []notification.Config{
			{ARN: testQueue}, {ARN: "arn:aws:sqs:us-east-1:000000000000:other"}, {ARN: testQueue},
		},
		Functions: []notification.Config{{ARN: testFunction}, {ARN: testFunction}},
		Topics:    []notification.Config{{ARN: testTopic}},
	}
	got := destinationsOf(cfg)
	want := []contracts.Destination{
		{Type: contracts.DestinationTopic, ARN: testTopic},
		{Type: contracts.DestinationQueue, ARN: testQueue},
		{Type: contracts.DestinationQueue, ARN: "arn:aws:sqs:us-east-1:000000000000:other"},
		{Type: contracts.DestinationFunction, ARN: testFunction},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestValidationReasons(t *testing.T) {
	queue := contracts.Destination{Type: contracts.DestinationQueue, ARN: testQueue}
	function := contracts.Destination{Type: contracts.DestinationFunction, ARN: testFunction}
	wrap := func(err error) error { return fmt.Errorf("%w: detail", err) }
	tests := []struct {
		err  error
		want string
		d    contracts.Destination
	}{
		{wrap(contracts.ErrDestinationDenied), reasonQueueDenied, queue},
		{
			wrap(contracts.ErrDestinationDenied),
			"Not authorized to invoke function [" + testFunction + "]", function,
		},
		{wrap(contracts.ErrDestinationNotFound), reasonNotFound, queue},
		{
			wrap(contracts.ErrDestinationUnconfigured),
			"The destination's service is not configured in this Tule " +
				"(TULE_Storage_QueuesEndpoint)", queue,
		},
		{
			wrap(contracts.ErrDestinationUnconfigured),
			"The destination's service is not configured in this Tule " +
				"(TULE_Storage_FunctionsEndpoint)", function,
		},
		{errors.New("connection refused"), reasonUnreachable, queue},
		{
			wrap(contracts.ErrDestinationRejected),
			"The destination refused the test message",
			function,
		},
	}
	for _, tt := range tests {
		if got := validationReason(tt.d, tt.err); got != tt.want {
			t.Errorf("%v for %s: got %q, want %q", tt.err, tt.d.Type, got, tt.want)
		}
	}
}

// fakeDestinations validates a destination by calling validate with it,
// and records how many validations run at once.
type fakeDestinations struct {
	validate func(ctx context.Context, d contracts.Destination) error
	running  int
	peak     int
	mu       sync.Mutex
}

func (f *fakeDestinations) Validate(
	ctx context.Context, d contracts.Destination, _ []byte, _ contracts.SourceRef,
) error {
	f.mu.Lock()
	f.running++
	f.peak = max(f.peak, f.running)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.running--
		f.mu.Unlock()
	}()
	return f.validate(ctx, d)
}

func (f *fakeDestinations) Deliver(
	context.Context, contracts.Destination, []byte, contracts.SourceRef,
) error {
	return errors.New("not used")
}

func queues(n int) []contracts.Destination {
	out := make([]contracts.Destination, n)
	for i := range out {
		out[i] = contracts.Destination{
			Type: contracts.DestinationQueue, ARN: testQueue + strconv.Itoa(i),
		}
	}
	return out
}

// Destinations are validated at most workers at once, and each error is
// answered at its destination's position.
func TestValidateAllBoundsConcurrencyAndKeepsOrder(t *testing.T) {
	dests := queues(20)
	f := &fakeDestinations{validate: func(_ context.Context, d contracts.Destination) error {
		time.Sleep(10 * time.Millisecond)
		if d.ARN == dests[3].ARN || d.ARN == dests[17].ARN {
			return fmt.Errorf("%w: %s", contracts.ErrDestinationDenied, d.ARN)
		}
		return nil
	}}
	errs := validateAll(t.Context(), f, dests, nil, contracts.SourceRef{}, time.Minute, 4)
	if f.peak > 4 || f.peak < 2 {
		t.Fatalf("%d validations ran at once, want 2 to 4", f.peak)
	}
	for i, err := range errs {
		if want := i == 3 || i == 17; (err != nil) != want ||
			(err != nil && !errors.Is(err, contracts.ErrDestinationDenied)) {
			t.Errorf("destination %d: got %v, want an error %t", i, err, want)
		}
	}
}

// Validation ends at its deadline: the calls running are canceled and
// the destinations not yet called fail, both as not reached.
func TestValidateAllStopsAtItsDeadline(t *testing.T) {
	f := &fakeDestinations{validate: func(ctx context.Context, _ contracts.Destination) error {
		<-ctx.Done()
		return fmt.Errorf("hung: %w", ctx.Err())
	}}
	dests := queues(10)
	start := time.Now()
	errs := validateAll(t.Context(), f, dests, nil, contracts.SourceRef{},
		50*time.Millisecond, 3)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("validation took %v", elapsed)
	}
	for i, err := range errs {
		if !errors.Is(err, context.DeadlineExceeded) ||
			validationReason(dests[i], err) != reasonUnreachable {
			t.Errorf("destination %d: got %v, want the deadline, not reached", i, err)
		}
	}
	if f.peak != 3 {
		t.Errorf("%d validations ran at once, want 3", f.peak)
	}
}
