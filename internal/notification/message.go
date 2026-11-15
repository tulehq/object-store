package notification

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tule.dev/object-store/internal/contracts"
)

// timeFormat is how an event message writes a time: "ISO-8601 format (for
// example, 1970-01-01T00:00:00.000Z)" (S3 User Guide, "Event message
// structure").
const timeFormat = "2006-01-02T15:04:05.000Z"

// The fixed values of an event message.
const (
	eventSource     = "aws:s3"
	s3SchemaVersion = "1.0"
	testEventName   = "s3:TestEvent"
	testService     = "Amazon S3"
	ownerIDLength   = 13
)

// The event message, its fields in the order of the S3 User Guide's
// "Event message structure".
type (
	message struct {
		Records []record `json:"Records"`
	}

	//nolint:govet // fieldalignment: the field order is the message's
	record struct {
		EventVersion      string            `json:"eventVersion"`
		EventSource       string            `json:"eventSource"`
		AWSRegion         string            `json:"awsRegion"`
		EventTime         string            `json:"eventTime"`
		EventName         string            `json:"eventName"`
		UserIdentity      principal         `json:"userIdentity"`
		RequestParameters requestParameters `json:"requestParameters"`
		ResponseElements  responseElements  `json:"responseElements"`
		S3                s3Entity          `json:"s3"`
	}

	principal struct {
		PrincipalID string `json:"principalId"`
	}

	requestParameters struct {
		SourceIPAddress string `json:"sourceIPAddress"`
	}

	responseElements struct {
		RequestID string `json:"x-amz-request-id"`
		HostID    string `json:"x-amz-id-2"`
	}

	//nolint:govet // fieldalignment: the field order is the message's
	s3Entity struct {
		SchemaVersion   string `json:"s3SchemaVersion"`
		ConfigurationID string `json:"configurationId"`
		Bucket          bucket `json:"bucket"`
		Object          object `json:"object"`
	}

	bucket struct {
		Name          string    `json:"name"`
		OwnerIdentity principal `json:"ownerIdentity"`
		ARN           string    `json:"arn"`
	}

	//nolint:govet // fieldalignment: the field order is the message's
	object struct {
		Key                 string `json:"key"`
		Size                *int64 `json:"size,omitempty"`
		ETag                string `json:"eTag,omitempty"`
		VersionID           string `json:"versionId,omitempty"`
		Sequencer           string `json:"sequencer,omitempty"`
		HasObjectAnnotation *bool  `json:"hasObjectAnnotation,omitempty"`
	}

	testEvent struct {
		Service   string `json:"Service"`
		Event     string `json:"Event"`
		Time      string `json:"Time"`
		Bucket    string `json:"Bucket"`
		RequestID string `json:"RequestId"`
		HostID    string `json:"HostId"`
	}
)

// render returns the message for ev sent to the configuration
// configurationID, in a bucket owned by owner (an OwnerPrincipalID). The
// object block has the key, EncodeKey-encoded; the size and eTag when
// ev.HasObject; the eTag alone when ev has one (tagging, ACL); the
// versionId unless the bucket is Unversioned; the sequencer when ev has a
// sequence, as 16 upper-case hexadecimal digits; and hasObjectAnnotation,
// always false, for a copy (N7, N8).
func render(owner, configurationID string, ev *contracts.BucketEvent) ([]byte, error) {
	obj := object{Key: EncodeKey(ev.Key), VersionID: ev.VersionID}
	if ev.HasObject {
		size := ev.Size
		obj.Size = &size
	}
	if ev.ETag != "" {
		obj.ETag = strings.Trim(ev.ETag, `"`)
	}
	if ev.Sequence != 0 {
		obj.Sequencer = fmt.Sprintf("%016X", ev.Sequence)
	}
	if ev.Name == contracts.EventObjectCreatedCopy {
		annotated := false
		obj.HasObjectAnnotation = &annotated
	}
	return marshal(message{Records: []record{{
		EventVersion:      EventVersion,
		EventSource:       eventSource,
		AWSRegion:         bucketRegion,
		EventTime:         ev.Time.UTC().Format(timeFormat),
		EventName:         string(ev.Name),
		UserIdentity:      principal{PrincipalID: ev.Source.PrincipalID},
		RequestParameters: requestParameters{SourceIPAddress: ev.Source.SourceIP},
		ResponseElements: responseElements{
			RequestID: ev.Source.RequestID, HostID: ev.Source.HostID,
		},
		S3: s3Entity{
			SchemaVersion:   s3SchemaVersion,
			ConfigurationID: configurationID,
			Bucket: bucket{
				Name:          ev.Bucket,
				OwnerIdentity: principal{PrincipalID: owner},
				ARN:           "arn:aws:s3:::" + ev.Bucket,
			},
			Object: obj,
		},
	}}})
}

// TestEvent returns the s3:TestEvent message PutBucketNotificationConfiguration
// sends a queue it validates, in the format of the S3 User Guide's
// example, which is not that of other event messages.
func TestEvent(bucket, requestID, hostID string, at time.Time) ([]byte, error) {
	return marshal(testEvent{
		Service: testService, Event: testEventName, Time: at.UTC().Format(timeFormat),
		Bucket: bucket, RequestID: requestID, HostID: hostID,
	})
}

// OwnerPrincipalID returns the ownerIdentity principalId of the buckets of
// account: S3's is "the Amazon retail (Amazon.com) customer ID of the
// bucket owner. This ID value is no longer used" (S3 User Guide), so one
// is derived, the same for an account every time (N4): "A" and the first
// 13 characters of the base32 of sha256("tule:" + account).
func OwnerPrincipalID(account string) string {
	sum := sha256.Sum256([]byte("tule:" + account))
	return "A" + base32.StdEncoding.EncodeToString(sum[:])[:ownerIDLength]
}

// marshal is json.Marshal without its escaping of <, > and &, which S3's
// messages do not have, and without a trailing newline.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("notification: encode message: %w", err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
