package lifecycle

import (
	"crypto/rand"
	"encoding/xml"
	"strconv"
	"strings"

	"tule.dev/object-store/internal/s3api"
)

// rootNames are the root elements a document may have. The API reference's
// request syntax names LifecycleConfiguration, and its own examples of
// PutBucketLifecycleConfiguration requests send LifeCycleConfiguration;
// both are read, and the configuration is answered as
// LifecycleConfiguration.
var rootNames = map[string]bool{
	"LifecycleConfiguration": true,
	"LifeCycleConfiguration": true,
}

// The wire twins of the document types. Every element is read as a slice
// and every value as text, so that decode can refuse, with MalformedXML,
// an element repeated where the schema allows one, and an empty or
// mistyped number, boolean or date, which encoding/xml would otherwise
// read as the last value given or as zero.
type (
	wireConfiguration struct {
		XMLName xml.Name
		Rules   []wireRule `xml:"Rule"`
	}

	wireRule struct {
		IDs                          []string                   `xml:"ID"`
		Filters                      []wireFilter               `xml:"Filter"`
		Prefixes                     []string                   `xml:"Prefix"`
		Statuses                     []string                   `xml:"Status"`
		Expirations                  []wireExpiration           `xml:"Expiration"`
		Transitions                  []wireTransition           `xml:"Transition"`
		NoncurrentVersionTransitions []wireNoncurrentTransition `xml:"NoncurrentVersionTransition"`
		NoncurrentVersionExpirations []wireNoncurrentExpiration `xml:"NoncurrentVersionExpiration"`
		AbortIncompleteUploads       []wireAbort                `xml:"AbortIncompleteMultipartUpload"`
	}

	wireFilter struct {
		And                   []wireAnd `xml:"And"`
		ObjectSizeGreaterThan []string  `xml:"ObjectSizeGreaterThan"`
		ObjectSizeLessThan    []string  `xml:"ObjectSizeLessThan"`
		Prefix                []string  `xml:"Prefix"`
		Tag                   []wireTag `xml:"Tag"`
	}

	wireAnd struct {
		ObjectSizeGreaterThan []string  `xml:"ObjectSizeGreaterThan"`
		ObjectSizeLessThan    []string  `xml:"ObjectSizeLessThan"`
		Prefix                []string  `xml:"Prefix"`
		Tags                  []wireTag `xml:"Tag"`
	}

	wireTag struct {
		Key   []string `xml:"Key"`
		Value []string `xml:"Value"`
	}

	wireExpiration struct {
		Date                      []string `xml:"Date"`
		Days                      []string `xml:"Days"`
		ExpiredObjectDeleteMarker []string `xml:"ExpiredObjectDeleteMarker"`
	}

	wireTransition struct {
		Date         []string `xml:"Date"`
		Days         []string `xml:"Days"`
		StorageClass []string `xml:"StorageClass"`
	}

	wireNoncurrentTransition struct {
		NewerNoncurrentVersions []string `xml:"NewerNoncurrentVersions"`
		NoncurrentDays          []string `xml:"NoncurrentDays"`
		StorageClass            []string `xml:"StorageClass"`
	}

	wireNoncurrentExpiration struct {
		NewerNoncurrentVersions []string `xml:"NewerNoncurrentVersions"`
		NoncurrentDays          []string `xml:"NoncurrentDays"`
	}

	wireAbort struct {
		DaysAfterInitiation []string `xml:"DaysAfterInitiation"`
	}
)

// Parse decodes and validates a PutBucketLifecycleConfiguration body:
// first the shape and types of the whole document, then rule by rule in
// document order, then across rules. It answers the
// first error found as the S3 error the request fails with (see
// validate.go). A rule without an ID, or with an empty one, is given a
// generated one. Elements the document model does not know are ignored.
func Parse(doc []byte) (*Configuration, error) {
	if len(doc) > MaxDocument {
		return nil, s3api.ErrMaxMessageLengthExceeded()
	}
	var w wireConfiguration
	if err := xml.Unmarshal(doc, &w); err != nil || !rootNames[w.XMLName.Local] ||
		len(w.Rules) == 0 {
		return nil, s3api.ErrMalformedXML()
	}
	if len(w.Rules) > MaxRules {
		return nil, s3api.ErrInvalidRequest(msgTooManyRules)
	}
	var d decoder
	c := &Configuration{Xmlns: xmlns, Rules: make([]Rule, 0, len(w.Rules))}
	for i := range w.Rules {
		c.Rules = append(c.Rules, w.Rules[i].decode(&d))
	}
	if d.bad {
		return nil, s3api.ErrMalformedXML()
	}
	if err := checkRules(c.Rules); err != nil {
		return nil, err
	}
	assignIDs(c.Rules)
	return c, nil
}

// ParseTransitionDefault validates an x-amz-transition-default-minimum-
// object-size request header value; "" is TransitionDefault128K, which
// S3 applies to a configuration put without the header.
func ParseTransitionDefault(v string) (string, error) {
	switch v {
	case "":
		return TransitionDefault128K, nil
	case TransitionDefault128K, TransitionDefaultVaries:
		return v, nil
	}
	return "", s3api.ErrInvalidArgument(msgTransitionDefault)
}

// assignIDs gives each rule without an ID a generated one, unique within
// rules (L13).
func assignIDs(rules []Rule) {
	used := make(map[string]bool, len(rules))
	for i := range rules {
		used[rules[i].ID] = true
	}
	for i := range rules {
		if rules[i].ID != "" {
			continue
		}
		id := rand.Text()
		for used[id] {
			id = rand.Text()
		}
		used[id] = true
		rules[i].ID = id
	}
}

// decode converts w to a Rule, recording in d what does not decode.
func (w *wireRule) decode(d *decoder) Rule {
	r := Rule{
		ID:     d.str(w.IDs),
		Prefix: d.strRef(w.Prefixes),
		Status: d.str(w.Statuses),
	}
	if d.one(len(w.Filters)) {
		r.Filter = w.Filters[0].decode(d)
	}
	if d.one(len(w.Expirations)) {
		e := &w.Expirations[0]
		r.Expiration = &Expiration{
			Date: d.date(e.Date), Days: d.int32(e.Days),
			ExpiredObjectDeleteMarker: d.boolean(e.ExpiredObjectDeleteMarker),
		}
	}
	for i := range w.Transitions {
		t := &w.Transitions[i]
		r.Transitions = append(r.Transitions, Transition{
			Date: d.date(t.Date), Days: d.int32(t.Days), StorageClass: d.str(t.StorageClass),
		})
	}
	for i := range w.NoncurrentVersionTransitions {
		t := &w.NoncurrentVersionTransitions[i]
		r.NoncurrentVersionTransitions = append(r.NoncurrentVersionTransitions,
			NoncurrentVersionTransition{
				NewerNoncurrentVersions: d.int32(t.NewerNoncurrentVersions),
				NoncurrentDays:          d.int32(t.NoncurrentDays),
				StorageClass:            d.str(t.StorageClass),
			})
	}
	if d.one(len(w.NoncurrentVersionExpirations)) {
		e := &w.NoncurrentVersionExpirations[0]
		r.NoncurrentVersionExpiration = &NoncurrentVersionExpiration{
			NewerNoncurrentVersions: d.int32(e.NewerNoncurrentVersions),
			NoncurrentDays:          d.int32(e.NoncurrentDays),
		}
	}
	if d.one(len(w.AbortIncompleteUploads)) {
		r.AbortIncompleteMultipartUpload = &AbortIncompleteMultipartUpload{
			DaysAfterInitiation: d.int32(w.AbortIncompleteUploads[0].DaysAfterInitiation),
		}
	}
	return r
}

// decode converts w to a Filter, recording in d what does not decode.
func (w *wireFilter) decode(d *decoder) *Filter {
	f := &Filter{
		ObjectSizeGreaterThan: d.int64(w.ObjectSizeGreaterThan),
		ObjectSizeLessThan:    d.int64(w.ObjectSizeLessThan),
		Prefix:                d.strRef(w.Prefix),
	}
	if d.one(len(w.Tag)) {
		tag := w.Tag[0].decode(d)
		f.Tag = &tag
	}
	if d.one(len(w.And)) {
		a := &w.And[0]
		f.And = &And{
			ObjectSizeGreaterThan: d.int64(a.ObjectSizeGreaterThan),
			ObjectSizeLessThan:    d.int64(a.ObjectSizeLessThan),
			Prefix:                d.strRef(a.Prefix),
		}
		for i := range a.Tags {
			f.And.Tags = append(f.And.Tags, a.Tags[i].decode(d))
		}
	}
	return f
}

// decode converts w to a Tag; a tag without a Value has an empty one.
func (w *wireTag) decode(d *decoder) s3api.Tag {
	return s3api.Tag{Key: d.str(w.Key), Value: d.str(w.Value)}
}

// decoder converts the text of single elements to their types, and
// records in bad whether one was repeated, or is a number, boolean or
// date that is empty or does not parse. Counts of days and versions are
// S3's 32-bit Integer, and sizes its 64-bit Long.
type decoder struct {
	bad bool
}

// one reports whether an element that may appear once appears n times,
// once; more is recorded as bad.
func (d *decoder) one(n int) bool {
	if n > 1 {
		d.bad = true
	}
	return n == 1
}

func (d *decoder) str(v []string) string {
	if !d.one(len(v)) {
		return ""
	}
	return v[0]
}

func (d *decoder) strRef(v []string) *string {
	if !d.one(len(v)) {
		return nil
	}
	return &v[0]
}

func (d *decoder) integer(v []string, bits int) *int64 {
	if !d.one(len(v)) {
		return nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v[0]), 10, bits)
	if err != nil {
		d.bad = true
		return nil
	}
	return &n
}

func (d *decoder) int32(v []string) *int {
	n := d.integer(v, 32)
	if n == nil {
		return nil
	}
	i := int(*n)
	return &i
}

func (d *decoder) int64(v []string) *int64 {
	return d.integer(v, 64)
}

func (d *decoder) boolean(v []string) *bool {
	if !d.one(len(v)) {
		return nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v[0]))
	if err != nil {
		d.bad = true
		return nil
	}
	return &b
}

func (d *decoder) date(v []string) *Date {
	if !d.one(len(v)) {
		return nil
	}
	var date Date
	if err := date.UnmarshalText([]byte(v[0])); err != nil {
		d.bad = true
		return nil
	}
	return &date
}
