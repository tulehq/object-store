package notification

import (
	"slices"
	"strings"

	"tule.dev/object-store/internal/contracts"
)

// eventPrefix is the prefix of every event type in a configuration, which
// an event message's eventName does not have.
const eventPrefix = "s3:"

// concreteTypes are the event types of a configuration that are not
// wildcards: the aws-sdk-go-v2 s3 types.Event values (v1.114.10), which
// match the S3 User Guide's "Event notification types and destinations".
// The guide's table also lists s3:TestEvent, which the SDK's enum omits;
// it is refused as a configured type (V2), a choice: only S3 sends it. Most never fire here: only
// the contracts.Event* names are recorded.
var concreteTypes = []string{
	"s3:ReducedRedundancyLostObject",
	"s3:ObjectCreated:Put",
	"s3:ObjectCreated:Post",
	"s3:ObjectCreated:Copy",
	"s3:ObjectCreated:CompleteMultipartUpload",
	"s3:ObjectRemoved:Delete",
	"s3:ObjectRemoved:DeleteMarkerCreated",
	"s3:ObjectRestore:Post",
	"s3:ObjectRestore:Completed",
	"s3:ObjectRestore:Delete",
	"s3:Replication:OperationFailedReplication",
	"s3:Replication:OperationNotTracked",
	"s3:Replication:OperationMissedThreshold",
	"s3:Replication:OperationReplicatedAfterThreshold",
	"s3:LifecycleTransition",
	"s3:IntelligentTiering",
	"s3:ObjectAcl:Put",
	"s3:LifecycleExpiration:Delete",
	"s3:LifecycleExpiration:DeleteMarkerCreated",
	"s3:ObjectTagging:Put",
	"s3:ObjectTagging:Delete",
	"s3:ObjectAnnotation:Put",
	"s3:ObjectAnnotation:Delete",
	"s3:ObjectRetention:Put",
}

// wildcardTypes are the event types that name a family: every concrete
// type that starts with the family and a colon.
var wildcardTypes = []string{
	"s3:ObjectCreated:*",
	"s3:ObjectRemoved:*",
	"s3:ObjectRestore:*",
	"s3:Replication:*",
	"s3:LifecycleExpiration:*",
	"s3:ObjectTagging:*",
	"s3:ObjectAnnotation:*",
}

// eventTypes maps each event type a configuration may name to the event
// names it matches, in concreteTypes order.
var eventTypes = buildEventTypes()

func buildEventTypes() map[string][]contracts.EventName {
	m := make(map[string][]contracts.EventName, len(concreteTypes)+len(wildcardTypes))
	for _, t := range concreteTypes {
		m[t] = []contracts.EventName{contracts.EventName(strings.TrimPrefix(t, eventPrefix))}
	}
	for _, w := range wildcardTypes {
		family := strings.TrimSuffix(w, "*")
		for _, t := range concreteTypes {
			if strings.HasPrefix(t, family) {
				m[w] = append(m[w], contracts.EventName(strings.TrimPrefix(t, eventPrefix)))
			}
		}
	}
	return m
}

// Expand returns the event names an event type of a configuration
// matches: itself without its s3: prefix, or for a wildcard every event of
// its family. ObjectRemoved never matches a lifecycle event: those are the
// LifecycleExpiration family. It returns nil for an unknown type.
func Expand(eventType string) []contracts.EventName {
	return slices.Clone(eventTypes[eventType])
}
