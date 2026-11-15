// Package notification is a bucket's event notification configuration: its
// document model, the validation of PutBucketNotificationConfiguration, the
// stored form, the matching of a bucket event to the destination it is
// sent to, and the event messages S3 sends. It does no I/O and reads no
// clock: every message takes the time it is about.
//
// TopicConfiguration and EventBridgeConfiguration are validated, stored
// and answered by GET; no event is ever sent to a topic or to EventBridge.
package notification

import (
	"encoding/xml"

	"tule.dev/object-store/internal/contracts"
)

// Limits and fixed values. S3 documents no size limit for the document,
// and 1 MiB is a choice. "The maximum length is 1,024 characters" is the
// FilterRule's (aws-sdk-go-v2 types.FilterRule); the 255 of an Id is a
// choice, as lifecycle's rule ID limit. EventVersion is the version the S3
// User Guide's "Event message structure" shows, unified for all event
// types since 2.4.
const (
	MaxDocument      = 1 << 20
	MaxFilterValue   = 1024
	MaxID            = 255
	EventVersion     = "2.6"
	ServicePrincipal = "s3.amazonaws.com"
	HeaderSkip       = "X-Amz-Skip-Destination-Validation"
)

// bucketRegion is the region every bucket is in and every destination
// must be in: object-store has no region setting, and every bucket reports
// us-east-1 (README "Connecting a client"). A region setting would replace
// it.
const bucketRegion = "us-east-1"

const xmlns = "http://s3.amazonaws.com/doc/2006-03-01/"

// The filter rule names, as GET answers them.
const (
	RulePrefix = "prefix"
	RuleSuffix = "suffix"
)

// Configuration is the PutBucketNotificationConfiguration request and
// GetBucketNotificationConfiguration response document. Each slice keeps
// the document order of its element; each configuration has an Id. Its
// canonical encoding answers the elements in the API reference's order:
// TopicConfiguration, QueueConfiguration, CloudFunctionConfiguration,
// EventBridgeConfiguration.
type Configuration struct {
	EventBridge *struct{}
	XMLName     xml.Name
	Xmlns       string
	Topics      []Config
	Queues      []Config
	Functions   []Config
}

// Config is one TopicConfiguration, QueueConfiguration or
// CloudFunctionConfiguration (the SDK's LambdaFunctionConfiguration); the
// slice it is in chooses the element its ARN is written as: Topic, Queue
// or CloudFunction. The deprecated InvocationRole of a
// CloudFunctionConfiguration is read and dropped.
//
//nolint:govet // fieldalignment: the field order is the element order of its twins
type Config struct {
	ID     string
	ARN    string
	Events []string
	Filter *Filter
}

// Filter is a configuration's key name filter, with at most one prefix and
// one suffix rule.
type Filter struct {
	S3Key S3Key `xml:"S3Key"`
}

// S3Key holds a filter's rules.
type S3Key struct {
	Rules []FilterRule `xml:"FilterRule"`
}

// FilterRule is one rule; its Name is RulePrefix or RuleSuffix.
type FilterRule struct {
	Name  string `xml:"Name"`
	Value string `xml:"Value"`
}

// Empty reports whether c turns notifications off: no configuration and no
// EventBridgeConfiguration.
func Empty(c *Configuration) bool {
	return len(c.Topics) == 0 && len(c.Queues) == 0 && len(c.Functions) == 0 &&
		c.EventBridge == nil
}

// kind is one of the three destination elements.
type kind struct {
	typ     contracts.DestinationType
	element string // the configuration element
	arn     string // the ARN element inside it
	service string // the ARN's service
}

var (
	topicKind = kind{
		contracts.DestinationTopic, "TopicConfiguration", "Topic", "sns",
	}
	queueKind = kind{
		contracts.DestinationQueue, "QueueConfiguration", "Queue", "sqs",
	}
	functionKind = kind{
		contracts.DestinationFunction, "CloudFunctionConfiguration", "CloudFunction", "lambda",
	}
	kinds = []kind{topicKind, queueKind, functionKind}
)

// kindOf is the kind of a destination type.
func kindOf(t contracts.DestinationType) (kind, bool) {
	for _, k := range kinds {
		if k.typ == t {
			return k, true
		}
	}
	return kind{}, false
}

// configs returns the configurations of c of kind k.
func (c *Configuration) configs(k kind) []Config {
	switch k.typ {
	case contracts.DestinationTopic:
		return c.Topics
	case contracts.DestinationQueue:
		return c.Queues
	case contracts.DestinationFunction:
		return c.Functions
	}
	return nil
}
