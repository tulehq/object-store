package contracts

import (
	"context"
	"errors"
	"time"
)

// Region is the region every bucket is in, and so the region every
// notification destination must be in and the destination services' calls
// are signed for: object-store has no region setting, and every bucket
// reports us-east-1.
const Region = "us-east-1"

// EventName is the eventName of a bucket event message: an event type
// without its "s3:" prefix (S3 User Guide, "Event message structure").
type EventName string

// The events the storage records. The other event types of a notification
// configuration are accepted and never fire.
const (
	EventObjectCreatedPut                       EventName = "ObjectCreated:Put"
	EventObjectCreatedCopy                      EventName = "ObjectCreated:Copy"
	EventObjectCreatedCompleteMultipartUpload   EventName = "ObjectCreated:CompleteMultipartUpload"
	EventObjectRemovedDelete                    EventName = "ObjectRemoved:Delete"
	EventObjectRemovedDeleteMarkerCreated       EventName = "ObjectRemoved:DeleteMarkerCreated"
	EventLifecycleExpirationDelete              EventName = "LifecycleExpiration:Delete"
	EventLifecycleExpirationDeleteMarkerCreated EventName = "LifecycleExpiration:DeleteMarkerCreated"
	EventObjectTaggingPut                       EventName = "ObjectTagging:Put"
	EventObjectTaggingDelete                    EventName = "ObjectTagging:Delete"
	EventObjectACLPut                           EventName = "ObjectAcl:Put"
)

// EventSource is who caused an event, as its message reports it: the
// principal's unique ID, the address the request came from, and the
// x-amz-request-id and x-amz-id-2 of its response.
type EventSource struct {
	PrincipalID string
	SourceIP    string
	RequestID   string
	HostID      string
}

// LifecyclePrincipal is the principalId and the sourceIPAddress of the
// events lifecycle rules cause: S3 acts as itself, and no request from an
// address causes them. A lifecycle event's request and host IDs are fresh
// for each event, in the formats of a response's x-amz-request-id and
// x-amz-id-2, and no response carries them.
const LifecyclePrincipal = "s3.amazonaws.com"

// eventSourceKey is the context key of an EventSource.
type eventSourceKey struct{}

// WithEventSource returns ctx carrying s, the source of the events the
// operations run with it cause.
func WithEventSource(ctx context.Context, s EventSource) context.Context {
	return context.WithValue(ctx, eventSourceKey{}, s)
}

// EventSourceFrom returns the EventSource ctx carries, and false when it
// carries none.
func EventSourceFrom(ctx context.Context) (EventSource, bool) {
	s, ok := ctx.Value(eventSourceKey{}).(EventSource)
	return s, ok
}

// BucketEvent is a change the storage committed, as an event message
// describes it.
//
//nolint:govet // fieldalignment: the fields follow the message's order
type BucketEvent struct {
	Time      time.Time // the commit, on the storage clock
	Source    EventSource
	Name      EventName
	Bucket    string
	Key       string // the key as stored, not URL-encoded
	VersionID string // "" in an Unversioned bucket; "null" for the null version
	ETag      string // without quotes; "" for removals
	Sequence  int64  // 0: no sequencer (tagging, ACL)
	Size      int64
	HasObject bool // size and eTag are reported (created events)
}

// DestinationType is the kind of a notification destination.
type DestinationType string

// The destination types, one per configuration element.
const (
	DestinationQueue    DestinationType = "queue"
	DestinationFunction DestinationType = "function"
	DestinationTopic    DestinationType = "topic"
)

// Destination is where a bucket event is sent: the destination's ARN and
// the Id of the notification configuration that matched it.
type Destination struct {
	Type            DestinationType
	ARN             string
	ConfigurationID string
}

// EventRouter decides where an event goes and renders its message. It does
// no I/O.
type EventRouter interface {
	// Route returns the destination doc, the bucket's stored notification
	// envelope, sends ev to; ok is false when none matches, the envelope
	// names another generation, or the destination can never be delivered
	// to here (a topic, or a service without an endpoint).
	Route(doc []byte, generation string, ev *BucketEvent) (d Destination, ok bool, err error)
	// Render returns the message, {"Records":[...]}, for ev sent to d.
	Render(d Destination, ev *BucketEvent) ([]byte, error)
}

// PendingEvent is a recorded event not yet delivered: its message, Body,
// rendered for Destination. ID names it within Bucket's outbox, and Created
// is when it was recorded, on the wall clock, for the age bound.
type PendingEvent struct {
	Created     time.Time
	Destination Destination
	Bucket      string
	ID          string
	Body        []byte
}

// RecoveredEvents is what StorageProvider's RecoverEvents did with the
// events a stop left recorded but not confirmed: Confirmed are those whose
// change was committed, now pending, and Dropped those whose change was
// not, removed.
type RecoveredEvents struct {
	Confirmed int
	Dropped   int
}

// SourceRef is the bucket an event is sent for, as the destination's policy
// sees it: aws:SourceArn and aws:SourceAccount.
type SourceRef struct {
	ARN     string
	Account string
}

// The errors of Destinations.
var (
	// ErrDestinationUnconfigured is a destination whose service has no
	// endpoint here.
	ErrDestinationUnconfigured = errors.New("events: the destination's service is not configured")
	// ErrDestinationNotFound is a destination that does not exist.
	ErrDestinationNotFound = errors.New("events: no such destination")
	// ErrDestinationDenied is a destination whose policy does not allow
	// s3.amazonaws.com to send to it.
	ErrDestinationDenied = errors.New(
		"events: the destination's policy does not allow s3.amazonaws.com")
	// ErrDestinationRejected is a message the destination refuses, which
	// sending again does not change.
	ErrDestinationRejected = errors.New("events: the destination refuses the message")
)

// Destinations sends to the destinations of bucket events.
type Destinations interface {
	// Validate is PutBucketNotificationConfiguration's check of d: an
	// s3:TestEvent, testEvent, sent to a queue, and a dry run of a
	// function. A topic is always ErrDestinationNotFound.
	Validate(ctx context.Context, d Destination, testEvent []byte, src SourceRef) error
	// Deliver sends body, an event message, to d.
	Deliver(ctx context.Context, d Destination, body []byte, src SourceRef) error
}
