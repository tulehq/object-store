// Package lifecycle is a bucket's S3 Lifecycle configuration: its document
// model, the validation of PutBucketLifecycleConfiguration, the stored
// form, and the evaluation of its rules against an object, an upload or
// the versions of one key. It does no I/O and reads no clock: every
// evaluation takes the time it is made at.
//
// Transition and NoncurrentVersionTransition actions are validated, stored
// and answered by GET, and never applied: there are no storage classes to
// move an object to.
package lifecycle

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"tule.dev/object-store/internal/s3api"
)

// Limits and the x-amz-transition-default-minimum-object-size header. S3
// allows "up to 1,000 rules. This limit is not adjustable"
// (PutBucketLifecycleConfiguration API reference); S3 documents no size
// limit for the document, and 1 MiB is a choice that fits 1,000 rules.
const (
	MaxRules                = 1000
	MaxDocument             = 1 << 20
	HeaderTransitionDefault = "X-Amz-Transition-Default-Minimum-Object-Size"
	TransitionDefault128K   = "all_storage_classes_128K"
	TransitionDefaultVaries = "varies_by_storage_class"
)

// The values of a rule's Status.
const (
	StatusEnabled  = "Enabled"
	StatusDisabled = "Disabled"
)

const xmlns = "http://s3.amazonaws.com/doc/2006-03-01/"

// Configuration is the PutBucketLifecycleConfiguration request and
// GetBucketLifecycleConfiguration response document. Its rules are in
// document order, each with an ID.
type Configuration struct {
	XMLName xml.Name `xml:"LifecycleConfiguration"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Rules   []Rule   `xml:"Rule"`
}

// Rule is one lifecycle rule. Exactly one of Filter and Prefix is set:
// Prefix for a rule written in the deprecated form that has no Filter. The
// field order is the element order GET answers.
//
//nolint:govet // fieldalignment: the field order is the element order
type Rule struct {
	ID                             string                          `xml:"ID"`
	Filter                         *Filter                         `xml:"Filter"`
	Prefix                         *string                         `xml:"Prefix"`
	Status                         string                          `xml:"Status"`
	Expiration                     *Expiration                     `xml:"Expiration"`
	Transitions                    []Transition                    `xml:"Transition"`
	NoncurrentVersionTransitions   []NoncurrentVersionTransition   `xml:"NoncurrentVersionTransition"`
	NoncurrentVersionExpiration    *NoncurrentVersionExpiration    `xml:"NoncurrentVersionExpiration"`
	AbortIncompleteMultipartUpload *AbortIncompleteMultipartUpload `xml:"AbortIncompleteMultipartUpload"`
}

// Filter is a rule's Filter: at most one of its fields is set, and none
// for an empty filter, which admits every object.
type Filter struct {
	And                   *And       `xml:"And"`
	ObjectSizeGreaterThan *int64     `xml:"ObjectSizeGreaterThan"`
	ObjectSizeLessThan    *int64     `xml:"ObjectSizeLessThan"`
	Prefix                *string    `xml:"Prefix"`
	Tag                   *s3api.Tag `xml:"Tag"`
}

// And is a Filter's conjunction of two or more predicates.
type And struct {
	ObjectSizeGreaterThan *int64      `xml:"ObjectSizeGreaterThan"`
	ObjectSizeLessThan    *int64      `xml:"ObjectSizeLessThan"`
	Prefix                *string     `xml:"Prefix"`
	Tags                  []s3api.Tag `xml:"Tag"`
}

// Expiration is a rule's expiration of current versions, by Days or Date,
// or its removal of expired object delete markers.
type Expiration struct {
	Date                      *Date `xml:"Date"`
	Days                      *int  `xml:"Days"`
	ExpiredObjectDeleteMarker *bool `xml:"ExpiredObjectDeleteMarker"`
}

// Transition is a rule's transition of current versions, never applied.
type Transition struct {
	Date         *Date  `xml:"Date"`
	Days         *int   `xml:"Days"`
	StorageClass string `xml:"StorageClass"`
}

// NoncurrentVersionTransition is a rule's transition of noncurrent
// versions, never applied.
type NoncurrentVersionTransition struct {
	NewerNoncurrentVersions *int   `xml:"NewerNoncurrentVersions"`
	NoncurrentDays          *int   `xml:"NoncurrentDays"`
	StorageClass            string `xml:"StorageClass"`
}

// NoncurrentVersionExpiration is a rule's permanent deletion of noncurrent
// versions.
type NoncurrentVersionExpiration struct {
	NewerNoncurrentVersions *int `xml:"NewerNoncurrentVersions"`
	NoncurrentDays          *int `xml:"NoncurrentDays"`
}

// AbortIncompleteMultipartUpload is a rule's abort of uploads left
// incomplete.
type AbortIncompleteMultipartUpload struct {
	DaysAfterInitiation *int `xml:"DaysAfterInitiation"`
}

// dateFormat is how a Date is answered, as S3 writes timestamps in XML.
const dateFormat = "2006-01-02T15:04:05.000Z"

// Date is an Expiration's or Transition's Date, in UTC. It is answered as
// 2006-01-02T15:04:05.000Z, and read as an RFC 3339 timestamp or a bare
// 2006-01-02 date ("The date value must conform to the ISO 8601 format",
// LifecycleExpiration API reference).
type Date struct{ time.Time }

// MarshalText implements encoding.TextMarshaler.
func (d Date) MarshalText() ([]byte, error) {
	return []byte(d.UTC().Format(dateFormat)), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Date) UnmarshalText(b []byte) error {
	s := strings.TrimSpace(string(b))
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		if t, err = time.Parse(time.DateOnly, s); err != nil {
			return fmt.Errorf("lifecycle: invalid date %q: %w", s, err)
		}
	}
	d.Time = t.UTC()
	return nil
}
