package notification

import (
	"errors"
	"strings"
	"testing"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

func TestParseDestinationARN(t *testing.T) {
	q, f, tp := contracts.DestinationQueue, contracts.DestinationFunction, contracts.DestinationTopic
	for _, tc := range []struct {
		typ  contracts.DestinationType
		arn  string
		want ARN
	}{
		{q, queueARN, ARN{"aws", "sqs", "us-east-1", "000000000000", "uploads"}},
		{
			q, "arn:aws:sqs:eu-west-1:123456789012:a_b-C.fifo",
			ARN{"aws", "sqs", "eu-west-1", "123456789012", "a_b-C.fifo"},
		},
		{f, functionARN, ARN{"aws", "lambda", "us-east-1", "000000000000", "function:thumbs"}},
		{f, functionARN + ":7", ARN{
			"aws", "lambda", "us-east-1", "000000000000",
			"function:thumbs:7",
		}},
		{f, functionARN + ":$LATEST", ARN{
			"aws", "lambda", "us-east-1", "000000000000",
			"function:thumbs:$LATEST",
		}},
		{tp, topicARN, ARN{"aws", "sns", "us-east-1", "000000000000", "alerts"}},
	} {
		got, err := ParseDestinationARN(tc.typ, tc.arn)
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v", tc.arn, got, err)
		}
	}
}

func TestParseDestinationARNRefuses(t *testing.T) {
	long := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = 'a'
		}
		return string(b)
	}
	q, f, tp := contracts.DestinationQueue, contracts.DestinationFunction, contracts.DestinationTopic
	for _, tc := range []struct {
		typ contracts.DestinationType
		arn string
	}{
		{q, ""},
		{q, "uploads"},
		{q, "arn:aws:sqs:us-east-1:000000000000"},
		{q, "urn:aws:sqs:us-east-1:000000000000:q"},
		{q, "arn:aws-us-gov:sqs:us-east-1:000000000000:q"},
		{q, "arn:aws:sns:us-east-1:000000000000:q"},
		{q, "arn:aws:sqs::000000000000:q"},
		{q, "arn:aws:sqs:US-EAST-1:000000000000:q"},
		{q, "arn:aws:sqs:us-east-1::q"},
		{q, "arn:aws:sqs:us-east-1:00000000000a:q"},
		{q, "arn:aws:sqs:us-east-1:000000000000:"},
		{q, "arn:aws:sqs:us-east-1:000000000000:a:b"},
		{q, "arn:aws:sqs:us-east-1:000000000000:" + long(81)},
		{q, "arn:aws:sqs:us-east-1:000000000000:" + long(77) + ".fifo"},
		{q, "arn:aws:sqs:us-east-1:000000000000:.fifo"},
		{f, "arn:aws:lambda:us-east-1:000000000000:thumbs"},
		{f, "arn:aws:lambda:us-east-1:000000000000:function:"},
		{f, "arn:aws:lambda:us-east-1:000000000000:function:" + long(65)},
		{f, functionARN + ":"},
		{f, functionARN + ":a:b"},
		{f, functionARN + ":$latest"},
		{tp, "arn:aws:sns:us-east-1:000000000000:" + long(257)},
		{tp, "arn:aws:sns:us-east-1:000000000000:a b"},
	} {
		_, err := ParseDestinationARN(tc.typ, tc.arn)
		var s3Err *s3api.Error
		if !errors.As(err, &s3Err) || s3Err.Code != ia || s3Err.Message != msgARN ||
			len(s3Err.Arguments) != 1 || s3Err.Arguments[0].Value != tc.arn {
			t.Errorf("%s %q: %v", tc.typ, tc.arn, err)
		}
	}
}

func TestParseDestinationARNUnknownType(t *testing.T) {
	_, err := ParseDestinationARN("bus", queueARN)
	var s3Err *s3api.Error
	if err == nil || errors.As(err, &s3Err) {
		t.Errorf("got %v, want an internal error", err)
	}
}

// TestQueueAndTopicNameLimits pins the name limits: 80 characters for a
// queue and 256 for a topic, a FIFO one's ".fifo" counted in them.
func TestQueueAndTopicNameLimits(t *testing.T) {
	q, tp := contracts.DestinationQueue, contracts.DestinationTopic
	name := func(n int) string { return strings.Repeat("n", n) }
	for _, tc := range []struct {
		typ  contracts.DestinationType
		name string
		ok   bool
	}{
		{q, name(80), true},
		{q, name(81), false},
		{q, name(75) + ".fifo", true},
		{q, name(76) + ".fifo", false},
		{tp, name(256), true},
		{tp, name(257), false},
		{tp, name(251) + ".fifo", true},
		{tp, name(252) + ".fifo", false},
	} {
		service := map[contracts.DestinationType]string{q: "sqs", tp: "sns"}[tc.typ]
		arn := "arn:aws:" + service + ":us-east-1:000000000000:" + tc.name
		a, err := ParseDestinationARN(tc.typ, arn)
		if (err == nil) != tc.ok || tc.ok && a.Resource != tc.name {
			t.Errorf("%s of %d characters: %+v, %v", tc.typ, len(tc.name), a, err)
		}
	}
}
