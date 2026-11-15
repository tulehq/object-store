package notification

import (
	"bytes"
	"testing"
)

func TestEncodeIsCanonical(t *testing.T) {
	c, err := Parse([]byte(doc(
		"<EventBridgeConfiguration/>",
		"<CloudFunctionConfiguration><Event>s3:ObjectRemoved:*</Event><Id>f</Id>"+
			"<CloudFunction>"+functionARN+"</CloudFunction></CloudFunctionConfiguration>",
		"<QueueConfiguration><Filter><S3Key><FilterRule><Name>Suffix</Name><Value>.jpg</Value>"+
			"</FilterRule><FilterRule><Name>prefix</Name><Value>a&amp;b/</Value></FilterRule>"+
			"</S3Key></Filter><Queue>"+queueARN+"</Queue><Event>s3:ObjectCreated:*</Event>"+
			"<Event>s3:ObjectTagging:Put</Event><Id>q</Id></QueueConfiguration>",
		"<TopicConfiguration><Topic>"+topicARN+"</Topic><Id>t</Id>"+
			"<Event>s3:ReducedRedundancyLostObject</Event></TopicConfiguration>",
	)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Encode("g1", c)
	if err != nil {
		t.Fatal(err)
	}
	s, decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	const want = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`<TopicConfiguration><Id>t</Id><Topic>` + topicARN + `</Topic>` +
		`<Event>s3:ReducedRedundancyLostObject</Event></TopicConfiguration>` +
		`<QueueConfiguration><Id>q</Id><Queue>` + queueARN + `</Queue>` +
		`<Event>s3:ObjectCreated:*</Event><Event>s3:ObjectTagging:Put</Event>` +
		`<Filter><S3Key><FilterRule><Name>suffix</Name><Value>.jpg</Value></FilterRule>` +
		`<FilterRule><Name>prefix</Name><Value>a&amp;b/</Value></FilterRule></S3Key></Filter>` +
		`</QueueConfiguration>` +
		`<CloudFunctionConfiguration><Id>f</Id><CloudFunction>` + functionARN +
		`</CloudFunction><Event>s3:ObjectRemoved:*</Event></CloudFunctionConfiguration>` +
		`<EventBridgeConfiguration></EventBridgeConfiguration></NotificationConfiguration>`
	if s.Document != want || s.Generation != "g1" {
		t.Errorf("stored %+v\nwant document\n%s", s, want)
	}
	requireStable(t, raw, s, decoded)
}

// requireStable checks that raw, the stored form of a configuration that
// decoded as s and decoded, encodes again to itself from decoded and from
// its document parsed again.
func requireStable(t *testing.T, raw []byte, s *Stored, decoded *Configuration) {
	t.Helper()
	again, err := Encode(s.Generation, decoded)
	if err != nil || !bytes.Equal(again, raw) {
		t.Errorf("re-encoding the decoded configuration changed it (%v):\n%s", err, again)
	}
	reparsed, err := Parse([]byte(s.Document))
	if err != nil {
		t.Fatalf("the stored document does not parse: %v", err)
	}
	third, err := Encode(s.Generation, reparsed)
	if err != nil || !bytes.Equal(third, raw) {
		t.Errorf("the stored document does not parse back to itself (%v):\n%s", err, third)
	}
}

func TestEncodeEmpty(t *testing.T) {
	c, err := Parse([]byte(doc()))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Encode("g", c)
	if err != nil {
		t.Fatal(err)
	}
	s, decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	const want = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`</NotificationConfiguration>`
	if s.Document != want || !Empty(decoded) {
		t.Errorf("document %s, %+v", s.Document, decoded)
	}
}

func TestDecodeRefusesACorruptFile(t *testing.T) {
	for name, raw := range map[string][]byte{
		"not JSON":         []byte("<x>"),
		"not XML inside":   []byte(`{"generation":"g","document":"<NotificationConfiguration>"}`),
		"an empty file":    nil,
		"a wrong document": []byte(`{"generation":"g","document":"<Other></Other>"}`),
	} {
		if _, _, err := Decode(raw); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}
