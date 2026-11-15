// Command recorder is a function for the notification tests: it takes an
// S3 event as Lambda gives one to a function (aws-lambda-go's
// events.S3Event), fails when a record lacks a field every event message
// has, or a created event lacks its object's size, ETag or sequencer, and
// returns a summary of the records, which the test reads back through the
// functions service's retrieval API.
package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

// record is the summary of one record.
type record struct {
	EventName       string `json:"eventName"`
	Bucket          string `json:"bucket"`
	Key             string `json:"key"`
	ConfigurationID string `json:"configurationId"`
	ETag            string `json:"eTag"`
	Sequencer       string `json:"sequencer"`
	PrincipalID     string `json:"principalId"`
	SourceIP        string `json:"sourceIPAddress"`
	RequestID       string `json:"requestId"`
	Size            int64  `json:"size"`
}

type summary struct {
	Records []record `json:"records"`
}

func handler(_ context.Context, ev events.S3Event) (summary, error) {
	if len(ev.Records) == 0 {
		return summary{}, errors.New("no records")
	}
	var out summary
	for i, r := range ev.Records {
		missing := missingFields(r)
		if len(missing) > 0 {
			return summary{}, fmt.Errorf("record %d lacks %s", i, strings.Join(missing, ", "))
		}
		out.Records = append(out.Records, record{
			EventName: r.EventName, Bucket: r.S3.Bucket.Name, Key: r.S3.Object.Key,
			ConfigurationID: r.S3.ConfigurationID, ETag: r.S3.Object.ETag,
			Sequencer: r.S3.Object.Sequencer, PrincipalID: r.PrincipalID.PrincipalID,
			SourceIP:  r.RequestParameters.SourceIPAddress,
			RequestID: r.ResponseElements["x-amz-request-id"], Size: r.S3.Object.Size,
		})
	}
	return out, nil
}

// missingFields names the fields r lacks.
func missingFields(r events.S3EventRecord) []string {
	fields := map[string]bool{
		"eventVersion":                        r.EventVersion != "",
		"eventSource":                         r.EventSource == "aws:s3",
		"awsRegion":                           r.AWSRegion != "",
		"eventTime":                           !r.EventTime.IsZero(),
		"eventName":                           r.EventName != "",
		"userIdentity.principalId":            r.PrincipalID.PrincipalID != "",
		"requestParameters.sourceIPAddress":   r.RequestParameters.SourceIPAddress != "",
		"responseElements.x-amz-request-id":   r.ResponseElements["x-amz-request-id"] != "",
		"responseElements.x-amz-id-2":         r.ResponseElements["x-amz-id-2"] != "",
		"s3.s3SchemaVersion":                  r.S3.SchemaVersion != "",
		"s3.configurationId":                  r.S3.ConfigurationID != "",
		"s3.bucket.name":                      r.S3.Bucket.Name != "",
		"s3.bucket.arn":                       r.S3.Bucket.Arn != "",
		"s3.bucket.ownerIdentity.principalId": r.S3.Bucket.OwnerIdentity.PrincipalID != "",
		"s3.object.key":                       r.S3.Object.Key != "",
	}
	if strings.HasPrefix(r.EventName, "ObjectCreated:") {
		fields["s3.object.size"] = r.S3.Object.Size > 0
		fields["s3.object.eTag"] = r.S3.Object.ETag != ""
		fields["s3.object.sequencer"] = r.S3.Object.Sequencer != ""
	}
	var missing []string
	for name, ok := range fields {
		if !ok {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}

func main() {
	lambda.Start(handler)
}
