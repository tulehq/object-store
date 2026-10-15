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

// Subresource returns the first subresource named in r's query, or "" when
// there is none.
func Subresource(r *http.Request) string {
	q := r.URL.Query()
	for _, name := range subresources {
		if q.Has(name) {
			return name
		}
	}
	return ""
}

// CheckSubresource returns a NotImplemented error when r names a subresource
// other than those in allowed.
func CheckSubresource(r *http.Request, allowed ...string) error {
	name := Subresource(r)
	if name == "" || slices.Contains(allowed, name) {
		return nil
	}
	return ErrNotImplemented("The ?" + name + " subresource is not supported.")
}
