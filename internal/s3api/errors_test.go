package s3api

import (
	"encoding/xml"
	"net/http"
	"testing"
)

func TestErrorMarshalsWithoutArguments(t *testing.T) {
	e := ErrInvalidArgument("bad")
	e.BucketName, e.Resource, e.RequestID = "b", "/b", "r1"
	got, err := xml.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	const want = `<Error><Code>InvalidArgument</Code><Message>bad</Message>` +
		`<BucketName>b</BucketName><Resource>/b</Resource><RequestId>r1</RequestId></Error>`
	if string(got) != want {
		t.Errorf("%s\nwant\n%s", got, want)
	}
	empty, err := xml.Marshal(&Error{})
	if err != nil || string(empty) != `<Error><Code></Code><Message></Message></Error>` {
		t.Errorf("%s %v", empty, err)
	}
}

func TestErrorMarshalsNumberedArguments(t *testing.T) {
	e := ErrUnableToValidate([]ErrorArgument{
		{Name: "arn:aws:sqs:us-east-1:000000000000:q", Value: "The destination does not exist"},
		{Name: "arn:aws:lambda:us-east-1:000000000000:function:f", Value: "a<b"},
	})
	e.Key, e.RequestID = "k", "r"
	if e.Status != http.StatusBadRequest || e.Code != "InvalidArgument" {
		t.Fatalf("%+v", e)
	}
	got, err := xml.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	const want = `<Error><Code>InvalidArgument</Code>` +
		`<Message>Unable to validate the following destination configurations</Message>` +
		`<ArgumentName1>arn:aws:sqs:us-east-1:000000000000:q</ArgumentName1>` +
		`<ArgumentValue1>The destination does not exist</ArgumentValue1>` +
		`<ArgumentName2>arn:aws:lambda:us-east-1:000000000000:function:f</ArgumentName2>` +
		`<ArgumentValue2>a&lt;b</ArgumentValue2>` +
		`<Key>k</Key><RequestId>r</RequestId></Error>`
	if string(got) != want {
		t.Errorf("%s\nwant\n%s", got, want)
	}
}

func TestToErrorKeepsArguments(t *testing.T) {
	src := ErrUnableToValidate([]ErrorArgument{{Name: "n", Value: "v"}})
	cp := toS3Error(src)
	if cp == src || len(cp.Arguments) != 1 || cp.Arguments[0].Name != "n" {
		t.Errorf("%+v", cp)
	}
}
