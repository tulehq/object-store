package s3api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// The errors the bucket configuration reads answer a bucket without the
// configuration with. GetBucketTagging's API reference names NoSuchTagSet,
// "There is no tag set associated with the bucket."; S3's error code list
// names none of them, so the other codes, every status and every message
// are S3's as its clients report them, not documented.
var (
	ErrNoSuchTagSet = NewError(http.StatusNotFound, "NoSuchTagSet",
		"The TagSet does not exist")
	ErrNoSuchCORSConfiguration = NewError(http.StatusNotFound, "NoSuchCORSConfiguration",
		"The CORS configuration does not exist")
	ErrNoSuchPublicAccessBlockConfiguration = NewError(http.StatusNotFound,
		"NoSuchPublicAccessBlockConfiguration",
		"The public access block configuration was not found")
	ErrOwnershipControlsNotFound = NewError(http.StatusNotFound,
		"OwnershipControlsNotFoundError", "The bucket ownership controls were not found")
)

// ErrInvalidTag is S3's 400 InvalidTag, which PutBucketTagging's API
// reference lists for "a tag [that] did not pass input validation".
func ErrInvalidTag(msg string) *Error {
	return NewError(http.StatusBadRequest, "InvalidTag", msg)
}

// Tag is one tag of a tag set.
type Tag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// Tagging is the PutBucketTagging request and GetBucketTagging response
// document.
type Tagging struct {
	TagSet  *TagSet  `xml:"TagSet"`
	XMLName xml.Name `xml:"Tagging"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
}

// TagSet is the tags of a Tagging document.
type TagSet struct {
	Tags []Tag `xml:"Tag"`
}

// The bucket tag rules ("Using cost allocation S3 bucket tags", S3 User
// Guide): "A tag set can contain as many as 50 tags", "The tag key is a
// case-sensitive string that can contain 1 to 128 Unicode characters",
// "The tag value is a case-sensitive string that can contain from 0 to 256
// Unicode characters", and "Keys must be unique within a tag set". The
// characters are AWS's general rule ("Best practices and strategies", Tag
// Editor User Guide): "letters, numbers, spaces representable in UTF-8,
// and the following characters: _ . : / = + - @". The aws: prefix is
// reserved ("User-defined tag restrictions", AWS Billing User Guide).
const (
	maxBucketTags   = 50
	maxTagKeyLen    = 128
	maxTagValueLen  = 256
	reservedTagPref = "aws:"
)

var tagCharsRe = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)

// ParseTagging decodes and validates a PutBucketTagging document. The
// messages of its InvalidTag errors are a choice: S3 documents the code but
// not its messages.
func ParseTagging(doc []byte) (*Tagging, error) {
	var t Tagging
	if err := xml.Unmarshal(doc, &t); err != nil || t.TagSet == nil {
		return nil, ErrMalformedXML()
	}
	if len(t.TagSet.Tags) > maxBucketTags {
		return nil, ErrInvalidTag("Bucket tag count cannot be greater than 50")
	}
	seen := make(map[string]bool, len(t.TagSet.Tags))
	for _, tag := range t.TagSet.Tags {
		if err := checkTag(tag); err != nil {
			return nil, err
		}
		if seen[tag.Key] {
			return nil, ErrInvalidTag("Cannot provide multiple Tags with the same key")
		}
		seen[tag.Key] = true
	}
	return &Tagging{Xmlns: xmlns, TagSet: t.TagSet}, nil
}

func checkTag(tag Tag) error {
	switch {
	case utf8.RuneCountInString(tag.Key) > maxTagKeyLen:
		return ErrInvalidTag("The TagKey you have provided is too long, max 128")
	case utf8.RuneCountInString(tag.Value) > maxTagValueLen:
		return ErrInvalidTag("The TagValue you have provided is too long, max 256")
	case tag.Key == "" || !utf8.ValidString(tag.Key) || !tagCharsRe.MatchString(tag.Key):
		return ErrInvalidTag("The TagKey you have provided is invalid")
	case !utf8.ValidString(tag.Value) || !tagCharsRe.MatchString(tag.Value):
		return ErrInvalidTag("The TagValue you have provided is invalid")
	case strings.HasPrefix(strings.ToLower(tag.Key), reservedTagPref):
		return ErrInvalidTag("Your TagKey cannot be prefixed with aws:")
	}
	return nil
}

// CORSConfiguration is the PutBucketCors request and GetBucketCors
// response document.
type CORSConfiguration struct {
	XMLName xml.Name   `xml:"CORSConfiguration"`
	Xmlns   string     `xml:"xmlns,attr,omitempty"`
	Rules   []CORSRule `xml:"CORSRule"`
}

// CORSRule is one rule of a CORS configuration (API_CORSRule).
type CORSRule struct {
	MaxAgeSeconds  *int     `xml:"MaxAgeSeconds,omitempty"`
	ID             string   `xml:"ID,omitempty"`
	AllowedHeaders []string `xml:"AllowedHeader"`
	AllowedMethods []string `xml:"AllowedMethod"`
	AllowedOrigins []string `xml:"AllowedOrigin"`
	ExposeHeaders  []string `xml:"ExposeHeader"`
}

// MaxCORSDocument is the largest CORS configuration PutBucketCors takes:
// "The document is limited to 64 KB in size." (PutBucketCors API
// reference).
const MaxCORSDocument = 64 * 1024

// The CORS rule limits: "You can add up to 100 rules to the configuration"
// ("Elements of a CORS configuration", S3 User Guide) and an ID that
// "cannot be longer than 255 characters" (API_CORSRule).
const (
	maxCORSRules = 100
	maxCORSID    = 255
)

// corsMethods are the methods an AllowedMethod may name: "Valid values are
// GET, PUT, HEAD, POST, and DELETE." (API_CORSRule).
var corsMethods = []string{
	http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodHead,
}

// ParseCORSConfiguration decodes and validates a PutBucketCors document.
// The rules are S3's ("Elements of a CORS configuration", S3 User Guide,
// and API_CORSRule): at least one rule and at most 100, each with an
// AllowedMethod and an AllowedOrigin, methods among GET, PUT, POST, DELETE
// and HEAD, "only one * wildcard character" in an AllowedOrigin and "at
// most one * wildcard character" in an AllowedHeader, and an ID of at most
// 255 characters. That an ExposeHeader may hold no wildcard and that
// MaxAgeSeconds may not be negative are choices. S3 documents none of
// their error codes or messages; these are choices too.
func ParseCORSConfiguration(doc []byte) (*CORSConfiguration, error) {
	var c CORSConfiguration
	if err := xml.Unmarshal(doc, &c); err != nil || len(c.Rules) == 0 {
		return nil, ErrMalformedXML()
	}
	if len(c.Rules) > maxCORSRules {
		return nil, ErrInvalidRequest(
			"The number of CORS rules should not exceed allowed limit of 100 rules.")
	}
	for _, rule := range c.Rules {
		if err := checkCORSRule(rule); err != nil {
			return nil, err
		}
	}
	c.Xmlns = xmlns
	return &c, nil
}

func checkCORSRule(rule CORSRule) error {
	if len(rule.AllowedMethods) == 0 || len(rule.AllowedOrigins) == 0 {
		return ErrMalformedXML()
	}
	for _, m := range rule.AllowedMethods {
		if !slices.Contains(corsMethods, m) {
			return ErrInvalidRequest(
				"Found unsupported HTTP method in CORS config. Unsupported method is " + m)
		}
	}
	if err := checkWildcards("AllowedOrigin", rule.AllowedOrigins); err != nil {
		return err
	}
	if err := checkWildcards("AllowedHeader", rule.AllowedHeaders); err != nil {
		return err
	}
	for _, h := range rule.ExposeHeaders {
		if strings.Contains(h, "*") {
			return ErrInvalidRequest(fmt.Sprintf("ExposeHeader %q contains wildcard. "+
				"We currently do not support wildcard for ExposeHeader.", h))
		}
	}
	return checkCORSLimits(rule)
}

// checkCORSLimits refuses a rule whose ID or MaxAgeSeconds is out of range.
func checkCORSLimits(rule CORSRule) error {
	if utf8.RuneCountInString(rule.ID) > maxCORSID {
		return ErrInvalidRequest("The ID of a CORS rule cannot be longer than 255 characters.")
	}
	if rule.MaxAgeSeconds != nil && *rule.MaxAgeSeconds < 0 {
		return ErrInvalidRequest("MaxAgeSeconds must not be negative.")
	}
	return nil
}

// checkWildcards refuses a value of the element name with more than one
// "*".
func checkWildcards(name string, values []string) error {
	for _, v := range values {
		if strings.Count(v, "*") > 1 {
			return ErrInvalidRequest(fmt.Sprintf("%s %q can not have more than one wildcard.",
				name, v))
		}
	}
	return nil
}

// Match returns the first rule that allows a request from origin with
// method and, for a preflight, the requested headers, as S3 "uses the
// first CORSRule rule that matches the incoming browser request" ("How
// does Amazon S3 evaluate the CORS configuration on a bucket?", S3 User
// Guide): an AllowedOrigin matches origin, an AllowedMethod is method, and
// every requested header matches an AllowedHeader. A "*" in an
// AllowedOrigin or AllowedHeader stands for any run of characters. Origins
// and methods are matched exactly and header names regardless of case, as
// HTTP compares them; S3 documents neither. allowed is the rule's first
// AllowedOrigin that matches origin, which decides how the request is
// answered: S3 checks "the origins in the AllowedOrigins element", so a
// bare "*" is what allowed origin only when no listed origin before it
// matches.
func (c *CORSConfiguration) Match(
	origin, method string,
	headers []string,
) (rule *CORSRule, allowed string, ok bool) {
	for i := range c.Rules {
		rule = &c.Rules[i]
		if !slices.Contains(rule.AllowedMethods, method) ||
			!allHeadersAllowed(rule.AllowedHeaders, headers) {
			continue
		}
		for _, p := range rule.AllowedOrigins {
			if wildcardMatch(p, origin) {
				return rule, p, true
			}
		}
	}
	return nil, "", false
}

func allHeadersAllowed(allowed, headers []string) bool {
	for _, h := range headers {
		if !slices.ContainsFunc(allowed, func(p string) bool {
			return wildcardMatch(strings.ToLower(p), strings.ToLower(h))
		}) {
			return false
		}
	}
	return true
}

// wildcardMatch reports whether s matches pattern, in which at most one
// "*" stands for any run of characters.
func wildcardMatch(pattern, s string) bool {
	prefix, suffix, ok := strings.Cut(pattern, "*")
	if !ok {
		return pattern == s
	}
	return len(s) >= len(prefix)+len(suffix) &&
		strings.HasPrefix(s, prefix) && strings.HasSuffix(s, suffix)
}

// PublicAccessBlockConfiguration is the PutPublicAccessBlock request and
// GetPublicAccessBlock response document. An element left out of a
// request is false.
type PublicAccessBlockConfiguration struct {
	XMLName               xml.Name `xml:"PublicAccessBlockConfiguration"`
	Xmlns                 string   `xml:"xmlns,attr,omitempty"`
	BlockPublicAcls       bool     `xml:"BlockPublicAcls"`
	IgnorePublicAcls      bool     `xml:"IgnorePublicAcls"`
	BlockPublicPolicy     bool     `xml:"BlockPublicPolicy"`
	RestrictPublicBuckets bool     `xml:"RestrictPublicBuckets"`
}

// NewPublicAccessBlockAll returns the configuration with all four settings
// on, which new buckets get.
func NewPublicAccessBlockAll() *PublicAccessBlockConfiguration {
	return &PublicAccessBlockConfiguration{
		Xmlns: xmlns, BlockPublicAcls: true, IgnorePublicAcls: true,
		BlockPublicPolicy: true, RestrictPublicBuckets: true,
	}
}

// ParsePublicAccessBlock decodes a PutPublicAccessBlock document, or a
// stored one.
func ParsePublicAccessBlock(doc []byte) (*PublicAccessBlockConfiguration, error) {
	var c PublicAccessBlockConfiguration
	if err := xml.Unmarshal(doc, &c); err != nil {
		return nil, ErrMalformedXML()
	}
	c.Xmlns = xmlns
	return &c, nil
}

// PolicyStatus is the GetBucketPolicyStatus response document.
type PolicyStatus struct {
	XMLName  xml.Name `xml:"PolicyStatus"`
	Xmlns    string   `xml:"xmlns,attr"`
	IsPublic bool     `xml:"IsPublic"`
}

// NewPolicyStatus returns a GetBucketPolicyStatus response.
func NewPolicyStatus(public bool) *PolicyStatus {
	return &PolicyStatus{Xmlns: xmlns, IsPublic: public}
}

// The Object Ownership settings (API_OwnershipControlsRule).
const (
	OwnershipBucketOwnerEnforced  = "BucketOwnerEnforced"
	OwnershipBucketOwnerPreferred = "BucketOwnerPreferred"
	OwnershipObjectWriter         = "ObjectWriter"
)

// OwnershipControls is the PutBucketOwnershipControls request and
// GetBucketOwnershipControls response document.
type OwnershipControls struct {
	XMLName xml.Name                `xml:"OwnershipControls"`
	Xmlns   string                  `xml:"xmlns,attr,omitempty"`
	Rules   []OwnershipControlsRule `xml:"Rule"`
}

// OwnershipControlsRule is the one rule of an OwnershipControls document.
type OwnershipControlsRule struct {
	ObjectOwnership string `xml:"ObjectOwnership"`
}

// NewOwnershipControls returns the document for the setting ownership.
func NewOwnershipControls(ownership string) *OwnershipControls {
	return &OwnershipControls{
		Xmlns: xmlns, Rules: []OwnershipControlsRule{{ObjectOwnership: ownership}},
	}
}

// ValidOwnership reports whether s is an Object Ownership setting.
func ValidOwnership(s string) bool {
	return s == OwnershipBucketOwnerEnforced || s == OwnershipBucketOwnerPreferred ||
		s == OwnershipObjectWriter
}

// ParseOwnershipControls decodes an OwnershipControls document, which must
// hold exactly one rule with a valid setting (Rule is "Required: Yes";
// that a second rule is refused is a choice).
func ParseOwnershipControls(doc []byte) (string, error) {
	var c OwnershipControls
	if err := xml.Unmarshal(doc, &c); err != nil || len(c.Rules) != 1 ||
		!ValidOwnership(c.Rules[0].ObjectOwnership) {
		return "", ErrMalformedXML()
	}
	return c.Rules[0].ObjectOwnership, nil
}

// The server-side encryption algorithms a general purpose bucket and a
// write take (API_ServerSideEncryptionByDefault and the PutObject API
// reference list aws:fsx and aws:backup too, which are for FSx and AWS
// Backup and are refused here).
const (
	SSEAES256  = "AES256"
	SSEKMS     = "aws:kms"
	SSEKMSDSSE = "aws:kms:dsse"
)

// ValidSSE reports whether s is an algorithm a general purpose bucket
// takes.
func ValidSSE(s string) bool {
	return s == SSEAES256 || s == SSEKMS || s == SSEKMSDSSE
}

// IsKMS reports whether s is an algorithm that takes a KMS key.
func IsKMS(s string) bool {
	return s == SSEKMS || s == SSEKMSDSSE
}

// ServerSideEncryptionConfiguration is the PutBucketEncryption request and
// GetBucketEncryption response document.
type ServerSideEncryptionConfiguration struct {
	XMLName xml.Name                   `xml:"ServerSideEncryptionConfiguration"`
	Xmlns   string                     `xml:"xmlns,attr,omitempty"`
	Rules   []ServerSideEncryptionRule `xml:"Rule"`
}

// ServerSideEncryptionRule is the one rule of a ServerSideEncryption
// configuration.
type ServerSideEncryptionRule struct {
	ApplyServerSideEncryptionByDefault *ServerSideEncryptionByDefault `xml:"ApplyServerSideEncryptionByDefault"`
	BlockedEncryptionTypes             *BlockedEncryptionTypes        `xml:"BlockedEncryptionTypes,omitempty"`
	BucketKeyEnabled                   bool                           `xml:"BucketKeyEnabled"`
}

// ServerSideEncryptionByDefault is a rule's default encryption.
type ServerSideEncryptionByDefault struct {
	SSEAlgorithm   string `xml:"SSEAlgorithm"`
	KMSMasterKeyID string `xml:"KMSMasterKeyID,omitempty"`
}

// BlockedEncryptionTypes is a rule's blocked encryption types, stored and
// returned as sent; nothing is blocked.
type BlockedEncryptionTypes struct {
	EncryptionType []string `xml:"EncryptionType"`
}

// NewDefaultEncryption returns the configuration of a bucket whose
// encryption was never set: "By default, all buckets have a default
// encryption configuration that uses server-side encryption with Amazon S3
// managed keys (SSE-S3)" (GetBucketEncryption API reference), and "By
// default, S3 Bucket Key is not enabled." (API_ServerSideEncryptionRule).
func NewDefaultEncryption() *ServerSideEncryptionConfiguration {
	return &ServerSideEncryptionConfiguration{
		Xmlns: xmlns,
		Rules: []ServerSideEncryptionRule{{
			ApplyServerSideEncryptionByDefault: &ServerSideEncryptionByDefault{
				SSEAlgorithm: SSEAES256,
			},
		}},
	}
}

// ParseEncryption decodes and validates a PutBucketEncryption document, or
// a stored one: one rule with a default whose algorithm is AES256, aws:kms
// or aws:kms:dsse, and a KMSMasterKeyID only for the KMS algorithms, "This
// parameter is allowed if and only if SSEAlgorithm is set to aws:kms or
// aws:kms:dsse" (API_ServerSideEncryptionByDefault). That a rule needs a
// default, that a second rule is refused, and the messages are choices.
// The key ID is not checked, as S3 does not check it: "Amazon S3 doesn't
// validate the KMS key ID provided in PutBucketEncryption requests."
func ParseEncryption(doc []byte) (*ServerSideEncryptionConfiguration, error) {
	var c ServerSideEncryptionConfiguration
	if err := xml.Unmarshal(doc, &c); err != nil || len(c.Rules) != 1 {
		return nil, ErrMalformedXML()
	}
	def := c.Rules[0].ApplyServerSideEncryptionByDefault
	if def == nil || !ValidSSE(def.SSEAlgorithm) {
		return nil, ErrMalformedXML()
	}
	if def.KMSMasterKeyID != "" && !IsKMS(def.SSEAlgorithm) {
		return nil, ErrInvalidArgument("a KMSMasterKeyID is not applicable if the default " +
			"sse algorithm is not aws:kms or aws:kms:dsse")
	}
	c.Xmlns = xmlns
	return &c, nil
}

// Default returns the configuration's default encryption.
func (c *ServerSideEncryptionConfiguration) Default() (sse, kmsKeyID string, bucketKey bool) {
	rule := c.Rules[0]
	return rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm,
		rule.ApplyServerSideEncryptionByDefault.KMSMasterKeyID, rule.BucketKeyEnabled
}

// Encode marshals doc, a response document, with the XML declaration, for
// storing as the bucket's configuration of its kind.
func Encode(doc any) ([]byte, error) {
	b, err := xml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("s3api: encode %T: %w", doc, err)
	}
	return append([]byte(xml.Header), b...), nil
}
