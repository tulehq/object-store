package s3api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"tule.dev/object-store/internal/contracts"
)

func TestRequestTags(t *testing.T) {
	for _, tt := range []struct {
		header string
		code   string
		want   []contracts.Tag
	}{
		{header: ""},
		{header: "a=1&b=2", want: []contracts.Tag{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}}},
		{header: "a%20b=c+d&e=", want: []contracts.Tag{{Key: "a b", Value: "c d"}, {Key: "e"}}},
		{header: "a&&b=1", want: []contracts.Tag{{Key: "a"}, {Key: "b", Value: "1"}}},
		{header: "a=1&a=2", code: "InvalidArgument"},
		{header: "a=%zz", code: "InvalidArgument"},
		{header: "aws%3Akey=1", code: "InvalidTag"},
		{header: "=1", code: "InvalidTag"},
		{header: "a=%E2%80%A2", code: "InvalidTag"},
		{header: strings.Repeat("k", 129) + "=1", code: "InvalidTag"},
		{header: "k=" + strings.Repeat("v", 257), code: "InvalidTag"},
		{header: "0&1&2&3&4&5&6&7&8&9", want: tagKeys("0123456789")},
		{header: "0&1&2&3&4&5&6&7&8&9&x", code: "InvalidTag"},
	} {
		r := &http.Request{Header: http.Header{}}
		if tt.header != "" {
			r.Header.Set(HeaderTagging, tt.header)
		}
		got, err := RequestTags(r)
		requireCode(t, tt.header, err, tt.code)
		if tt.code == "" && !slices.Equal(got, tt.want) {
			t.Errorf("%q: %v, want %v", tt.header, got, tt.want)
		}
	}
}

func TestParseObjectTagging(t *testing.T) {
	doc := func(tags ...string) []byte {
		return []byte("<Tagging><TagSet>" + strings.Join(tags, "") + "</TagSet></Tagging>")
	}
	tag := func(k, v string) string {
		return "<Tag><Key>" + k + "</Key><Value>" + v + "</Value></Tag>"
	}
	many := make([]string, 11)
	for i := range many {
		many[i] = tag(string(rune('a'+i)), "v")
	}
	for _, tt := range []struct {
		name string
		code string
		doc  []byte
		want int
	}{
		{name: "empty", doc: doc()},
		{name: "two", doc: doc(tag("a", "1"), tag("b", "")), want: 2},
		{name: "ten", doc: doc(many[:10]...), want: 10},
		{name: "eleven", doc: doc(many...), code: "InvalidTag"},
		{name: "duplicate", doc: doc(tag("a", "1"), tag("a", "2")), code: "InvalidTag"},
		{name: "reserved", doc: doc(tag("AWS:x", "1")), code: "InvalidTag"},
		{name: "no TagSet", doc: []byte("<Tagging/>"), code: "MalformedXML"},
		{name: "not XML", doc: []byte("<Tagging>"), code: "MalformedXML"},
	} {
		got, err := ParseObjectTagging(tt.doc)
		requireCode(t, tt.name, err, tt.code)
		if tt.code == "" && len(got) != tt.want {
			t.Errorf("%s: %d tags, want %d", tt.name, len(got), tt.want)
		}
	}
}

func tagKeys(keys string) []contracts.Tag {
	tags := make([]contracts.Tag, len(keys))
	for i, k := range keys {
		tags[i] = contracts.Tag{Key: string(k)}
	}
	return tags
}

// requireCode fails unless err is the S3 error code, or nil for "".
func requireCode(t *testing.T, what string, err error, code string) {
	t.Helper()
	if code == "" {
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
		return
	}
	s3Err, ok := err.(*Error) //nolint:errorlint // the parsers return *Error unwrapped
	if !ok || s3Err.Code != code {
		t.Errorf("%s: %v, want %s", what, err, code)
	}
}
