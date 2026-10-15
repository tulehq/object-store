package object

import (
	"net/http"
	"net/url"
	"strings"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// copyInput reads a CopyObject request for the object at dst.
func copyInput(r *http.Request, dst s3api.Target) (contracts.CopyObjectInput, error) {
	in := contracts.CopyObjectInput{DstBucket: dst.Bucket, DstKey: dst.Key}

	srcBucket, srcKey, err := parseCopySource(r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		return in, err
	}
	in.SrcBucket, in.SrcKey = srcBucket, srcKey

	if in.Precondition, err = precondition(r); err != nil {
		return in, err
	}

	switch d := r.Header.Get("X-Amz-Metadata-Directive"); d {
	case "", "COPY":
		if srcBucket == dst.Bucket && srcKey == dst.Key {
			return in, s3api.ErrInvalidRequest("This copy request is illegal because it is " +
				"trying to copy an object to itself without changing the object's metadata, " +
				"storage class, website redirect location or encryption attributes.")
		}
	case "REPLACE":
		replace := putInput(r)
		in.Replace = &replace
	default:
		return in, s3api.ErrInvalidArgument("Unknown metadata directive " + d)
	}

	return in, nil
}

// parseCopySource splits an x-amz-copy-source value, "bucket/key" with an
// optional leading slash and a URL-encoded key, into its bucket and key.
func parseCopySource(v string) (bucket, key string, err error) {
	v, query, _ := strings.Cut(v, "?")
	if query != "" {
		return "", "", s3api.ErrNotImplemented(
			"Copying a specific object version is not supported.",
		)
	}

	decoded, uErr := url.PathUnescape(v)
	if uErr != nil {
		return "", "", s3api.ErrInvalidArgument("Invalid copy source encoding")
	}

	bucket, key, ok := strings.Cut(strings.TrimPrefix(decoded, "/"), "/")
	if !ok || bucket == "" || key == "" {
		return "", "", s3api.ErrInvalidArgument(
			"Copy Source must mention the source bucket and key: sourcebucket/sourcekey",
		)
	}

	return bucket, key, nil
}
