package object

import "testing"

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
