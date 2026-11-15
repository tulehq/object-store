package notification

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"tule.dev/object-store/internal/s3api"
)

func doc(configs ...string) string {
	return `<NotificationConfiguration xmlns="` + xmlns + `">` + strings.Join(configs, "") +
		`</NotificationConfiguration>`
}

const (
	queueARN    = "arn:aws:sqs:us-east-1:000000000000:uploads"
	functionARN = "arn:aws:lambda:us-east-1:000000000000:function:thumbs"
	topicARN    = "arn:aws:sns:us-east-1:000000000000:alerts"

	mx = "MalformedXML"
	ia = "InvalidArgument"
)

// queue is a QueueConfiguration with inner, which has no Queue element.
func queue(inner string) string {
	return "<QueueConfiguration>" + inner + "<Queue>" + queueARN + "</Queue></QueueConfiguration>"
}

func ev(name string) string {
	return "<Event>" + name + "</Event>"
}

func filter(rules ...string) string {
	return "<Filter><S3Key>" + strings.Join(rules, "") + "</S3Key></Filter>"
}

func filterRule(name, value string) string {
	return "<FilterRule><Name>" + name + "</Name><Value>" + value + "</Value></FilterRule>"
}

// requireError checks that err is a 400 S3 error with code and message;
// "" admits any message.
func requireError(t *testing.T, err error, code, message string) {
	t.Helper()
	var s3Err *s3api.Error
	if !errors.As(err, &s3Err) {
		t.Fatalf("got %v, want %s", err, code)
	}
	if s3Err.Status != http.StatusBadRequest || s3Err.Code != code ||
		message != "" && s3Err.Message != message {
		t.Fatalf("got %d %s %q, want 400 %s %q", s3Err.Status, s3Err.Code, s3Err.Message,
			code, message)
	}
}

// requireEqual reports each pair of got and want values that differ.
func requireEqual(t *testing.T, what string, pairs [][2]any) {
	t.Helper()
	for i, p := range pairs {
		if p[0] != p[1] {
			t.Errorf("%s %d: got %v, want %v", what, i, p[0], p[1])
		}
	}
}

const put = "s3:ObjectCreated:Put"

// refuseCases are the documents Parse refuses (design table V1 to V9),
// with the code and message; "" is the standard MalformedXML message.
var refuseCases = []struct {
	name    string
	body    string
	code    string
	message string
}{
	{"V1 not XML", "not xml", mx, ""},
	{"V1 wrong root", "<Foo>" + queue(ev(put)) + "</Foo>", mx, ""},
	{"V1 no Queue", doc("<QueueConfiguration>" + ev(put) + "</QueueConfiguration>"), mx, ""},
	{
		"V1 no Topic",
		doc("<TopicConfiguration>" + ev(put) + "<Queue>" + queueARN + "</Queue>" +
			"</TopicConfiguration>"), mx, "",
	},
	{
		"V1 no CloudFunction",
		doc("<CloudFunctionConfiguration>" + ev(put) + "</CloudFunctionConfiguration>"), mx, "",
	},
	{"V1 no Event", doc(queue("<Id>a</Id>")), mx, ""},
	{
		"V1 two Queue elements",
		doc("<QueueConfiguration>" + ev(put) + "<Queue>" + queueARN + "</Queue><Queue>" +
			queueARN + "</Queue></QueueConfiguration>"), mx, "",
	},
	{"V1 two Ids", doc(queue("<Id>a</Id><Id>b</Id>" + ev(put))), mx, ""},
	{"V1 two Filters", doc(queue(ev(put) + filter() + filter())), mx, ""},
	{
		"V1 two S3Keys",
		doc(queue(ev(put) + "<Filter><S3Key></S3Key><S3Key></S3Key></Filter>")), mx, "",
	},
	{
		"V1 a repeated rule Name",
		doc(queue(ev(put) + filter("<FilterRule><Name>prefix</Name><Name>suffix</Name>"+
			"<Value>a</Value></FilterRule>"))), mx, "",
	},
	{
		"V1 two EventBridgeConfiguration",
		doc("<EventBridgeConfiguration/><EventBridgeConfiguration></EventBridgeConfiguration>"),
		mx, "",
	},
	{
		"V1 a shape error in a later configuration wins over an earlier one's check",
		doc(queue(ev("s3:Nope")), "<QueueConfiguration>"+ev(put)+"</QueueConfiguration>"),
		mx, "",
	},
	{"V2 unknown event", doc(queue(ev("s3:ObjectCreated:Nope"))), ia, msgEvent},
	{"V2 s3:TestEvent", doc(queue(ev("s3:TestEvent"))), ia, msgEvent},
	{"V2 no s3: prefix", doc(queue(ev("ObjectCreated:Put"))), ia, msgEvent},
	{"V2 wrong case", doc(queue(ev("s3:objectcreated:put"))), ia, msgEvent},
	{"V2 a bare wildcard", doc(queue(ev("s3:*"))), ia, msgEvent},
	{
		"V2 a family without a wildcard",
		doc(queue(ev("s3:LifecycleTransition:*"))), ia, msgEvent,
	},
	{"V3 rule name", doc(queue(ev(put) + filter(filterRule("middle", "a")))), ia, msgRuleName},
	{"V3 missing rule name", doc(queue(ev(put) + filter("<FilterRule><Value>a</Value>"+
		"</FilterRule>"))), ia, msgRuleName},
	{
		"V4 two prefixes",
		doc(queue(ev(put) + filter(filterRule("prefix", "a"), filterRule("Prefix", "b")))),
		ia, msgTwoPrefixes,
	},
	{
		"V4 two suffixes",
		doc(queue(ev(put) + filter(filterRule("suffix", "a"), filterRule("SUFFIX", "b")))),
		ia, msgTwoSuffixes,
	},
	{
		"V5 value over 1024",
		doc(queue(ev(put) + filter(filterRule("prefix", strings.Repeat("a", 1025))))),
		ia, msgValueTooLong,
	},
	{
		"V5 value over 1024 characters",
		doc(queue(ev(put) + filter(filterRule("prefix", strings.Repeat("é", 1025))))),
		ia, msgValueTooLong,
	},
	{
		"V6 Id over 255", doc(queue("<Id>" + strings.Repeat("i", 256) + "</Id>" + ev(put))),
		ia, msgIDTooLong,
	},
	{
		"V6 duplicate Id across kinds",
		doc(queue("<Id>same</Id>"+ev(put)), "<CloudFunctionConfiguration><Id>same</Id>"+
			ev("s3:ObjectRemoved:*")+"<CloudFunction>"+functionARN+"</CloudFunction>"+
			"</CloudFunctionConfiguration>"),
		ia, msgDuplicateID,
	},
	{"V7 not an ARN", queueWith("uploads"), ia, msgARN},
	{"V7 wrong service", queueWith(topicARN), ia, msgARN},
	{"V7 wrong partition", queueWith("arn:aws-cn:sqs:us-east-1:000000000000:q"), ia, msgARN},
	{"V7 short account", queueWith("arn:aws:sqs:us-east-1:0000:q"), ia, msgARN},
	{"V7 bad queue name", queueWith("arn:aws:sqs:us-east-1:000000000000:a/b"), ia, msgARN},
	{
		"V7 a function ARN without function:",
		doc("<CloudFunctionConfiguration>" + ev(put) +
			"<CloudFunction>arn:aws:lambda:us-east-1:000000000000:thumbs</CloudFunction>" +
			"</CloudFunctionConfiguration>"),
		ia, msgARN,
	},
	{"V8 another region", queueWith("arn:aws:sqs:us-west-2:000000000000:q"), ia, msgRegion},
	{"V8 a FIFO queue", queueWith("arn:aws:sqs:us-east-1:000000000000:q.fifo"), ia, msgFIFO},
	{
		"V8 a FIFO topic",
		doc("<TopicConfiguration>" + ev(put) +
			"<Topic>arn:aws:sns:us-east-1:000000000000:t.fifo</Topic></TopicConfiguration>"),
		ia, msgFIFO,
	},
	{
		"V9 overlap across destination types",
		doc(queue(ev("s3:ObjectCreated:*")), "<CloudFunctionConfiguration>"+ev(put)+
			"<CloudFunction>"+functionARN+"</CloudFunction></CloudFunctionConfiguration>"),
		ia, msgOverlap,
	},
	{
		"V2 in document order before a later V9",
		doc(queue(ev(put)), queue(ev(put)), queue(ev("s3:Nope"))), ia, msgEvent,
	},
}

func queueWith(arn string) string {
	return doc("<QueueConfiguration>" + ev(put) + "<Queue>" + arn + "</Queue></QueueConfiguration>")
}

func TestParseRefuses(t *testing.T) {
	for _, tc := range refuseCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			requireError(t, err, tc.code, tc.message)
		})
	}
}

func TestParseRefusesADocumentOverTheLimit(t *testing.T) {
	_, err := Parse([]byte(doc(strings.Repeat(" ", MaxDocument))))
	requireError(t, err, "MaxMessageLengthExceeded", "")
}

func TestARNErrorsNameTheElementAndValue(t *testing.T) {
	for _, tc := range []struct {
		body, name, value string
	}{
		{queueWith("nope"), "Queue", "nope"},
		{
			queueWith("arn:aws:sqs:eu-west-1:000000000000:q"), "Queue",
			"arn:aws:sqs:eu-west-1:000000000000:q",
		},
		{doc("<CloudFunctionConfiguration>" + ev(put) + "<CloudFunction>" + queueARN +
			"</CloudFunction></CloudFunctionConfiguration>"), "CloudFunction", queueARN},
		{
			doc("<TopicConfiguration>" + ev(put) + "<Topic>x</Topic></TopicConfiguration>"),
			"Topic", "x",
		},
	} {
		_, err := Parse([]byte(tc.body))
		var s3Err *s3api.Error
		if !errors.As(err, &s3Err) || len(s3Err.Arguments) != 1 ||
			s3Err.Arguments[0] != (s3api.ErrorArgument{Name: tc.name, Value: tc.value}) {
			t.Errorf("%s: got %#v", tc.value, err)
		}
	}
}

func TestParseAccepts(t *testing.T) {
	c, err := Parse([]byte(doc(
		"<CloudFunctionConfiguration><Id>f</Id>"+ev("s3:ObjectRemoved:*")+
			"<CloudFunction>"+functionARN+":live</CloudFunction>"+
			"<InvocationRole>arn:aws:iam::000000000000:role/old</InvocationRole>"+
			"</CloudFunctionConfiguration>",
		queue("<Id>q</Id>"+ev(put)+ev("s3:ObjectTagging:*")+
			filter(
				filterRule("Prefix", "a+b/"),
				filterRule("SUFFIX", ".jpg"),
			)+"<Unknown>x</Unknown>"),
		"<EventBridgeConfiguration></EventBridgeConfiguration>",
		"<TopicConfiguration><Id>t</Id>"+ev("s3:ReducedRedundancyLostObject")+
			"<Topic>"+topicARN+"</Topic></TopicConfiguration>",
		"<Other/>",
	)))
	if err != nil {
		t.Fatal(err)
	}
	requireEqual(t, "counts", [][2]any{
		{len(c.Topics), 1}, {len(c.Queues), 1}, {len(c.Functions), 1}, {c.EventBridge != nil, true},
	})
	q := c.Queues[0]
	if q.Filter == nil {
		t.Fatalf("queue %+v", q)
	}
	requireEqual(t, "fields", [][2]any{
		{c.Topics[0].ARN, topicARN},
		{c.Functions[0].ARN, functionARN + ":live"},
		{c.Functions[0].ID, "f"},
		{q.ID, "q"},
		{len(q.Events), 2},
		{fmt.Sprint(q.Filter.S3Key.Rules), "[{prefix a+b/} {suffix .jpg}]"},
	})
}

func TestParseEmptyDisables(t *testing.T) {
	for _, body := range []string{
		doc(), "<NotificationConfiguration/>", "<NotificationConfiguration>\n</NotificationConfiguration>",
	} {
		c, err := Parse([]byte(body))
		if err != nil || !Empty(c) {
			t.Errorf("%s: %+v, %v", body, c, err)
		}
	}
	c, err := Parse([]byte(doc("<EventBridgeConfiguration/>")))
	if err != nil || Empty(c) {
		t.Errorf("EventBridge only: %+v, %v", c, err)
	}
}

// TestParseCountsFilterValuesInCharacters pins V5's limit in characters,
// not bytes: 1,024 two-byte characters are accepted.
func TestParseCountsFilterValuesInCharacters(t *testing.T) {
	value := strings.Repeat("é", MaxFilterValue)
	c, err := Parse([]byte(doc(queue(ev(put) + filter(filterRule("prefix", value))))))
	if err != nil || c.Queues[0].Filter.S3Key.Rules[0].Value != value {
		t.Fatalf("%v", err)
	}
}

func TestParseDropsAFilterWithoutRules(t *testing.T) {
	for _, f := range []string{"<Filter/>", "<Filter><S3Key/></Filter>"} {
		c, err := Parse([]byte(doc(queue(ev(put) + f))))
		if err != nil || c.Queues[0].Filter != nil {
			t.Errorf("%s: %+v, %v", f, c, err)
		}
	}
}

// uuidText is a UUID's text form, version 4.
var uuidText = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
)

func TestParseGeneratesBase64UUIDIds(t *testing.T) {
	c, err := Parse([]byte(doc(
		queue(ev(put)),
		queue("<Id></Id>"+ev("s3:ObjectRemoved:*")),
		queue("<Id>kept</Id>"+ev("s3:ObjectTagging:*")),
	)))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, q := range c.Queues[:2] {
		raw, err := base64.StdEncoding.DecodeString(q.ID)
		if err != nil || !uuidText.Match(raw) || seen[q.ID] {
			t.Errorf("queue %d Id %q decodes to %q (%v)", i, q.ID, raw, err)
		}
		seen[q.ID] = true
	}
	if c.Queues[2].ID != "kept" {
		t.Errorf("Id %q", c.Queues[2].ID)
	}
	// The API reference's sample Id is of the same form.
	raw, _ := base64.StdEncoding.DecodeString("YjVkM2Y0YmUtNGI3NC00ZjQyLWEwNGItNDIyYWUxY2I0N2M4")
	if !uuidText.Match(raw) {
		t.Errorf("sample Id decodes to %q", raw)
	}
}

func TestParseSkip(t *testing.T) {
	for h, want := range map[string]bool{
		"": false, "false": false, "FALSE": false,
		"true": true, "True": true,
	} {
		got, err := ParseSkip(h)
		if err != nil || got != want {
			t.Errorf("%q: %v, %v", h, got, err)
		}
	}
	for _, h := range []string{"yes", "1", " true"} {
		_, err := ParseSkip(h)
		requireError(t, err, ia, msgSkip)
	}
}
