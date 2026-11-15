package bucket

import (
	"testing"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// A Deleted entry takes the shape of the DeleteObjects API reference's
// examples for what the delete did.
func TestDeletedShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  s3api.ObjectIdentifier
		out  contracts.DeleteObjectOutput
		want s3api.DeletedObject
	}{
		{
			name: "unversioned",
			obj:  s3api.ObjectIdentifier{Key: "k"},
			want: s3api.DeletedObject{Key: "k"},
		},
		{
			name: "unversioned by version",
			obj:  s3api.ObjectIdentifier{Key: "k", VersionID: "null"},
			want: s3api.DeletedObject{Key: "k"},
		},
		{
			name: "marker created",
			obj:  s3api.ObjectIdentifier{Key: "k"},
			out:  contracts.DeleteObjectOutput{VersionID: "M", DeleteMarker: true},
			want: s3api.DeletedObject{Key: "k", DeleteMarker: true, DeleteMarkerVersionID: "M"},
		},
		{
			name: "version removed",
			obj:  s3api.ObjectIdentifier{Key: "k", VersionID: "V"},
			out:  contracts.DeleteObjectOutput{VersionID: "V"},
			want: s3api.DeletedObject{Key: "k", VersionID: "V"},
		},
		{
			name: "marker removed",
			obj:  s3api.ObjectIdentifier{Key: "k", VersionID: "M"},
			out:  contracts.DeleteObjectOutput{VersionID: "M", DeleteMarker: true},
			want: s3api.DeletedObject{
				Key: "k", VersionID: "M", DeleteMarker: true, DeleteMarkerVersionID: "M",
			},
		},
	} {
		if got := deleted(tc.obj, &tc.out); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
