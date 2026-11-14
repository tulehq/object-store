package s3api

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"strings"

	"tule.dev/object-store/internal/contracts"
)

// The headers of object tagging: the tag set of a write, a copy's
// tagging directive, and the tag count a read answers (PutObject,
// CopyObject and GetObject API references).
const (
	HeaderTagging          = "X-Amz-Tagging"
	HeaderTaggingDirective = "X-Amz-Tagging-Directive"
	HeaderTaggingCount     = "X-Amz-Tagging-Count"
)

// maxObjectTags is S3's limit on an object's tags: "You can associate up
// to 10 tags with an object." ("Tagging your objects", S3 User Guide). The
// other rules are those of a bucket's tags (checkTag): "A tag key can be up
// to 128 Unicode characters in length, and tag values can be up to 256
// Unicode characters in length", and "Tags that are associated with an
// object must have unique tag keys".
const maxObjectTags = 10

// errTaggingHeader is S3's error for an x-amz-tagging header that does not
// decode, or names a key twice, as its clients report it; S3 documents
// neither.
func errTaggingHeader() *Error {
	return ErrInvalidArgument("The header 'x-amz-tagging' shall be encoded as UTF-8 then " +
		"URLEncoded URL query parameters without tag name duplicates.")
}

// ParseObjectTagging decodes and validates a PutObjectTagging document,
// as ParseTagging does a bucket's, with the object limits. An empty TagSet
// is valid, and removes the object's tags.
func ParseObjectTagging(doc []byte) ([]contracts.Tag, error) {
	var t Tagging
	if err := xml.Unmarshal(doc, &t); err != nil || t.TagSet == nil {
		return nil, ErrMalformedXML()
	}
	seen := make(map[string]bool, len(t.TagSet.Tags))
	tags := make([]contracts.Tag, 0, len(t.TagSet.Tags))
	for _, tag := range t.TagSet.Tags {
		if seen[tag.Key] {
			return nil, ErrInvalidTag("Cannot provide multiple Tags with the same key")
		}
		seen[tag.Key] = true
		tags = append(tags, contracts.Tag(tag))
	}
	if err := checkObjectTags(tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// RequestTags returns the tag set r's x-amz-tagging header carries, in its
// order, or nil when it carries none or an empty one. The header is "The
// tag-set for the object. The tag-set must be encoded as URL Query
// parameters. (For example, "Key1=Value1")" (PutObject API reference): it
// is split at "&", each pair at its first "=", and each half decoded as a
// query component, "+" being a space; a pair without "=" has an empty
// value and an empty pair is skipped. Only the first value of a repeated
// header is read, as for every other header.
func RequestTags(r *http.Request) ([]contracts.Tag, error) {
	v := r.Header.Get(HeaderTagging)
	if v == "" {
		return nil, nil
	}
	var tags []contracts.Tag
	seen := map[string]bool{}
	for pair := range strings.SplitSeq(v, "&") {
		if pair == "" {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(pair, "=")
		k, kErr := url.QueryUnescape(rawKey)
		val, vErr := url.QueryUnescape(rawValue)
		if kErr != nil || vErr != nil || seen[k] {
			return nil, errTaggingHeader()
		}
		seen[k] = true
		tags = append(tags, contracts.Tag{Key: k, Value: val})
	}
	if err := checkObjectTags(tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// checkObjectTags applies the object tag limits to tags, whose keys are
// unique, with the errors of a bucket's (checkTag); the count's message is
// a choice.
func checkObjectTags(tags []contracts.Tag) error {
	if len(tags) > maxObjectTags {
		return ErrInvalidTag("Object tags cannot be greater than 10")
	}
	for _, tag := range tags {
		if err := checkTag(Tag(tag)); err != nil {
			return err
		}
	}
	return nil
}

// NewTagging is the GetObjectTagging response document of tags: an empty
// TagSet for an object without tags.
func NewTagging(tags []contracts.Tag) *Tagging {
	set := &TagSet{Tags: make([]Tag, len(tags))}
	for i, tag := range tags {
		set.Tags[i] = Tag(tag)
	}
	return &Tagging{Xmlns: xmlns, TagSet: set}
}
