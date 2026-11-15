package object

import (
	"errors"
	"net/http"
	"testing"

	"tule.dev/object-store/internal/s3api"
)

func TestParseCopySource(t *testing.T) {
	tests := []struct {
		in, bucket, key, version string
		wantErr                  bool
	}{
		{in: "src/a/b.txt", bucket: "src", key: "a/b.txt"},
		{in: "/src/a%20b%3Dc.txt", bucket: "src", key: "a b=c.txt"},
		{in: "src/k?versionId=abc", bucket: "src", key: "k", version: "abc"},
		{in: "/src/a%3Fb?versionId=null", bucket: "src", key: "a?b", version: "null"},
		{in: "src/k?versionId=", wantErr: true},
		{in: "src/k?versionId=a&versionId=b", wantErr: true},
		{in: "src/k?versionId=a&x=1", wantErr: true},
		{in: "src/k?x=1", wantErr: true},
		{in: "src", wantErr: true},
		{in: "src/", wantErr: true},
		{in: "src/%zz", wantErr: true},
	}

	for _, tt := range tests {
		src, err := parseCopySource(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got %+v", tt.in, src)
			}
			continue
		}
		if err != nil || src.Bucket != tt.bucket || src.Key != tt.key ||
			src.VersionID != tt.version {
			t.Errorf("%q: got %+v %v, want %q %q %q", tt.in, src, err,
				tt.bucket, tt.key, tt.version)
		}
	}
}

// A copy source's query is refused with the status its fault calls for:
// 400 for one that does not decode or an empty versionId, 501 for a query
// other than one versionId.
func TestParseCopySourceQueryErrors(t *testing.T) {
	const bad, unserved = "InvalidArgument", "NotImplemented"
	tests := []struct {
		in, code string
		status   int
	}{
		{in: "src/k?versionId=%zz", status: http.StatusBadRequest, code: bad},
		{in: "src/k?versionId=", status: http.StatusBadRequest, code: bad},
		{in: "src/k?x=1", status: http.StatusNotImplemented, code: unserved},
		{in: "src/k?versionId=a&versionId=b", status: http.StatusNotImplemented, code: unserved},
	}
	for _, tt := range tests {
		_, err := parseCopySource(tt.in)
		var s3Err *s3api.Error
		if !errors.As(err, &s3Err) || s3Err.Status != tt.status || s3Err.Code != tt.code {
			t.Errorf("%q: got %v, want %d %s", tt.in, err, tt.status, tt.code)
		}
	}
}
