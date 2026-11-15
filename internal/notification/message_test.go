package notification

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"tule.dev/object-store/internal/contracts"
)

const (
	account = "000000000000"
	owner   = "AQGM2Z5KA6ASUB" // OwnerPrincipalID(account)
)

// at is the design's example time, with sub-millisecond digits that the
// message drops.
var at = time.Date(2026, 11, 15, 12, 30, 0, 123456789, time.FixedZone("EET", 2*60*60))

// putEvent is the design's example: the PUT of "uploads/red flower.jpg"
// (1024 bytes) into the Enabled bucket photos by an IAM user.
func putEvent() *contracts.BucketEvent {
	return &contracts.BucketEvent{
		Time: at,
		Source: contracts.EventSource{
			PrincipalID: "AIDAQ3EGRSXBEXAMPLE1", SourceIP: "127.0.0.1", RequestID: "dev-42",
			HostID: "q2Cs1Bbdw0U5Y0tqJ3yP2u0m0hSSXQ0dGtpYkB2cGxlSGVsbG9Xb3JsZA==",
		},
		Name:      contracts.EventObjectCreatedPut,
		Bucket:    "photos",
		Key:       "uploads/red flower.jpg",
		VersionID: "0PHV8M1J4K2TG5A7Q3RS9MNOPB",
		ETag:      `"d41d8cd98f00b204e9800998ecf8427e"`,
		Sequence:  0x1879A3F2C4B5D600,
		Size:      1024,
		HasObject: true,
	}
}

// goldenPut is the design's §3.4 message, its ownerIdentity the one
// OwnerPrincipalID derives for account (the design's is a placeholder).
const goldenPut = `{"Records":[{"eventVersion":"2.6","eventSource":"aws:s3",` +
	`"awsRegion":"us-east-1","eventTime":"2026-11-15T10:30:00.123Z",` +
	`"eventName":"ObjectCreated:Put","userIdentity":{"principalId":"AIDAQ3EGRSXBEXAMPLE1"},` +
	`"requestParameters":{"sourceIPAddress":"127.0.0.1"},"responseElements":` +
	`{"x-amz-request-id":"dev-42","x-amz-id-2":` +
	`"q2Cs1Bbdw0U5Y0tqJ3yP2u0m0hSSXQ0dGtpYkB2cGxlSGVsbG9Xb3JsZA=="},"s3":` +
	`{"s3SchemaVersion":"1.0","configurationId":"uploads-to-queue","bucket":{"name":"photos",` +
	`"ownerIdentity":{"principalId":"` + owner + `"},"arn":"arn:aws:s3:::photos"},` +
	`"object":{"key":"uploads/red+flower.jpg","size":1024,` +
	`"eTag":"d41d8cd98f00b204e9800998ecf8427e","versionId":"0PHV8M1J4K2TG5A7Q3RS9MNOPB",` +
	`"sequencer":"1879A3F2C4B5D600"}}}]}`

func renderFor(t *testing.T, ev *contracts.BucketEvent) []byte {
	t.Helper()
	r := NewRouter(account, nil)
	b, err := r.Render(contracts.Destination{
		Type: contracts.DestinationQueue, ARN: queueARN, ConfigurationID: "uploads-to-queue",
	}, ev)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRenderGolden(t *testing.T) {
	got := renderFor(t, putEvent())
	if string(got) != goldenPut {
		t.Fatalf("message\n%s\nwant\n%s", got, goldenPut)
	}

	var e events.S3Event
	if err := json.Unmarshal(got, &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Records) != 1 {
		t.Fatalf("%d records", len(e.Records))
	}
	rec := e.Records[0]
	o := rec.S3.Object
	requireEqual(t, "record", [][2]any{
		{rec.EventVersion, "2.6"},
		{rec.EventSource, "aws:s3"},
		{rec.AWSRegion, "us-east-1"},
		{rec.EventTime.Equal(at.Truncate(time.Millisecond)), true},
		{rec.EventName, "ObjectCreated:Put"},
		{rec.PrincipalID.PrincipalID, "AIDAQ3EGRSXBEXAMPLE1"},
		{rec.RequestParameters.SourceIPAddress, "127.0.0.1"},
		{rec.ResponseElements["x-amz-request-id"], "dev-42"},
		{rec.ResponseElements["x-amz-id-2"], putEvent().Source.HostID},
		{rec.S3.SchemaVersion, "1.0"},
		{rec.S3.ConfigurationID, "uploads-to-queue"},
		{rec.S3.Bucket.Name, "photos"},
		{rec.S3.Bucket.Arn, "arn:aws:s3:::photos"},
		{rec.S3.Bucket.OwnerIdentity.PrincipalID, owner},
		{o.Key, "uploads/red+flower.jpg"},
		{o.URLDecodedKey, "uploads/red flower.jpg"},
		{o.Size, int64(1024)},
		{o.ETag, "d41d8cd98f00b204e9800998ecf8427e"},
		{o.VersionID, "0PHV8M1J4K2TG5A7Q3RS9MNOPB"},
		{o.Sequencer, "1879A3F2C4B5D600"},
	})
}

func TestRenderObjectFields(t *testing.T) {
	removal := func(name contracts.EventName, version string, seq int64) *contracts.BucketEvent {
		e := putEvent()
		e.Name, e.VersionID, e.ETag, e.Sequence, e.Size, e.HasObject = name, version, "", seq, 0, false
		return e
	}
	tagging := putEvent()
	tagging.Name, tagging.Sequence, tagging.HasObject = contracts.EventObjectTaggingPut, 0, false
	copied := putEvent()
	copied.Name, copied.VersionID = contracts.EventObjectCreatedCopy, ""
	empty := putEvent()
	empty.Size, empty.Key, empty.Sequence = 0, "e", 1
	for _, tc := range []struct {
		name string
		ev   *contracts.BucketEvent
		want string
	}{
		{
			"unversioned delete", removal(contracts.EventObjectRemovedDelete, "", 7),
			`{"key":"uploads/red+flower.jpg","sequencer":"0000000000000007"}`,
		},
		{
			"marker in a Suspended bucket",
			removal(contracts.EventObjectRemovedDeleteMarkerCreated, "null", 0xABC),
			`{"key":"uploads/red+flower.jpg","versionId":"null","sequencer":"0000000000000ABC"}`,
		},
		{"tagging", tagging, `{"key":"uploads/red+flower.jpg",` +
			`"eTag":"d41d8cd98f00b204e9800998ecf8427e","versionId":"0PHV8M1J4K2TG5A7Q3RS9MNOPB"}`},
		{"copy, unversioned", copied, `{"key":"uploads/red+flower.jpg","size":1024,` +
			`"eTag":"d41d8cd98f00b204e9800998ecf8427e","sequencer":"1879A3F2C4B5D600",` +
			`"hasObjectAnnotation":false}`},
		{"an empty object", empty, `{"key":"e","size":0,` +
			`"eTag":"d41d8cd98f00b204e9800998ecf8427e","versionId":"0PHV8M1J4K2TG5A7Q3RS9MNOPB",` +
			`"sequencer":"0000000000000001"}`},
	} {
		got := string(renderFor(t, tc.ev))
		if !strings.Contains(got, `"object":`+tc.want+`}}]}`) {
			t.Errorf("%s: %s\nwant object %s", tc.name, got, tc.want)
		}
		var e events.S3Event
		if err := json.Unmarshal([]byte(got), &e); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestRenderDoesNotEscapeHTML(t *testing.T) {
	ev := putEvent()
	ev.Source.PrincipalID = "AROA:a<b>&c"
	got := renderFor(t, ev)
	if !strings.Contains(string(got), `"principalId":"AROA:a<b>&c"`) {
		t.Errorf("%s", got)
	}
}

func TestTestEventGolden(t *testing.T) {
	const host = "8cLeGAmw098X5cv4Zkwcmo8vvZa3eH3eKxsPzbB9wrR+YstdA6Knx4Ip8EXAMPLE"
	got, err := TestEvent("photos", "dev-41", host, at)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"Service":"Amazon S3","Event":"s3:TestEvent",` +
		`"Time":"2026-11-15T10:30:00.123Z","Bucket":"photos","RequestId":"dev-41",` +
		`"HostId":"` + host + `"}`
	if string(got) != want {
		t.Fatalf("test event\n%s\nwant\n%s", got, want)
	}
	var e events.S3TestEvent
	if err := json.Unmarshal(got, &e); err != nil {
		t.Fatal(err)
	}
	if e.Service != "Amazon S3" || e.Event != "s3:TestEvent" || e.Bucket != "photos" ||
		e.RequestID != "dev-41" || e.HostID != host ||
		!e.Time.Equal(at.Truncate(time.Millisecond)) {
		t.Errorf("%+v", e)
	}
}

func TestOwnerPrincipalID(t *testing.T) {
	if got := OwnerPrincipalID(account); got != owner {
		t.Errorf("%s", got)
	}
	if OwnerPrincipalID("111111111111") == owner {
		t.Error("two accounts share an owner ID")
	}
}
