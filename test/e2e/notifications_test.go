//go:build e2e

package e2e

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// emptyNotification is GetBucketNotificationConfiguration's answer for a
// bucket without a configuration.
const emptyNotification = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
	`<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
	`</NotificationConfiguration>`

// unableToValidate is the message of a configuration whose destinations
// fail validation.
const unableToValidate = "Unable to validate the following destination configurations"

// skipValidation is the header that skips destination validation.
var skipValidation = map[string]string{"X-Amz-Skip-Destination-Validation": "true"}

func putNotification(
	t *testing.T, c *s3.Client, bucket string, cfg *types.NotificationConfiguration, skip bool,
) error {
	t.Helper()
	in := &s3.PutBucketNotificationConfigurationInput{
		Bucket: aws.String(bucket), NotificationConfiguration: cfg,
	}
	if skip {
		in.SkipDestinationValidation = aws.Bool(true)
	}
	_, err := c.PutBucketNotificationConfiguration(t.Context(), in)
	return err //nolint:wrapcheck // the test inspects the SDK error
}

func getNotification(
	t *testing.T, c *s3.Client, bucket string,
) (*s3.GetBucketNotificationConfigurationOutput, error) {
	t.Helper()
	//nolint:wrapcheck // the test inspects the SDK error
	return c.GetBucketNotificationConfiguration(t.Context(),
		&s3.GetBucketNotificationConfigurationInput{Bucket: aws.String(bucket)})
}

func keyFilter(rules ...types.FilterRule) *types.NotificationConfigurationFilter {
	return &types.NotificationConfigurationFilter{Key: &types.S3KeyFilter{FilterRules: rules}}
}

func filterRule(name, value string) types.FilterRule {
	return types.FilterRule{Name: types.FilterRuleName(name), Value: aws.String(value)}
}

// uuidText is the text form of a version 4 UUID.
var uuidText = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// The destinations of everyElement.
const (
	rtTopic    = "arn:aws:sns:us-east-1:000000000000:alerts"
	rtQueue    = "arn:aws:sqs:us-east-1:000000000000:events"
	rtFunction = "arn:aws:lambda:us-east-1:000000000000:function:thumbnail"
)

// everyElement is a configuration of every element the SDK models; its
// queue configuration has no Id, and its function's filter rule name is
// not in lower case.
var everyElement = &types.NotificationConfiguration{
	TopicConfigurations: []types.TopicConfiguration{{
		Id: aws.String("alerts"), TopicArn: aws.String(rtTopic),
		Events: []types.Event{"s3:ObjectCreated:*"},
		Filter: keyFilter(filterRule("prefix", "images/"), filterRule("suffix", ".jpg")),
	}},
	QueueConfigurations: []types.QueueConfiguration{{
		QueueArn: aws.String(rtQueue),
		Events:   []types.Event{"s3:ObjectRemoved:*", "s3:LifecycleExpiration:Delete"},
	}},
	LambdaFunctionConfigurations: []types.LambdaFunctionConfiguration{{
		Id: aws.String("thumbs"), LambdaFunctionArn: aws.String(rtFunction),
		Events: []types.Event{"s3:ObjectTagging:*"},
		Filter: keyFilter(filterRule("Prefix", "docs/")),
	}},
	EventBridgeConfiguration: &types.EventBridgeConfiguration{},
}

// startNotificationServer starts object-store with no destination service
// and the bucket notifyBucket.
func startNotificationServer(t *testing.T) (*liveServer, *s3.Client) {
	t.Helper()
	l := startLive(t, nil)
	c := l.testClient()
	createBucket(t, c, notifyBucket)
	return l, c
}

// A bucket without a configuration answers an empty one. A configuration
// of every element is stored as it was put, which needs the topic's
// validation skipped, and answered in the API reference's element order,
// filter rule names in lower case and a configuration without an Id given
// one in the form of the API reference's sample, the base64 of a UUID,
// which putting the answer back keeps.
func TestNotificationConfigurationRoundTrip(t *testing.T) {
	l, c := startNotificationServer(t)
	res := notificationRequest(t, l, testCreds, http.MethodGet, "", nil)
	requireEqual(t, res.StatusCode, http.StatusOK, "get before any put")
	requireEqual(t, res.Body, emptyNotification, "the empty document")

	requireCode(t, putNotification(t, c, notifyBucket, everyElement, false), "InvalidArgument")
	requireNoErr(t, putNotification(t, c, notifyBucket, everyElement, true), "put every element")

	got, err := getNotification(t, c, notifyBucket)
	requireNoErr(t, err, "get every element")
	requireEqual(t, len(got.TopicConfigurations), 1, "topic configurations")
	tc := got.TopicConfigurations[0]
	requireEqual(t, aws.ToString(tc.Id), "alerts", "topic Id")
	requireEqual(t, aws.ToString(tc.TopicArn), rtTopic, "topic ARN")
	sameJSON(t, tc.Events, everyElement.TopicConfigurations[0].Events, "topic events")
	sameJSON(t, tc.Filter, everyElement.TopicConfigurations[0].Filter, "topic filter")
	requireEqual(t, len(got.QueueConfigurations), 1, "queue configurations")
	qc := got.QueueConfigurations[0]
	requireEqual(t, aws.ToString(qc.QueueArn), rtQueue, "queue ARN")
	sameJSON(t, qc.Events, everyElement.QueueConfigurations[0].Events, "queue events")
	requireEqual(t, qc.Filter == nil, true, "no queue filter")
	id, err := base64.StdEncoding.DecodeString(aws.ToString(qc.Id))
	if err != nil || !uuidText.Match(id) {
		t.Fatalf("generated Id %q is not the base64 of a UUID (%q, %v)",
			aws.ToString(qc.Id), id, err)
	}
	requireEqual(t, len(got.LambdaFunctionConfigurations), 1, "function configurations")
	fc := got.LambdaFunctionConfigurations[0]
	requireEqual(t, aws.ToString(fc.Id), "thumbs", "function Id")
	requireEqual(t, aws.ToString(fc.LambdaFunctionArn), rtFunction, "function ARN")
	sameJSON(t, fc.Filter, keyFilter(filterRule("prefix", "docs/")), "function filter")
	requireEqual(t, got.EventBridgeConfiguration != nil, true, "EventBridge echoed")

	res = notificationRequest(t, l, testCreds, http.MethodGet, "", nil)
	requireEqual(t, res.Header.Get("Content-Type"), "application/xml", "content type")
	order := []string{
		"<TopicConfiguration>", "<QueueConfiguration>", "<CloudFunctionConfiguration>",
		"<EventBridgeConfiguration>",
	}
	for i := 1; i < len(order); i++ {
		a, b := strings.Index(res.Body, order[i-1]), strings.Index(res.Body, order[i])
		if a < 0 || b < a {
			t.Fatalf("%s is not before %s in %s", order[i-1], order[i], res.Body)
		}
	}

	requireNoErr(t, putNotification(t, c, notifyBucket, &types.NotificationConfiguration{
		TopicConfigurations: got.TopicConfigurations, QueueConfigurations: got.QueueConfigurations,
		LambdaFunctionConfigurations: got.LambdaFunctionConfigurations,
		EventBridgeConfiguration:     got.EventBridgeConfiguration,
	}, true), "put the answer back")
	again, err := getNotification(t, c, notifyBucket)
	requireNoErr(t, err, "get it again")
	requireEqual(t, aws.ToString(again.QueueConfigurations[0].Id), aws.ToString(qc.Id),
		"the generated Id, put back")
}

// "You can disable notifications by adding the empty
// NotificationConfiguration element" (PutBucketNotificationConfiguration
// API reference), and the deprecated PutBucketNotification's
// CloudFunctionConfiguration is read, its InvocationRole dropped.
func TestNotificationConfigurationDisableAndDeprecatedForm(t *testing.T) {
	l, c := startNotificationServer(t)
	requireNoErr(t, putNotification(t, c, notifyBucket, everyElement, true), "put every element")
	requireNoErr(t, putNotification(t, c, notifyBucket, &types.NotificationConfiguration{}, false),
		"put the empty configuration")
	res := notificationRequest(t, l, testCreds, http.MethodGet, "", nil)
	requireEqual(t, res.Body, emptyNotification, "the configuration, turned off")

	v1 := `<NotificationConfiguration><CloudFunctionConfiguration><Id>legacy</Id>` +
		`<InvocationRole>arn:aws:iam::000000000000:role/s3-invoke</InvocationRole>` +
		`<CloudFunction>` + rtFunction + `</CloudFunction><Event>s3:ObjectCreated:Put</Event>` +
		`</CloudFunctionConfiguration></NotificationConfiguration>`
	res = notificationRequest(t, l, testCreds, http.MethodPut, v1, skipValidation)
	requireEqual(t, res.StatusCode, http.StatusOK, "put the deprecated form: "+res.Body)
	requireEqual(t, res.Body, "", "the put's body")
	got, err := getNotification(t, c, notifyBucket)
	requireNoErr(t, err, "get the deprecated form")
	requireEqual(t, len(got.LambdaFunctionConfigurations), 1, "function configurations")
	requireEqual(t, aws.ToString(got.LambdaFunctionConfigurations[0].Id), "legacy", "its Id")
	res = notificationRequest(t, l, testCreds, http.MethodGet, "", nil)
	if strings.Contains(res.Body, "InvocationRole") {
		t.Fatalf("the deprecated form is answered with its InvocationRole: %s", res.Body)
	}
}

// A configuration stored in another generation of its bucket, as a server
// that did not know notification configurations leaves one across a
// delete and create, is none; a missing bucket is NoSuchBucket; and S3
// has no DeleteBucketNotification.
func TestNotificationConfigurationGenerationAndMissingBucket(t *testing.T) {
	l, c := startNotificationServer(t)
	requireNoErr(t, putNotification(t, c, notifyBucket, everyElement, true), "put every element")
	file := filepath.Join(os.Getenv("TULE_Storage_RootDir"), ".tule", "config", "notification",
		notifyBucket)
	raw, err := os.ReadFile(file) //nolint:gosec // G304: this test's own storage root
	requireNoErr(t, err, "read the stored configuration")
	var stored map[string]string
	requireNoErr(t, json.Unmarshal(raw, &stored), "decode the stored configuration")
	stored["generation"] = "another"
	raw, err = json.Marshal(stored)
	requireNoErr(t, err, "encode the stored configuration")
	//nolint:gosec // G703: this test's own storage root
	requireNoErr(t, os.WriteFile(file, raw, 0o600), "write the stored configuration")
	res := notificationRequest(t, l, testCreds, http.MethodGet, "", nil)
	requireEqual(t, res.Body, emptyNotification, "a configuration of another generation")

	requireCode(t, putNotification(t, c, "missing", &types.NotificationConfiguration{}, true),
		"NoSuchBucket")
	_, err = getNotification(t, c, "missing")
	requireCode(t, err, "NoSuchBucket")

	res = notificationRequest(t, l, testCreds, http.MethodDelete, "", nil)
	var e argumentError
	requireNoErr(t, xml.Unmarshal([]byte(res.Body), &e), "decode "+res.Body)
	requireEqual(t, res.StatusCode, http.StatusNotImplemented, "DELETE ?notification")
	requireEqual(t, e.Code, "NotImplemented", "DELETE ?notification")
}

// configWith returns a document holding one QueueConfiguration whose body
// is inner.
func configWith(inner string) string {
	return `<NotificationConfiguration><QueueConfiguration>` + inner +
		`</QueueConfiguration></NotificationConfiguration>`
}

// queueConfig is the inside of a QueueConfiguration of the queue events,
// for every created event, with extra after its Event.
func queueConfig(extra string) string {
	return `<Queue>` + rtQueue + `</Queue><Event>s3:ObjectCreated:*</Event>` + extra
}

// queueARNConfig is a document of one QueueConfiguration of arn for every
// created event.
func queueARNConfig(arn string) string {
	return configWith(`<Queue>` + arn + `</Queue><Event>s3:ObjectCreated:*</Event>`)
}

// prefixFilterOf is a Filter of one rule.
func prefixFilterOf(name, value string) string {
	return `<Filter><S3Key><FilterRule><Name>` + name + `</Name><Value>` + value +
		`</Value></FilterRule></S3Key></Filter>`
}

const malformedXML = "The XML you provided was not well-formed or did not validate " +
	"against our published schema."

// notificationCase is one refused configuration: its document, the error
// code and message, and the first argument the error names.
type notificationCase struct {
	name, doc, code, msg string
	argName, argValue    string
}

// notificationCases are the checks V1 to V9 of the design, in order.
var notificationCases = []notificationCase{
	{name: "V1 not XML", doc: "<Notification", code: "MalformedXML", msg: malformedXML},
	{
		name: "V1 another root", doc: "<LifecycleConfiguration/>",
		code: "MalformedXML", msg: malformedXML,
	},
	{
		name: "V1 no Queue", doc: configWith(`<Event>s3:ObjectCreated:*</Event>`),
		code: "MalformedXML", msg: malformedXML,
	},
	{
		name: "V1 no Event", doc: configWith(`<Queue>` + rtQueue + `</Queue>`),
		code: "MalformedXML", msg: malformedXML,
	},
	{
		name: "V1 two EventBridge",
		doc: `<NotificationConfiguration><EventBridgeConfiguration/>` +
			`<EventBridgeConfiguration/></NotificationConfiguration>`,
		code: "MalformedXML", msg: malformedXML,
	},
	{
		name: "V2 unknown event",
		doc:  configWith(`<Queue>` + rtQueue + `</Queue><Event>s3:ObjectCreated:Upload</Event>`),
		code: "InvalidArgument", msg: "The event is not supported for notifications",
	},
	{
		name: "V2 test event",
		doc:  configWith(`<Queue>` + rtQueue + `</Queue><Event>s3:TestEvent</Event>`),
		code: "InvalidArgument", msg: "The event is not supported for notifications",
	},
	{
		name: "V3 rule name", doc: configWith(queueConfig(prefixFilterOf("middle", "x"))),
		code: "InvalidArgument", msg: "filter rule name must be either prefix or suffix",
	},
	{
		name: "V4 two prefixes",
		doc: configWith(queueConfig(`<Filter><S3Key>` +
			`<FilterRule><Name>prefix</Name><Value>a</Value></FilterRule>` +
			`<FilterRule><Name>Prefix</Name><Value>b</Value></FilterRule></S3Key></Filter>`)),
		code: "InvalidArgument", msg: "Cannot specify more than one prefix rule in a filter.",
	},
	{
		name: "V4 two suffixes",
		doc: configWith(queueConfig(`<Filter><S3Key>` +
			`<FilterRule><Name>suffix</Name><Value>a</Value></FilterRule>` +
			`<FilterRule><Name>suffix</Name><Value>b</Value></FilterRule></S3Key></Filter>`)),
		code: "InvalidArgument", msg: "Cannot specify more than one suffix rule in a filter.",
	},
	{
		name: "V5 long value",
		doc:  configWith(queueConfig(prefixFilterOf("prefix", strings.Repeat("a", 1025)))),
		code: "InvalidArgument",
		msg:  "The filter rule value cannot be longer than 1024 characters.",
	},
	{
		name: "V6 long Id",
		doc:  configWith(`<Id>` + strings.Repeat("i", 256) + `</Id>` + queueConfig("")),
		code: "InvalidArgument", msg: "The configuration Id cannot be longer than 255 characters.",
	},
	{
		name: "V6 duplicate Id",
		doc: `<NotificationConfiguration>` +
			`<QueueConfiguration><Id>same</Id>` + queueConfig("") + `</QueueConfiguration>` +
			`<QueueConfiguration><Id>same</Id><Queue>` + queueARN("other") + `</Queue>` +
			`<Event>s3:ObjectRemoved:*</Event></QueueConfiguration></NotificationConfiguration>`,
		code: "InvalidArgument", msg: "Configuration Id must be unique.",
	},
	{
		name: "V7 malformed ARN", doc: queueARNConfig("arn:aws:sqs:us-east-1:events"),
		code: "InvalidArgument", msg: "The ARN is not well formed",
		argName: "Queue", argValue: "arn:aws:sqs:us-east-1:events",
	},
	{
		name: "V7 another service",
		doc:  queueARNConfig("arn:aws:sns:us-east-1:000000000000:events"),
		code: "InvalidArgument", msg: "The ARN is not well formed",
		argName: "Queue", argValue: "arn:aws:sns:us-east-1:000000000000:events",
	},
	{
		name: "V7 another partition",
		doc:  queueARNConfig("arn:aws-cn:sqs:us-east-1:000000000000:events"),
		code: "InvalidArgument", msg: "The ARN is not well formed",
		argName: "Queue", argValue: "arn:aws-cn:sqs:us-east-1:000000000000:events",
	},
	{
		name: "V8 another region",
		doc:  queueARNConfig("arn:aws:sqs:eu-west-1:000000000000:events"),
		code: "InvalidArgument",
		msg: "The notification destination service region is not valid for the bucket " +
			"location constraint",
		argName: "Queue", argValue: "arn:aws:sqs:eu-west-1:000000000000:events",
	},
	{
		name: "V8 FIFO queue", doc: queueARNConfig(queueARN("events.fifo")),
		code:    "InvalidArgument",
		msg:     "FIFO queues and topics are not supported as notification destinations",
		argName: "Queue", argValue: queueARN("events.fifo"),
	},
	{
		// A queue and a function overlap too (N12).
		name: "V9 overlap",
		doc: `<NotificationConfiguration><QueueConfiguration>` +
			queueConfig(prefixFilterOf("prefix", "images/")) + `</QueueConfiguration>` +
			`<CloudFunctionConfiguration><CloudFunction>` + functionARN("f") +
			`</CloudFunction><Event>s3:ObjectCreated:Put</Event>` +
			prefixFilterOf("prefix", "images/jpg/") +
			`</CloudFunctionConfiguration></NotificationConfiguration>`,
		code: "InvalidArgument",
		msg: "Configurations overlap. Configurations on the same bucket cannot share a " +
			"common event type.",
	},
}

// Each check of a configuration answers its error, whether or not
// destination validation is skipped, and before any destination is
// called: this server has no destination service.
func TestNotificationValidation(t *testing.T) {
	l, _ := startNotificationServer(t)
	for _, tt := range notificationCases {
		t.Run(tt.name, func(t *testing.T) {
			for _, header := range []map[string]string{nil, skipValidation} {
				res := notificationRequest(t, l, testCreds, http.MethodPut, tt.doc, header)
				e := requireBadRequest(t, res, tt.code, tt.msg)
				requireEqual(t, e.ArgumentName1, tt.argName, "ArgumentName1")
				requireEqual(t, e.ArgumentValue1, tt.argValue, "ArgumentValue1")
			}
		})
	}

	// "You can use overlapping object key name filters with different
	// event types" (S3 User Guide).
	res := notificationRequest(t, l, testCreds, http.MethodPut,
		`<NotificationConfiguration><QueueConfiguration>`+
			queueConfig(prefixFilterOf("prefix", "images/"))+`</QueueConfiguration>`+
			`<QueueConfiguration><Queue>`+queueARN("other")+`</Queue>`+
			`<Event>s3:ObjectRemoved:*</Event>`+prefixFilterOf("prefix", "images/")+
			`</QueueConfiguration></NotificationConfiguration>`, skipValidation)
	requireEqual(t, res.StatusCode, http.StatusOK, "the same prefix for other events: "+res.Body)

	// N21: the header is true or false, in any case.
	res = notificationRequest(t, l, testCreds, http.MethodPut, "<NotificationConfiguration/>",
		map[string]string{"X-Amz-Skip-Destination-Validation": "yes"})
	requireBadRequest(t, res, "InvalidArgument",
		"Invalid value for header x-amz-skip-destination-validation")
	res = notificationRequest(t, l, testCreds, http.MethodPut, "<NotificationConfiguration/>",
		map[string]string{"X-Amz-Skip-Destination-Validation": "FALSE"})
	requireEqual(t, res.StatusCode, http.StatusOK, "FALSE: "+res.Body)
}

// testEventMessage is the s3:TestEvent message (S3 User Guide, "Event
// message structure").
type testEventMessage struct {
	Service   string
	Event     string
	Time      string
	Bucket    string
	RequestID string `json:"RequestId"`
	HostID    string `json:"HostId"`
}

// eventTime is the format of an event message's time.
var eventTime = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)

// put sends a PutBucketNotificationConfiguration of doc to notifyBucket,
// with the root key.
func (s *notificationStack) put(t *testing.T, doc string, header map[string]string) *liveResponse {
	t.Helper()
	return notificationRequest(t, s.srv, rootCreds(), http.MethodPut, doc, header)
}

// stored returns notifyBucket's configuration document.
func (s *notificationStack) stored(t *testing.T) string {
	t.Helper()
	return notificationRequest(t, s.srv, rootCreds(), http.MethodGet, "", nil).Body
}

// queuesDoc is a configuration of a queue configuration for each of names,
// each for the created events of keys under its name.
func queuesDoc(names ...string) string {
	doc := "<NotificationConfiguration>"
	for i, n := range names {
		doc += `<QueueConfiguration><Id>q` + strconv.Itoa(i) + `</Id><Queue>` + queueARN(n) +
			`</Queue><Event>s3:ObjectCreated:*</Event>` + prefixFilterOf("prefix", n+"/") +
			`</QueueConfiguration>`
	}
	return doc + "</NotificationConfiguration>"
}

// functionDoc is a configuration of one function configuration of
// function.
func functionDoc(function string) string {
	return `<NotificationConfiguration><CloudFunctionConfiguration><CloudFunction>` +
		functionARN(function) + `</CloudFunction><Event>s3:ObjectCreated:*</Event>` +
		`</CloudFunctionConfiguration></NotificationConfiguration>`
}

// PutBucketNotificationConfiguration validates each destination as S3
// does (its API reference), against the real queues and functions
// services.
func TestDestinationValidation(t *testing.T) {
	s := startNotificationStack(t)
	createBucket(t, s.s3, notifyBucket)
	t.Run("queues", func(t *testing.T) { checkQueueValidation(t, s) })
	t.Run("topics", func(t *testing.T) { checkTopicValidation(t, s) })
	t.Run("functions", func(t *testing.T) { checkFunctionValidation(t, s) })
}

// A queue whose policy lets S3 send is sent one s3:TestEvent, named twice
// or not, which stays in it; a queue that does not, and one that does not
// exist, are refused, each listed with its reason, and nothing is stored;
// skipped, they are stored and sent nothing.
func checkQueueValidation(t *testing.T, s *notificationStack) {
	t.Helper()
	eventsURL := s.createQueue(t, "events", s3QueuePolicy("events", notifyBucket))
	closedURL := s.createQueue(t, "closed", "")

	res := s.put(t, `<NotificationConfiguration>`+
		`<QueueConfiguration><Id>a</Id><Queue>`+queueARN("events")+`</Queue>`+
		`<Event>s3:ObjectCreated:*</Event></QueueConfiguration>`+
		`<QueueConfiguration><Id>b</Id><Queue>`+queueARN("events")+`</Queue>`+
		`<Event>s3:ObjectRemoved:*</Event></QueueConfiguration>`+
		`</NotificationConfiguration>`, nil)
	requireEqual(t, res.StatusCode, http.StatusOK, "put the allowed queue: "+res.Body)
	msgs := s.receiveAll(t, eventsURL)
	requireEqual(t, len(msgs), 1, "test events in the queue")
	var ev testEventMessage
	requireNoErr(t, json.Unmarshal([]byte(aws.ToString(msgs[0].Body)), &ev),
		"decode the test event")
	requireEqual(t, ev, testEventMessage{
		Service: "Amazon S3", Event: "s3:TestEvent", Time: ev.Time, Bucket: notifyBucket,
		RequestID: res.Header.Get("x-amz-request-id"), HostID: res.Header.Get("x-amz-id-2"),
	}, "the test event")
	if !eventTime.MatchString(ev.Time) {
		t.Errorf("Time %q is not the event time format", ev.Time)
	}
	requireEqual(t, msgs[0].Attributes[string(sqstypes.MessageSystemAttributeNameSenderId)],
		"s3.amazonaws.com", "SenderId")
	before := s.stored(t)
	if !strings.Contains(before, queueARN("events")) {
		t.Fatalf("the allowed queue's configuration was not stored: %s", before)
	}

	// The allowed queue's test event, sent first, stays sent.
	res = s.put(t, queuesDoc("events", "closed", "missing"), nil)
	e := requireBadRequest(t, res, "InvalidArgument", unableToValidate)
	requireEqual(t, e, argumentError{
		Code: "InvalidArgument", Message: unableToValidate,
		ArgumentName1: queueARN("closed"),
		ArgumentValue1: "Permissions on the destination queue do not allow S3 to publish " +
			"notifications from this bucket",
		ArgumentName2: queueARN("missing"), ArgumentValue2: "The destination does not exist",
	}, "the refusal")
	requireEqual(t, s.stored(t), before, "the configuration after a refusal")
	requireEqual(t, len(s.receiveAll(t, eventsURL)), 1, "the allowed queue's second test event")

	res = s.put(t, queuesDoc("closed", "missing"), skipValidation)
	requireEqual(t, res.StatusCode, http.StatusOK, "put with skip: "+res.Body)
	requireEqual(t, len(s.receiveAll(t, closedURL)), 0, "messages in the closed queue")
	if got := s.stored(t); !strings.Contains(got, queueARN("missing")) {
		t.Fatalf("the skipped configuration was not stored: %s", got)
	}
}

// A topic never validates, as there is no topic service; skipped, it is
// stored.
func checkTopicValidation(t *testing.T, s *notificationStack) {
	t.Helper()
	doc := `<NotificationConfiguration><TopicConfiguration><Topic>` + rtTopic +
		`</Topic><Event>s3:ObjectCreated:*</Event></TopicConfiguration>` +
		`</NotificationConfiguration>`
	e := requireBadRequest(t, s.put(t, doc, nil), "InvalidArgument", unableToValidate)
	requireEqual(t, e.ArgumentName1, rtTopic, "the topic's ArgumentName1")
	requireEqual(t, e.ArgumentValue1, "The destination does not exist", "the topic's reason")
	requireEqual(t, s.put(t, doc, skipValidation).StatusCode, http.StatusOK, "put with skip")
	if got := s.stored(t); !strings.Contains(got, "<Topic>"+rtTopic+"</Topic>") {
		t.Fatalf("the skipped topic was not stored: %s", got)
	}
}

// A function without a permission for S3 is refused; with one, it
// validates with a dry run and is never invoked, Active or Failed; a
// missing one is refused.
func checkFunctionValidation(t *testing.T, s *notificationStack) {
	t.Helper()
	s.createFunction(t, "recorder")
	e := requireBadRequest(t, s.put(t, functionDoc("recorder"), nil), "InvalidArgument",
		unableToValidate)
	requireEqual(t, e.ArgumentName1, functionARN("recorder"), "the function's ArgumentName1")
	requireEqual(t, e.ArgumentValue1,
		"Not authorized to invoke function ["+functionARN("recorder")+"]", "the reason")

	s.allowS3(t, "recorder", notifyBucket)
	res := s.put(t, functionDoc("recorder"), nil)
	requireEqual(t, res.StatusCode, http.StatusOK, "put the permitted function: "+res.Body)
	requireEqual(t, s.invocations(t, "recorder"), 0, "invocations of the validated function")

	e = requireBadRequest(t, s.put(t, functionDoc("missing"), nil), "InvalidArgument",
		unableToValidate)
	requireEqual(t, e.ArgumentValue1, "The destination does not exist", "a missing function")

	// A function that cannot be invoked in its state, with the permission,
	// validates: S3 checks the permission alone.
	s.createFailedFunction(t, "broken")
	e = requireBadRequest(t, s.put(t, functionDoc("broken"), nil), "InvalidArgument",
		unableToValidate)
	requireEqual(t, e.ArgumentValue1,
		"Not authorized to invoke function ["+functionARN("broken")+"]", "a Failed function")
	s.allowS3(t, "broken", notifyBucket)
	res = s.put(t, functionDoc("broken"), nil)
	requireEqual(t, res.StatusCode, http.StatusOK, "put the permitted Failed function: "+res.Body)
}

// An object-store with no queues or functions endpoint refuses their
// destinations with the setting to set, and one whose endpoints do not
// answer refuses them as not reached; skipped, they are stored.
func TestDestinationValidationWithoutTheServices(t *testing.T) {
	queueDoc := queueARNConfig(queueARN("events"))
	t.Run("unconfigured", func(t *testing.T) {
		l, _ := startNotificationServer(t)
		for doc, setting := range map[string]string{
			queueDoc:                "TULE_Storage_QueuesEndpoint",
			functionDoc("recorder"): "TULE_Storage_FunctionsEndpoint",
		} {
			res := notificationRequest(t, l, testCreds, http.MethodPut, doc, nil)
			e := requireBadRequest(t, res, "InvalidArgument", unableToValidate)
			requireEqual(t, e.ArgumentValue1,
				"The destination's service is not configured in this Tule ("+setting+")",
				"the reason")
			res = notificationRequest(t, l, testCreds, http.MethodPut, doc, skipValidation)
			requireEqual(t, res.StatusCode, http.StatusOK, "put with skip: "+res.Body)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		nowhere := "http://127.0.0.1:" + strconv.Itoa(freePort(t))
		l := startLive(t, map[string]string{
			"TULE_Storage_QueuesEndpoint":    nowhere,
			"TULE_Storage_FunctionsEndpoint": nowhere,
		})
		createBucket(t, l.testClient(), notifyBucket)
		for _, doc := range []string{queueDoc, functionDoc("recorder")} {
			res := notificationRequest(t, l, testCreds, http.MethodPut, doc, nil)
			e := requireBadRequest(t, res, "InvalidArgument", unableToValidate)
			requireEqual(t, e.ArgumentValue1, "The destination could not be reached", "the reason")
		}
	})
}
