package contracts

import "time"

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
