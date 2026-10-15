package object

import "testing"

func TestParseCopySource(t *testing.T) {
	tests := []struct {
		in, bucket, key string
		wantErr         bool
	}{
		{in: "src/a/b.txt", bucket: "src", key: "a/b.txt"},
		{in: "/src/a%20b%3Dc.txt", bucket: "src", key: "a b=c.txt"},
		{in: "src/k?versionId=abc", wantErr: true},
		{in: "src", wantErr: true},
		{in: "src/", wantErr: true},
		{in: "src/%zz", wantErr: true},
	}

	for _, tt := range tests {
		bucket, key, err := parseCopySource(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got %q %q", tt.in, bucket, key)
			}
			continue
		}
		if err != nil || bucket != tt.bucket || key != tt.key {
			t.Errorf("%q: got %q %q %v, want %q %q", tt.in, bucket, key, err, tt.bucket, tt.key)
		}
	}
}
