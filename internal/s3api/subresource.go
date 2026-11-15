package s3api

import (
	"net/http"
	"slices"
)

// subresources are the query parameters that turn a bucket or object request
// into a different S3 operation (GET /bucket?acl is GetBucketAcl, not
// ListObjects). Handlers must not treat these as plain requests: a PUT with
// ?tagging would otherwise overwrite the object with the tagging document.
var subresources = []string{
	"accelerate", "acl", "analytics", "attributes", "cors", "delete",
	"encryption", "intelligent-tiering", "inventory", "legal-hold",
	"lifecycle", "location", "logging", "metadataConfiguration", "metrics",
	"notification", "object-lock", "ownershipControls", "partNumber",
	"policy", "policyStatus", "publicAccessBlock", "renameObject",
	"replication", "requestPayment", "restore", "retention", "select",
	"session", "tagging", "torrent", "uploadId", "uploads", "versionId",
	"versioning", "versions", "website",
}

// Subresources returns every subresource named in r's query, in the order
// of subresources, or nil when there is none.
func Subresources(r *http.Request) []string {
	q := r.URL.Query()
	var names []string
	for _, name := range subresources {
		if q.Has(name) {
			names = append(names, name)
		}
	}
	return names
}

// Subresource returns the first subresource named in r's query, or "" when
// there is none.
func Subresource(r *http.Request) string {
	if names := Subresources(r); len(names) > 0 {
		return names[0]
	}
	return ""
}

// CheckSubresource returns a NotImplemented error when r names any
// subresource other than those in allowed. Every subresource is checked, so
// one that changes the meaning of another, such as ?versionId with
// ?tagging, is refused rather than ignored.
func CheckSubresource(r *http.Request, allowed ...string) error {
	for _, name := range Subresources(r) {
		if !slices.Contains(allowed, name) {
			return ErrNotImplemented("The ?" + name + " subresource is not supported.")
		}
	}
	return nil
}
