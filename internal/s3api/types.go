// Package s3api holds the S3 wire format: response documents, error
// responses and request body encodings.
package s3api

import (
	"encoding/xml"
	"time"
)

const xmlns = "http://s3.amazonaws.com/doc/2006-03-01/"

// timeFormat is the ISO 8601 form S3 uses for timestamps in XML bodies.
const timeFormat = "2006-01-02T15:04:05.000Z"

// Timestamp formats a time the way S3 XML bodies do.
func Timestamp(t time.Time) string {
	return t.UTC().Format(timeFormat)
}

// ETag quotes a hex digest for the ETag header and XML fields. An empty digest
// stays empty so callers can omit the field.
func ETag(digest string) string {
	if digest == "" {
		return ""
	}
	return `"` + digest + `"`
}

// Owner is the bucket owner in a ListBuckets response. Buckets have no owner
// here, so it is sent empty.
type Owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

// Bucket is one entry of a ListBuckets response.
type Bucket struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

// ListAllMyBucketsResult is the ListBuckets response document.
type ListAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Xmlns   string   `xml:"xmlns,attr"`
	Owner   Owner    `xml:"Owner"`
	Buckets []Bucket `xml:"Buckets>Bucket"`
}

// NewListAllMyBucketsResult returns a ListBuckets response listing buckets.
func NewListAllMyBucketsResult(buckets []Bucket) *ListAllMyBucketsResult {
	return &ListAllMyBucketsResult{Xmlns: xmlns, Buckets: buckets}
}

// Object is one Contents entry of a ListObjectsV2 response.
type Object struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag,omitempty"`
	StorageClass string `xml:"StorageClass"`
	Size         int64  `xml:"Size"`
}

// CommonPrefix is one CommonPrefixes entry of a ListObjectsV2 response.
type CommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// ListBucketResult is the ListObjectsV2 response document.
type ListBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Xmlns                 string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	EncodingType          string         `xml:"EncodingType,omitempty"`
	Contents              []Object       `xml:"Contents"`
	CommonPrefixes        []CommonPrefix `xml:"CommonPrefixes"`
	MaxKeys               int            `xml:"MaxKeys"`
	KeyCount              int            `xml:"KeyCount"`
	IsTruncated           bool           `xml:"IsTruncated"`
}

// NewListBucketResult returns an empty ListObjectsV2 response document.
func NewListBucketResult() *ListBucketResult {
	return &ListBucketResult{Xmlns: xmlns}
}

// CopyObjectResult is the CopyObject response document.
type CopyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	Xmlns        string   `xml:"xmlns,attr"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

// NewCopyObjectResult returns a CopyObject response for the copy's ETag digest
// and modification time.
func NewCopyObjectResult(etag string, lastModified time.Time) *CopyObjectResult {
	return &CopyObjectResult{
		Xmlns:        xmlns,
		ETag:         ETag(etag),
		LastModified: Timestamp(lastModified),
	}
}

// MaxDeleteObjects is the most keys one DeleteObjects request may name.
const MaxDeleteObjects = 1000

// Delete is the DeleteObjects request document.
type Delete struct {
	XMLName xml.Name           `xml:"Delete"`
	Objects []ObjectIdentifier `xml:"Object"`
	Quiet   bool               `xml:"Quiet"`
}

// ObjectIdentifier names one object in a DeleteObjects request.
type ObjectIdentifier struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId"`
}

// DeleteResult is the DeleteObjects response document.
type DeleteResult struct {
	XMLName xml.Name        `xml:"DeleteResult"`
	Xmlns   string          `xml:"xmlns,attr"`
	Deleted []DeletedObject `xml:"Deleted"`
	Errors  []DeleteError   `xml:"Error"`
}

// NewDeleteResult returns an empty DeleteObjects response document.
func NewDeleteResult() *DeleteResult {
	return &DeleteResult{Xmlns: xmlns}
}

// DeletedObject reports one key a DeleteObjects request removed.
type DeletedObject struct {
	Key string `xml:"Key"`
}

// DeleteError reports one key a DeleteObjects request failed to remove.
type DeleteError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// InitiateMultipartUploadResult is the CreateMultipartUpload response document.
type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// NewInitiateMultipartUploadResult returns a CreateMultipartUpload response.
func NewInitiateMultipartUploadResult(bucket, key, uploadID string) *InitiateMultipartUploadResult {
	return &InitiateMultipartUploadResult{
		Xmlns:    xmlns,
		Bucket:   bucket,
		Key:      key,
		UploadID: uploadID,
	}
}

// CompleteMultipartUpload is the CompleteMultipartUpload request document.
type CompleteMultipartUpload struct {
	XMLName xml.Name        `xml:"CompleteMultipartUpload"`
	Parts   []CompletedPart `xml:"Part"`
}

// CompletedPart is one Part entry of a CompleteMultipartUpload request.
type CompletedPart struct {
	ETag       string `xml:"ETag"`
	PartNumber int    `xml:"PartNumber"`
}

// CompleteMultipartUploadResult is the CompleteMultipartUpload response document.
type CompleteMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// NewCompleteMultipartUploadResult returns a CompleteMultipartUpload response
// for the assembled object's location and ETag digest.
func NewCompleteMultipartUploadResult(
	location, bucket, key, etag string,
) *CompleteMultipartUploadResult {
	return &CompleteMultipartUploadResult{
		Xmlns: xmlns, Location: location, Bucket: bucket, Key: key, ETag: ETag(etag),
	}
}

// ListBucketResultV1 is the ListObjects (version 1) response document.
type ListBucketResultV1 struct {
	XMLName        xml.Name       `xml:"ListBucketResult"`
	Xmlns          string         `xml:"xmlns,attr"`
	Name           string         `xml:"Name"`
	Prefix         string         `xml:"Prefix"`
	Marker         string         `xml:"Marker"`
	NextMarker     string         `xml:"NextMarker,omitempty"`
	Delimiter      string         `xml:"Delimiter,omitempty"`
	EncodingType   string         `xml:"EncodingType,omitempty"`
	Contents       []Object       `xml:"Contents"`
	CommonPrefixes []CommonPrefix `xml:"CommonPrefixes"`
	MaxKeys        int            `xml:"MaxKeys"`
	IsTruncated    bool           `xml:"IsTruncated"`
}

// NewListBucketResultV1 returns an empty ListObjects (version 1) response
// document.
func NewListBucketResultV1() *ListBucketResultV1 {
	return &ListBucketResultV1{Xmlns: xmlns}
}

// LocationConstraint is the GetBucketLocation response document. An empty
// region means us-east-1.
type LocationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	Xmlns   string   `xml:"xmlns,attr"`
	Region  string   `xml:",chardata"`
}

// NewLocationConstraint returns a GetBucketLocation response for region.
func NewLocationConstraint(region string) *LocationConstraint {
	return &LocationConstraint{Xmlns: xmlns, Region: region}
}

// VersioningConfiguration is the GetBucketVersioning response document. An
// empty Status means versioning was never enabled on the bucket.
type VersioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Xmlns   string   `xml:"xmlns,attr"`
	Status  string   `xml:"Status,omitempty"`
}

// NewVersioningConfiguration returns the GetBucketVersioning response for a
// bucket that has never had versioning enabled.
func NewVersioningConfiguration() *VersioningConfiguration {
	return &VersioningConfiguration{Xmlns: xmlns}
}

// Part is one Part entry of a ListParts response.
type Part struct {
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	PartNumber   int    `xml:"PartNumber"`
	Size         int64  `xml:"Size"`
}

// ListPartsResult is the ListParts response document.
type ListPartsResult struct {
	XMLName              xml.Name `xml:"ListPartsResult"`
	Xmlns                string   `xml:"xmlns,attr"`
	Bucket               string   `xml:"Bucket"`
	Key                  string   `xml:"Key"`
	UploadID             string   `xml:"UploadId"`
	StorageClass         string   `xml:"StorageClass"`
	Parts                []Part   `xml:"Part"`
	PartNumberMarker     int      `xml:"PartNumberMarker"`
	NextPartNumberMarker int      `xml:"NextPartNumberMarker,omitempty"`
	MaxParts             int      `xml:"MaxParts"`
	IsTruncated          bool     `xml:"IsTruncated"`
}

// NewListPartsResult returns an empty ListParts response document.
func NewListPartsResult() *ListPartsResult {
	return &ListPartsResult{Xmlns: xmlns}
}

// Upload is one Upload entry of a ListMultipartUploads response.
type Upload struct {
	Key          string `xml:"Key"`
	UploadID     string `xml:"UploadId"`
	StorageClass string `xml:"StorageClass"`
	Initiated    string `xml:"Initiated"`
}

// ListMultipartUploadsResult is the ListMultipartUploads response document.
type ListMultipartUploadsResult struct {
	XMLName            xml.Name       `xml:"ListMultipartUploadsResult"`
	Xmlns              string         `xml:"xmlns,attr"`
	Bucket             string         `xml:"Bucket"`
	KeyMarker          string         `xml:"KeyMarker"`
	UploadIDMarker     string         `xml:"UploadIdMarker"`
	NextKeyMarker      string         `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string         `xml:"NextUploadIdMarker,omitempty"`
	Prefix             string         `xml:"Prefix"`
	Delimiter          string         `xml:"Delimiter,omitempty"`
	EncodingType       string         `xml:"EncodingType,omitempty"`
	Uploads            []Upload       `xml:"Upload"`
	CommonPrefixes     []CommonPrefix `xml:"CommonPrefixes"`
	MaxUploads         int            `xml:"MaxUploads"`
	IsTruncated        bool           `xml:"IsTruncated"`
}

// NewListMultipartUploadsResult returns an empty ListMultipartUploads
// response document.
func NewListMultipartUploadsResult() *ListMultipartUploadsResult {
	return &ListMultipartUploadsResult{Xmlns: xmlns}
}
