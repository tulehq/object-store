package notification

import (
	"net/url"
	"testing"

	"tule.dev/object-store/internal/contracts"
)

func TestEncodeKey(t *testing.T) {
	for key, want := range map[string]string{
		"HappyFace.jpg":          "HappyFace.jpg",
		"red flower.jpg":         "red+flower.jpg",
		"uploads/red flower.jpg": "uploads/red+flower.jpg",
		"a b/c+d=e.txt":          "a+b/c%2Bd%3De.txt",
		"a+b":                    "a%2Bb",
		"/lead//trail/":          "/lead//trail/",
		"x?y&z#w%":               "x%3Fy%26z%23w%25",
		"-_.~":                   "-_.~",
		"café/日本.txt":            "caf%C3%A9/%E6%97%A5%E6%9C%AC.txt",
		"tab\there":              "tab%09here",
	} {
		got := EncodeKey(key)
		if got != want {
			t.Errorf("%q: %q, want %q", key, got, want)
		}
		// aws-lambda-go decodes the key with url.QueryUnescape.
		if back, err := url.QueryUnescape(got); err != nil || back != key {
			t.Errorf("%q: decodes to %q (%v)", key, back, err)
		}
	}
}

func TestMatch(t *testing.T) {
	c, err := Parse([]byte(doc(
		"<TopicConfiguration><Id>t</Id>"+ev("s3:ObjectTagging:*")+
			"<Topic>"+topicARN+"</Topic></TopicConfiguration>",
		queue("<Id>spaces</Id>"+ev("s3:ObjectCreated:*")+
			filter(filterRule("prefix", "my+photos/"), filterRule("suffix", ".jpg"))),
		queue("<Id>plus</Id>"+ev("s3:ObjectCreated:*")+
			filter(filterRule("prefix", "c%2B%2B/"))),
		"<CloudFunctionConfiguration><Id>f</Id>"+ev("s3:ObjectRemoved:Delete")+
			"<CloudFunction>"+functionARN+"</CloudFunction></CloudFunctionConfiguration>",
	)))
	if err != nil {
		t.Fatal(err)
	}
	queueTo := func(id string) contracts.Destination {
		return contracts.Destination{
			Type: contracts.DestinationQueue, ARN: queueARN,
			ConfigurationID: id,
		}
	}
	none := contracts.Destination{}
	for _, tc := range []struct {
		name contracts.EventName
		key  string
		want contracts.Destination
	}{
		{contracts.EventObjectCreatedPut, "my photos/cat.jpg", queueTo("spaces")},
		{contracts.EventObjectCreatedCopy, "my photos/a/b c.jpg", queueTo("spaces")},
		{contracts.EventObjectCreatedPut, "my+photos/cat.jpg", none},
		{contracts.EventObjectCreatedPut, "my photos/cat.png", none},
		{contracts.EventObjectCreatedPut, "c++/main.cc", queueTo("plus")},
		{contracts.EventObjectCreatedPut, "c%2B%2B/main.cc", none},
		{contracts.EventObjectRemovedDelete, "anything", contracts.Destination{
			Type: contracts.DestinationFunction, ARN: functionARN, ConfigurationID: "f",
		}},
		{contracts.EventObjectRemovedDeleteMarkerCreated, "anything", none},
		{contracts.EventLifecycleExpirationDelete, "anything", none},
		{contracts.EventObjectTaggingDelete, "k", contracts.Destination{
			Type: contracts.DestinationTopic, ARN: topicARN, ConfigurationID: "t",
		}},
		{contracts.EventObjectACLPut, "k", none},
	} {
		got, ok := c.Match(tc.name, tc.key)
		if got != tc.want || ok != (tc.want != none) {
			t.Errorf("%s %q: %+v %v, want %+v", tc.name, tc.key, got, ok, tc.want)
		}
	}
}
