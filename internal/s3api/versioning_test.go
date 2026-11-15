package s3api

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

func TestVersionID(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  string
		code  string
	}{
		{query: "", want: ""},
		{query: "versionId=abc", want: "abc"},
		{query: "versionId=null", want: "null"},
		{query: "versionId=", code: "InvalidArgument"},
		{query: "versionId", code: "InvalidArgument"},
	} {
		got, err := VersionID(queryRequest(t, tc.query))
		var s3Err *Error
		switch {
		case tc.code == "" && err != nil:
			t.Errorf("%q: %v", tc.query, err)
		case tc.code != "" && (!errors.As(err, &s3Err) || s3Err.Code != tc.code):
			t.Errorf("%q: error %v, want %s", tc.query, err, tc.code)
		case got != tc.want:
			t.Errorf("%q: %q, want %q", tc.query, got, tc.want)
		}
	}
}

func TestParseVersioningConfiguration(t *testing.T) {
	doc := func(inner string) []byte {
		return []byte(`<VersioningConfiguration xmlns="` + xmlns + `">` + inner +
			`</VersioningConfiguration>`)
	}
	for _, tc := range []struct {
		want contracts.VersioningConfig
		doc  []byte
		bad  bool
	}{
		{doc: doc(`<Status>Enabled</Status>`), want: contracts.VersioningConfig{
			Status: contracts.VersioningEnabled,
		}},
		{
			doc: doc(`<Status>Suspended</Status><MfaDelete>Disabled</MfaDelete>`),
			want: contracts.VersioningConfig{
				Status: contracts.VersioningSuspended, MFADelete: "Disabled",
			},
		},
		// Status and MfaDelete Enabled are the store's to refuse.
		{doc: doc(``), want: contracts.VersioningConfig{}},
		{doc: doc(`<Status>On</Status>`), want: contracts.VersioningConfig{Status: "On"}},
		{
			doc: doc(`<Status>Enabled</Status><MfaDelete>Enabled</MfaDelete>`),
			want: contracts.VersioningConfig{
				Status: contracts.VersioningEnabled, MFADelete: "Enabled",
			},
		},
		{doc: doc(`<Status>Enabled</Status><MfaDelete>On</MfaDelete>`), bad: true},
		{doc: []byte(`<Other><Status>Enabled</Status></Other>`), bad: true},
		{doc: []byte(`<VersioningConfiguration>`), bad: true},
	} {
		got, err := ParseVersioningConfiguration(tc.doc)
		var s3Err *Error
		switch {
		case tc.bad && (!errors.As(err, &s3Err) || s3Err.Code != "MalformedXML"):
			t.Errorf("%s: error %v, want MalformedXML", tc.doc, err)
		case !tc.bad && err != nil:
			t.Errorf("%s: %v", tc.doc, err)
		case !tc.bad && got != tc.want:
			t.Errorf("%s: %+v, want %+v", tc.doc, got, tc.want)
		}
	}
}

// The versions and delete markers of a listing are encoded in the order
// listed, each as its own element, and a marker has no ETag, Size or
// StorageClass.
func TestListVersionsResultKeepsTheOrder(t *testing.T) {
	res := NewListVersionsResult()
	res.Entries = []ListVersionsEntry{
		NewDeleteMarkerEntry("a", "3", "t3", true),
		NewVersionEntry("a", "2", "t2", `"e"`, "STANDARD", 0, false),
		NewDeleteMarkerEntry("a", "1", "t1", false),
	}
	raw, err := xml.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	var order []string
	dec := xml.NewDecoder(strings.NewReader(body))
	for {
		tok, tErr := dec.Token()
		if tErr != nil {
			break
		}
		if se, ok := tok.(xml.StartElement); ok &&
			(se.Name.Local == "Version" || se.Name.Local == "DeleteMarker") {
			order = append(order, se.Name.Local)
		}
	}
	if got := strings.Join(order, ","); got != "DeleteMarker,Version,DeleteMarker" {
		t.Fatalf("order %s in %s", got, body)
	}
	for _, want := range []string{
		`<Version><Key>a</Key><VersionId>2</VersionId>` +
			`<LastModified>t2</LastModified><ETag>&#34;e&#34;</ETag><Size>0</Size>` +
			`<StorageClass>STANDARD</StorageClass><IsLatest>false</IsLatest></Version>`,
		`<DeleteMarker><Key>a</Key><VersionId>3</VersionId><LastModified>t3</LastModified>` +
			`<IsLatest>true</IsLatest></DeleteMarker>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s does not contain %s", body, want)
		}
	}
}

// The versioning sentinels map to their S3 errors, and a missing version,
// which is also a missing object, to NoSuchVersion, not NoSuchKey.
func TestVersioningErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		code   string
		status int
	}{
		{
			fmt.Errorf("x: %w", errors.Join(storage.ErrVersionNotFound, storage.ErrObjectNotFound)),
			"NoSuchVersion", http.StatusNotFound,
		},
		{&contracts.DeleteMarkerError{Current: true}, "NoSuchKey", http.StatusNotFound},
		{&contracts.DeleteMarkerError{}, "MethodNotAllowed", http.StatusMethodNotAllowed},
		{contracts.ErrInvalidVersionID, "InvalidArgument", http.StatusBadRequest},
		{
			contracts.ErrUnversionedState, "IllegalVersioningConfigurationException",
			http.StatusBadRequest,
		},
		{
			fmt.Errorf("x: %w", contracts.ErrInvalidVersioningStatus), "MalformedXML",
			http.StatusBadRequest,
		},
		{
			fmt.Errorf("x: %w", contracts.ErrMFADeleteUnsupported), "NotImplemented",
			http.StatusNotImplemented,
		},
		{
			fmt.Errorf("x: %w", contracts.ErrVersionMarkerWithoutKey), "InvalidArgument",
			http.StatusBadRequest,
		},
	} {
		got := toS3Error(tc.err)
		if got.Code != tc.code || got.Status != tc.status {
			t.Errorf("%v: %s %d, want %s %d", tc.err, got.Code, got.Status, tc.code, tc.status)
		}
	}
}
