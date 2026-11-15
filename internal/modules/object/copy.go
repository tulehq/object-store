package object

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// copyInput reads a CopyObject request for the object at dst.
func copyInput(r *http.Request, dst s3api.Target) (contracts.CopyObjectInput, error) {
	in := contracts.CopyObjectInput{DstBucket: dst.Bucket, DstKey: dst.Key}

	src, err := parseCopySource(r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		return in, err
	}
	in.SrcBucket, in.SrcKey, in.SrcVersion = src.Bucket, src.Key, src.VersionID

	if in.Precondition, err = precondition(r); err != nil {
		return in, err
	}
	in.SourceCondition = sourceCondition(r)

	if d := r.Header.Get(s3api.HeaderTaggingDirective); !validTaggingDirective(d) {
		return in, s3api.ErrInvalidArgument("Unknown tagging directive " + d)
	}

	switch d := r.Header.Get("X-Amz-Metadata-Directive"); d {
	case "", "COPY":
		// A version named onto its own key may restore it (see
		// selfCopy), which the handler decides once it knows the bucket's
		// versioning state.
		if src.Bucket == dst.Bucket && src.Key == dst.Key && src.VersionID == "" {
			return in, errCopyToItself()
		}
	case "REPLACE":
		replace := putInput(r)
		in.Replace = &replace
	default:
		return in, s3api.ErrInvalidArgument("Unknown metadata directive " + d)
	}

	return in, nil
}

// validTaggingDirective reports whether d is an x-amz-tagging-directive
// CopyObject takes: none, COPY or REPLACE.
func validTaggingDirective(d string) bool {
	return d == "" || d == "COPY" || d == "REPLACE"
}

// errCopyToItself is the refusal of a copy of an object onto itself under
// the COPY metadata directive.
func errCopyToItself() error {
	return s3api.ErrInvalidRequest("This copy request is illegal because it is " +
		"trying to copy an object to itself without changing the object's metadata, " +
		"storage class, website redirect location or encryption attributes.")
}

// selfCopy reports whether in copies a version named by ID onto its own
// key under the COPY metadata directive, the one copy of an object onto
// itself copyInput lets through. It restores the version in a bucket that
// keeps versions: "Copy a previous version of the object into the same
// bucket. The copied object becomes the current version of that object
// and all object versions are preserved." (S3 User Guide, "Restoring
// previous versions"). In an Unversioned bucket it would replace its own
// source, so it is refused as any other copy onto itself is.
func selfCopy(in contracts.CopyObjectInput) bool {
	return in.Replace == nil && in.SrcBucket == in.DstBucket && in.SrcKey == in.DstKey
}

// parseCopySource splits an x-amz-copy-source value, "bucket/key" with an
// optional leading slash and a URL-encoded key, and an optional
// "?versionId=<version-id>" (CopyObject and UploadPartCopy API
// references), into the source it names: its bucket, key and version, ""
// for the current one. A query that does not decode is 400
// InvalidArgument, as a key that does not is; one of anything but one
// versionId is 501 NotImplemented; and an empty versionId is 400
// InvalidArgument, as s3api.VersionID answers one.
func parseCopySource(v string) (s3api.Target, error) {
	v, query, _ := strings.Cut(v, "?")
	versionID, err := copySourceVersion(query)
	if err != nil {
		return s3api.Target{}, err
	}

	decoded, uErr := url.PathUnescape(v)
	if uErr != nil {
		return s3api.Target{}, s3api.ErrInvalidArgument("Invalid copy source encoding")
	}

	bucket, key, ok := strings.Cut(strings.TrimPrefix(decoded, "/"), "/")
	if !ok || bucket == "" || key == "" {
		return s3api.Target{}, s3api.ErrInvalidArgument(
			"Copy Source must mention the source bucket and key: sourcebucket/sourcekey",
		)
	}

	return s3api.Target{Bucket: bucket, Key: key, VersionID: versionID}, nil
}

// copySourceVersion returns the version query, an x-amz-copy-source's
// query, names, "" for none, as parseCopySource reads it.
func copySourceVersion(query string) (string, error) {
	if query == "" {
		return "", nil
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", s3api.ErrInvalidArgument("Invalid copy source encoding")
	}
	ids, ok := values["versionId"]
	if len(values) != 1 || !ok || len(ids) != 1 {
		return "", s3api.ErrNotImplemented(
			"An x-amz-copy-source query other than one versionId is not supported.",
		)
	}
	if ids[0] == "" {
		return "", s3api.ErrInvalidArgument("Version id cannot be the empty string")
	}
	return ids[0], nil
}

// copySourceError is err, the error a copy failed with, as answered: a
// source that names a delete marker by version is 400 InvalidRequest.
// "If you specify versionId in the x-amz-copy-source and the versionId is
// a delete marker, Amazon S3 returns an HTTP 400 Bad Request error,
// because you are not allowed to specify a delete marker as a version for
// the x-amz-copy-source" (UploadPartCopy API reference); the code and
// message are a choice, as the reference gives neither. A current delete
// marker is the 404 of a missing source.
func copySourceError(err error) error {
	var dm *contracts.DeleteMarkerError
	if errors.As(err, &dm) && !dm.Current {
		return s3api.ErrInvalidRequest("The source of a copy request may not specifically " +
			"refer to a delete marker by version id.")
	}
	return err
}

// sourceCondition reads the x-amz-copy-source-if-* headers of a CopyObject
// or UploadPartCopy. An ETag is compared without its quotes. A date that
// is not an HTTP date is ignored, as RFC 7232 has a recipient ignore an
// invalid If-Modified-Since and If-Unmodified-Since; S3 documents neither
// case.
func sourceCondition(r *http.Request) contracts.SourceCondition {
	date := func(h string) *time.Time {
		t, err := http.ParseTime(r.Header.Get(h))
		if err != nil {
			return nil
		}
		return &t
	}
	etag := func(h string) string {
		return strings.Trim(strings.TrimSpace(r.Header.Get(h)), `"`)
	}
	return contracts.SourceCondition{
		IfMatch:           etag("X-Amz-Copy-Source-If-Match"),
		IfNoneMatch:       etag("X-Amz-Copy-Source-If-None-Match"),
		IfModifiedSince:   date("X-Amz-Copy-Source-If-Modified-Since"),
		IfUnmodifiedSince: date("X-Amz-Copy-Source-If-Unmodified-Since"),
	}
}
