package s3api

import (
	"encoding/xml"
	"net/http"

	"tule.dev/object-store/internal/contracts"
)

// The headers that name the version a request acted on.
const (
	// HeaderVersionID is x-amz-version-id: the version a read served, a
	// write created or a delete removed or created.
	HeaderVersionID = "X-Amz-Version-Id"
	// HeaderDeleteMarker is x-amz-delete-marker: set to true when the
	// version a request found, created or removed is a delete marker.
	HeaderDeleteMarker = "X-Amz-Delete-Marker"
	// HeaderCopySourceVersionID is x-amz-copy-source-version-id: the
	// version of the source a CopyObject or UploadPartCopy copied.
	HeaderCopySourceVersionID = "X-Amz-Copy-Source-Version-Id"
)

// VersionID returns the versionId r's query names, as given, or "" when it
// names none. An empty one is 400 InvalidArgument; whether a value is one
// the store issues is the store's to tell (contracts.ErrInvalidVersionID).
func VersionID(r *http.Request) (string, error) {
	q := r.URL.Query()
	if !q.Has("versionId") {
		return "", nil
	}
	id := q.Get("versionId")
	if id == "" {
		return "", ErrInvalidArgument("Version id cannot be the empty string")
	}
	return id, nil
}

// ParseVersioningConfiguration decodes a PutBucketVersioning document.
// Its Status is left for the store to check (contracts.ErrUnversionedState
// and ErrInvalidVersioningStatus), and an MfaDelete other than the valid
// values Enabled and Disabled (API_VersioningConfiguration) is
// MalformedXML.
func ParseVersioningConfiguration(doc []byte) (contracts.VersioningConfig, error) {
	var c VersioningConfiguration
	if err := xml.Unmarshal(doc, &c); err != nil {
		return contracts.VersioningConfig{}, ErrMalformedXML()
	}
	switch c.MfaDelete {
	case "", "Enabled", "Disabled":
	default:
		return contracts.VersioningConfig{}, ErrMalformedXML()
	}
	return contracts.VersioningConfig{
		Status:    contracts.Versioning(c.Status),
		MFADelete: c.MfaDelete,
	}, nil
}

// ListVersionsResult is the ListObjectVersions response document. Its
// versions and delete markers are one slice, each entry named Version or
// DeleteMarker by its XMLName, so they are encoded interleaved in the
// order listed, as S3 lists them.
type ListVersionsResult struct {
	XMLName             xml.Name            `xml:"ListVersionsResult"`
	Xmlns               string              `xml:"xmlns,attr"`
	Name                string              `xml:"Name"`
	Prefix              string              `xml:"Prefix"`
	KeyMarker           string              `xml:"KeyMarker"`
	VersionIDMarker     string              `xml:"VersionIdMarker"`
	NextKeyMarker       string              `xml:"NextKeyMarker,omitempty"`
	NextVersionIDMarker string              `xml:"NextVersionIdMarker,omitempty"`
	Delimiter           string              `xml:"Delimiter,omitempty"`
	EncodingType        string              `xml:"EncodingType,omitempty"`
	Entries             []ListVersionsEntry `xml:""`
	CommonPrefixes      []CommonPrefix      `xml:"CommonPrefixes"`
	MaxKeys             int                 `xml:"MaxKeys"`
	IsTruncated         bool                `xml:"IsTruncated"`
}

// NewListVersionsResult returns an empty ListObjectVersions response
// document.
func NewListVersionsResult() *ListVersionsResult {
	return &ListVersionsResult{Xmlns: xmlns}
}

// ListVersionsEntry is one Version or DeleteMarker of a ListObjectVersions
// response, as its XMLName says. A delete marker has no ETag, Size or
// StorageClass. No Owner is sent.
type ListVersionsEntry struct {
	XMLName      xml.Name
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag,omitempty"`
	Size         *int64 `xml:"Size,omitempty"`
	StorageClass string `xml:"StorageClass,omitempty"`
	IsLatest     bool   `xml:"IsLatest"`
}

// NewVersionEntry returns the Version entry of a listing.
func NewVersionEntry(
	key, versionID, lastModified, etag, storageClass string,
	size int64,
	latest bool,
) ListVersionsEntry {
	return ListVersionsEntry{
		XMLName: xml.Name{Local: "Version"}, Key: key, VersionID: versionID,
		LastModified: lastModified, ETag: etag, StorageClass: storageClass, Size: &size,
		IsLatest: latest,
	}
}

// NewDeleteMarkerEntry returns the DeleteMarker entry of a listing.
func NewDeleteMarkerEntry(key, versionID, lastModified string, latest bool) ListVersionsEntry {
	return ListVersionsEntry{
		XMLName: xml.Name{Local: "DeleteMarker"}, Key: key, VersionID: versionID,
		LastModified: lastModified, IsLatest: latest,
	}
}
