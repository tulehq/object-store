// Package destinations sends bucket event notifications to their
// destinations as Amazon S3 sends them, as the service principal
// s3.amazonaws.com: to a queue through the queues service's service-send
// endpoint, and to a function through the functions service's
// service-invoke endpoint. Each call supplies aws:SourceArn and
// aws:SourceAccount, the bucket and its owner, so the destination's policy
// can name them (S3 User Guide, "Granting permissions to publish event
// notification messages to a destination"). No message is ever sent to a
// topic: there is no topic service.
package destinations

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"tule.dev/functions/serviceinvoke"
	"tule.dev/queues/servicesend"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/notification"
)

// Clients is the contracts.Destinations of the queues and functions
// services. Either client may be nil, for a service without an endpoint:
// its destinations are then contracts.ErrDestinationUnconfigured. It is
// safe for concurrent use.
type Clients struct {
	queues    *servicesend.Client
	functions *serviceinvoke.Client
}

var _ contracts.Destinations = (*Clients)(nil)

// New returns the Clients sending through queues and functions, either of
// which may be nil.
func New(queues *servicesend.Client, functions *serviceinvoke.Client) *Clients {
	return &Clients{queues: queues, functions: functions}
}

// Validate implements contracts.Destinations: PutBucketNotificationConfiguration
// sends a queue testEvent, an s3:TestEvent, which stays in the queue as it
// does on AWS, and checks a function with a dry run, which runs nothing:
// S3 "first verifies that any ... SNS or SQS destination exists, and that
// the bucket owner has permission to publish to it by sending a test
// notification. In the case of AWS Lambda destinations, Amazon S3 verifies
// that the Lambda function permissions grant Amazon S3 permission to
// invoke the function" (PutBucketNotificationConfiguration API reference).
// So a function that cannot be invoked in its state, pending, updating or
// failed, validates: the functions service evaluates the function policy
// before the state (serviceinvoke.ErrConflict comes after a policy that
// allows the service), and S3 checks the permission alone.
func (c *Clients) Validate(
	ctx context.Context, d contracts.Destination, testEvent []byte, src contracts.SourceRef,
) error {
	switch d.Type {
	case contracts.DestinationQueue:
		return c.send(ctx, d, testEvent, src)
	case contracts.DestinationFunction:
		err := c.invoke(ctx, d, nil, src, serviceinvoke.WithDryRun())
		if errors.Is(err, serviceinvoke.ErrConflict) {
			return nil
		}
		return err
	case contracts.DestinationTopic:
		return fmt.Errorf("%w: %s", contracts.ErrDestinationNotFound, d.ARN)
	}
	return fmt.Errorf("destinations: unknown destination type %q", d.Type)
}

// Deliver implements contracts.Destinations: body is sent to a queue as
// the message, and to a function as the payload of an Event invocation,
// as S3 invokes a function asynchronously.
func (c *Clients) Deliver(
	ctx context.Context, d contracts.Destination, body []byte, src contracts.SourceRef,
) error {
	switch d.Type {
	case contracts.DestinationQueue:
		return c.send(ctx, d, body, src)
	case contracts.DestinationFunction:
		return c.invoke(ctx, d, body, src, serviceinvoke.WithEvent())
	case contracts.DestinationTopic:
		return fmt.Errorf("%w: %s", contracts.ErrDestinationNotFound, d.ARN)
	}
	return fmt.Errorf("destinations: unknown destination type %q", d.Type)
}

// send sends body to the queue d names. A queue that does not exist, a
// queue policy that does not allow the service, and a message the queue
// refuses are the contracts errors; anything else, a transport error or
// another answer of the endpoint, is returned as it is, as a failure
// sending again may not have.
func (c *Clients) send(
	ctx context.Context, d contracts.Destination, body []byte, src contracts.SourceRef,
) error {
	if c.queues == nil {
		return fmt.Errorf("%w: %s", contracts.ErrDestinationUnconfigured, d.ARN)
	}
	_, err := c.queues.SendMessage(ctx, d.ARN, notification.ServicePrincipal, string(body),
		servicesend.WithSourceArn(src.ARN), servicesend.WithSourceAccount(src.Account))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, servicesend.ErrNotFound):
		return fmt.Errorf("%w: %w", contracts.ErrDestinationNotFound, err)
	case errors.Is(err, servicesend.ErrAccessDenied):
		return fmt.Errorf("%w: %w", contracts.ErrDestinationDenied, err)
	case errors.Is(err, servicesend.ErrInvalidMessage):
		return fmt.Errorf("%w: %w", contracts.ErrDestinationRejected, err)
	}
	return fmt.Errorf("destinations: send to %s: %w", d.ARN, err)
}

// invoke invokes the function d names with payload and opts. A function
// that does not exist and a function policy that does not allow the
// service are the contracts errors, and so is a payload the endpoint
// refuses, 400 or 413, or a result it cannot answer, 502, which invoking
// again does not change. Anything else, a function that cannot be invoked
// in its state (serviceinvoke.ErrConflict), one at its container cap
// (429), a failure of the service or a transport error, is returned as it
// is.
func (c *Clients) invoke(
	ctx context.Context,
	d contracts.Destination,
	payload []byte,
	src contracts.SourceRef,
	opts ...serviceinvoke.Option,
) error {
	if c.functions == nil {
		return fmt.Errorf("%w: %s", contracts.ErrDestinationUnconfigured, d.ARN)
	}
	opts = append(opts,
		serviceinvoke.WithSourceArn(src.ARN), serviceinvoke.WithSourceAccount(src.Account))
	_, err := c.functions.Invoke(ctx, d.ARN, notification.ServicePrincipal, payload, opts...)
	var e *serviceinvoke.Error
	switch {
	case err == nil:
		return nil
	case errors.Is(err, serviceinvoke.ErrNotFound):
		return fmt.Errorf("%w: %w", contracts.ErrDestinationNotFound, err)
	case errors.Is(err, serviceinvoke.ErrAccessDenied):
		return fmt.Errorf("%w: %w", contracts.ErrDestinationDenied, err)
	case errors.As(err, &e) && (e.Status == http.StatusBadRequest ||
		e.Status == http.StatusRequestEntityTooLarge || e.Status == http.StatusBadGateway):
		return fmt.Errorf("%w: %w", contracts.ErrDestinationRejected, err)
	}
	return fmt.Errorf("destinations: invoke %s: %w", d.ARN, err)
}
