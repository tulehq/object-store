package bucket

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gasmod/gas"
	"github.com/go-chi/chi/v5/middleware"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/httpx"
	"tule.dev/object-store/internal/notification"
	"tule.dev/object-store/internal/s3api"
)

// The notification configuration is served apart from configKinds: it is
// stored as a notification.Stored envelope, which keeps the bucket
// generation it was put in, and its Put validates the destinations it
// names before storing it. The deprecated PutBucketNotification and
// GetBucketNotification share the request lines of
// PutBucketNotificationConfiguration and GetBucketNotificationConfiguration,
// and are served as they are: notification.Parse reads their documents,
// and drops a CloudFunctionConfiguration's InvocationRole.

// emptyNotification is the document of a bucket without a notification
// configuration: "If notifications are not enabled on the bucket, the
// action returns an empty NotificationConfiguration element"
// (GetBucketNotificationConfiguration API reference).
const emptyNotification = xml.Header +
	`<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
	`</NotificationConfiguration>`

// The reasons a destination fails validation, the ArgumentValue answered
// with its ARN (design N13: the denial messages are strings S3 was seen to
// answer, the others choices).
const (
	reasonQueueDenied = "Permissions on the destination queue do not allow S3 to publish " +
		"notifications from this bucket"
	reasonFunctionDenied = "Not authorized to invoke function [%s]"
	reasonNotFound       = "The destination does not exist"
	reasonUnconfigured   = "The destination's service is not configured in this Tule (%s)"
	reasonUnreachable    = "The destination could not be reached"
	reasonRejected       = "The destination refused the test message"
)

// validationTimeout bounds the validation of one configuration's
// destinations: a destination not validated by then fails as not
// reached. validationWorkers is how many destinations are validated at
// once. Each call is also bounded by the clients' own 10 second timeout.
const (
	validationTimeout = 30 * time.Second
	validationWorkers = 8
)

// endpointSettings names the setting that configures each destination
// type's service, for reasonUnconfigured.
var endpointSettings = map[contracts.DestinationType]string{
	contracts.DestinationQueue:    "TULE_Storage_QueuesEndpoint",
	contracts.DestinationFunction: "TULE_Storage_FunctionsEndpoint",
}

// putBucketNotification serves PutBucketNotificationConfiguration (PUT
// /<bucket>?notification): the document is read, at most
// notification.MaxDocument bytes, and validated (notification.Parse);
// then, unless x-amz-skip-destination-validation is true, each destination
// it names is validated (validateDestinations); then it is stored with the
// bucket's generation, replacing any configuration the bucket had, or, for
// a configuration with nothing in it, the bucket's is removed: "You can
// disable notifications by adding the empty NotificationConfiguration
// element" (its API reference). It answers 200 with no body. The
// operation is evaluated before the body is read, and the bucket pinned
// only after the destinations are validated, so a DeleteBucket does not
// wait on their calls. Content-MD5 and the checksum headers are accepted
// and ignored.
func (m *Module) putBucketNotification(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}
	r := ctx.Request()

	caller, err := m.authz.Authorize(r, authz.PutBucketNotificationConfiguration, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	skip, err := notification.ParseSkip(r.Header.Get(notification.HeaderSkip))
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	doc, err := s3api.ReadDocument(r, notification.MaxDocument, m.verifier.ReadBody)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	cfg, err := notification.Parse(doc)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	// The generation stored is the one evaluated, which Pin then holds,
	// so a bucket deleted and created again meanwhile is NoSuchBucket
	// rather than given this configuration.
	ref, err := caller.Ref(bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	if !skip {
		if err = m.validateDestinations(r, bucket, cfg); err != nil {
			return s3api.WriteError(ctx, target, err)
		}
	}
	pinned, release, err := caller.Pin(ctx, bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer release()
	if err = m.storeNotification(pinned, ref, cfg); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

// storeNotification stores cfg as the notification configuration of the
// bucket ref, in its generation, or removes the bucket's when cfg has
// nothing in it, so a bucket without notifications has no file to read on
// each write.
func (m *Module) storeNotification(
	pinned context.Context, ref contracts.BucketRef, cfg *notification.Configuration,
) error {
	if notification.Empty(cfg) {
		//nolint:wrapcheck // the storage's errors, answered by s3api.WriteError
		return m.storage.DeleteBucketConfig(pinned, ref.Name, contracts.ConfigNotification)
	}
	stored, err := notification.Encode(ref.Generation, cfg)
	if err != nil {
		return err //nolint:wrapcheck // the notification package's, already described
	}
	//nolint:wrapcheck // as above
	return m.storage.PutBucketConfig(pinned, ref.Name, contracts.ConfigNotification, stored)
}

// validateDestinations validates each distinct destination cfg names, in
// the order GET answers them, as S3 does before it stores a configuration
// (contracts.Destinations' Validate): a queue is sent an s3:TestEvent,
// which stays in it, and a function is checked with a dry run. Every
// destination is tried, and every one that fails is answered, each ARN
// with its reason, as one 400 InvalidArgument "Unable to validate the
// following destination configurations"; the test events already sent
// stay sent, as on AWS. A topic always fails, as there is no topic
// service, and so does a destination whose service has no endpoint here,
// or which could not be reached, which is logged. At most
// validationWorkers destinations are validated at once, and all of them
// within validationTimeout.
func (m *Module) validateDestinations(
	r *http.Request, bucket string, cfg *notification.Configuration,
) error {
	ctx := r.Context()
	testEvent, err := notification.TestEvent(bucket, middleware.GetReqID(ctx),
		httpx.HostID(ctx), time.Now())
	if err != nil {
		return err //nolint:wrapcheck // the notification package's, already described
	}
	src := contracts.SourceRef{ARN: "arn:aws:s3:::" + bucket, Account: m.authz.Account()}
	dests := destinationsOf(cfg)
	errs := validateAll(ctx, m.destinations, dests, testEvent, src,
		validationTimeout, validationWorkers)
	var failed []s3api.ErrorArgument
	for i, d := range dests {
		vErr := errs[i]
		if vErr == nil {
			continue
		}
		reason := validationReason(d, vErr)
		if reason == reasonUnreachable {
			httpx.Logger(ctx).Warn("failed to validate a notification destination").
				Str("bucket", bucket).Str("destination", d.ARN).Err("error", vErr).Send()
		}
		failed = append(failed, s3api.ErrorArgument{Name: d.ARN, Value: reason})
	}
	if len(failed) > 0 {
		return s3api.ErrUnableToValidate(failed)
	}
	return nil
}

// validateAll validates each of dests with v, at most workers at once,
// and returns the error of each, in the order of dests. Validation ends
// timeout after it starts: a call still running is canceled, and a
// destination not yet called fails with the context's error, so either
// fails as not reached.
func validateAll(
	ctx context.Context,
	v contracts.Destinations,
	dests []contracts.Destination,
	testEvent []byte,
	src contracts.SourceRef,
	timeout time.Duration,
	workers int,
) []error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	errs := make([]error, len(dests))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, d := range dests {
		if ctx.Err() == nil {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
			}
		}
		if err := ctx.Err(); err != nil {
			errs[i] = fmt.Errorf("bucket: validation of %s not started: %w", d.ARN, err)
			continue
		}
		wg.Go(func() {
			defer func() { <-sem }()
			errs[i] = v.Validate(ctx, d, testEvent, src)
		})
	}
	wg.Wait()
	return errs
}

// destinationsOf returns the destinations cfg names, each once, topics,
// then queues, then functions, each in document order.
func destinationsOf(cfg *notification.Configuration) []contracts.Destination {
	var out []contracts.Destination
	seen := map[contracts.Destination]bool{}
	for _, kind := range []struct {
		typ     contracts.DestinationType
		configs []notification.Config
	}{
		{contracts.DestinationTopic, cfg.Topics},
		{contracts.DestinationQueue, cfg.Queues},
		{contracts.DestinationFunction, cfg.Functions},
	} {
		for _, c := range kind.configs {
			d := contracts.Destination{Type: kind.typ, ARN: c.ARN}
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}

// validationReason is the ArgumentValue answered for d, which failed
// validation with err.
func validationReason(d contracts.Destination, err error) string {
	switch {
	case errors.Is(err, contracts.ErrDestinationDenied) &&
		d.Type == contracts.DestinationFunction:
		return fmt.Sprintf(reasonFunctionDenied, d.ARN)
	case errors.Is(err, contracts.ErrDestinationDenied):
		return reasonQueueDenied
	case errors.Is(err, contracts.ErrDestinationNotFound):
		return reasonNotFound
	case errors.Is(err, contracts.ErrDestinationUnconfigured):
		return fmt.Sprintf(reasonUnconfigured, endpointSettings[d.Type])
	case errors.Is(err, contracts.ErrDestinationRejected):
		return reasonRejected
	}
	return reasonUnreachable
}

// getBucketNotification serves GetBucketNotificationConfiguration (GET
// /<bucket>?notification): the stored document, or, for a bucket without
// one, an empty NotificationConfiguration. A configuration stored in
// another generation of the bucket, which a server that did not know
// notification configurations can leave when it deletes the bucket and
// creates it again, is none.
func (m *Module) getBucketNotification(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(),
		authz.GetBucketNotificationConfiguration, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer g.Release()
	ref, err := g.Caller().Ref(bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	raw, err := m.storage.GetBucketConfig(g.Context(), bucket, contracts.ConfigNotification)
	g.Release()
	if errors.Is(err, contracts.ErrNoSuchBucketConfig) {
		return m.writeDocument(ctx, []byte(emptyNotification))
	}
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	stored, _, err := notification.Decode(raw)
	if err != nil {
		return s3api.WriteError(ctx, target, fmt.Errorf(
			"bucket: the stored notification configuration of bucket %s: %w", bucket, err))
	}
	if stored.Generation != ref.Generation {
		return m.writeDocument(ctx, []byte(emptyNotification))
	}
	return m.writeDocument(ctx, []byte(stored.Document))
}
