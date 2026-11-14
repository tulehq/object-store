package object

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

func TestParseCopyRange(t *testing.T) {
	for _, tt := range []struct {
		want   *contracts.CopyRange
		values []string
		ok     bool
	}{
		{ok: true},
		{values: []string{"bytes=0-9"}, want: &contracts.CopyRange{First: 0, Last: 9}, ok: true},
		{values: []string{"bytes=5-5"}, want: &contracts.CopyRange{First: 5, Last: 5}, ok: true},
		{values: []string{"bytes=9-0"}},
		{values: []string{"bytes=0-"}},
		{values: []string{"bytes=-5"}},
		{values: []string{"bytes=a-b"}},
		{values: []string{"bytes=+1-2"}},
		{values: []string{"0-9"}},
		{values: []string{"bytes=0-1,3-4"}},
		{values: []string{"bytes=0-1", "bytes=2-3"}},
		{values: []string{"bytes=0-99999999999999999999"}},
	} {
		got, err := parseCopyRange(tt.values)
		if (err == nil) != tt.ok {
			t.Errorf("%q: %v", tt.values, err)
			continue
		}
		if tt.ok && (got == nil) != (tt.want == nil) || got != nil && *got != *tt.want {
			t.Errorf("%q: %+v, want %+v", tt.values, got, tt.want)
		}
	}
}

func TestSourceCondition(t *testing.T) {
	r := &http.Request{Header: http.Header{}}
	r.Header.Set("X-Amz-Copy-Source-If-Match", `"abc"`)
	r.Header.Set("X-Amz-Copy-Source-If-None-Match", "def")
	r.Header.Set("X-Amz-Copy-Source-If-Modified-Since", "Mon, 02 Jan 2006 15:04:05 GMT")
	r.Header.Set("X-Amz-Copy-Source-If-Unmodified-Since", "not a date")
	got := sourceCondition(r)
	want := time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)
	if got.IfMatch != "abc" || got.IfNoneMatch != "def" || got.IfModifiedSince == nil ||
		!got.IfModifiedSince.Equal(want) || got.IfUnmodifiedSince != nil {
		t.Fatalf("%+v", got)
	}
}

func TestRequestedAttributes(t *testing.T) {
	r := &http.Request{Header: http.Header{}}
	if _, err := requestedAttributes(r); err == nil {
		t.Error("no attributes accepted")
	}
	r.Header.Add("X-Amz-Object-Attributes", "ETag, ObjectSize")
	r.Header.Add("X-Amz-Object-Attributes", "ObjectParts")
	got, err := requestedAttributes(r)
	if err != nil || !slices.Equal(got, []string{"ETag", "ObjectSize", "ObjectParts"}) {
		t.Errorf("%v %v", got, err)
	}
	r.Header.Add("X-Amz-Object-Attributes", "Size")
	if _, err = requestedAttributes(r); err == nil {
		t.Error("an unknown attribute accepted")
	}
}

func TestObjectParts(t *testing.T) {
	info := &contracts.ObjectInfo{PartSizes: []int64{10, 20, 30}}
	for _, tt := range []struct {
		parts       []s3api.ObjectPart
		marker, max int
		next        int
		truncated   bool
	}{
		{max: 1000, parts: []s3api.ObjectPart{{PartNumber: 1, Size: 10}, {PartNumber: 2, Size: 20}, {PartNumber: 3, Size: 30}}},
		{max: 2, parts: []s3api.ObjectPart{{PartNumber: 1, Size: 10}, {PartNumber: 2, Size: 20}}, truncated: true, next: 2},
		{marker: 2, max: 2, parts: []s3api.ObjectPart{{PartNumber: 3, Size: 30}}},
		{marker: 3, max: 2},
		{max: 0, truncated: true},
	} {
		got := objectParts(info, tt.marker, tt.max)
		if !slices.Equal(got.Parts, tt.parts) || got.IsTruncated != tt.truncated ||
			got.NextPartNumberMarker != tt.next || got.PartsCount != 3 ||
			got.PartNumberMarker != tt.marker || got.MaxParts != tt.max {
			t.Errorf("marker %d max %d: %+v", tt.marker, tt.max, got)
		}
	}
}
