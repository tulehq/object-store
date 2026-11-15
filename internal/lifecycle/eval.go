package lifecycle

import (
	"net/http"
	"strings"
	"time"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// Midnight is the UTC midnight that starts t's UTC day.
func Midnight(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// EligibleAt is when an action days days after t applies: "adding the
// number of days specified in the rule to the object creation time and
// rounding up the resulting time to the next day at midnight UTC" (S3
// User Guide, "Lifecycle configuration elements"). A sum that falls
// exactly on a midnight rounds to the next one, strictly after it (L1).
func EligibleAt(t time.Time, days int) time.Time {
	return Midnight(t.UTC().AddDate(0, 0, days)).AddDate(0, 0, 1)
}

// Object is the current version of an object, as x-amz-expiration
// describes it.
type Object struct {
	LastModified time.Time
	Key          string
	Tags         []contracts.Tag
	Size         int64
}

// predicate is what a rule's filter, or its top-level Prefix, admits.
type predicate struct {
	gt, lt *int64
	prefix string
	tags   []s3api.Tag
}

func (r *Rule) predicate() predicate {
	switch f := r.Filter; {
	case r.Prefix != nil:
		return predicate{prefix: *r.Prefix}
	case f == nil:
		return predicate{}
	case f.And != nil:
		return predicate{
			gt: f.And.ObjectSizeGreaterThan, lt: f.And.ObjectSizeLessThan,
			prefix: deref(f.And.Prefix), tags: f.And.Tags,
		}
	default:
		p := predicate{
			gt: f.ObjectSizeGreaterThan, lt: f.ObjectSizeLessThan, prefix: deref(f.Prefix),
		}
		if f.Tag != nil {
			p.tags = []s3api.Tag{*f.Tag}
		}
		return p
	}
}

// admitsKey reports whether p admits an object of key and size, its tags
// aside. Both size bounds are exclusive ("The ObjectSizeGreaterThan and
// ObjectSizeLessThan filters exclude the specified values", S3 User
// Guide).
func (p *predicate) admitsKey(key string, size int64) bool {
	return strings.HasPrefix(key, p.prefix) && (p.gt == nil || size > *p.gt) &&
		(p.lt == nil || size < *p.lt)
}

// admitsTags reports whether p admits an object with tags: each tag of p
// must be among them with exactly its value, so a tag without a Value
// admits only an empty value; the object may have other tags.
func (p *predicate) admitsTags(tags []contracts.Tag) bool {
	for _, want := range p.tags {
		if !hasTag(tags, want) {
			return false
		}
	}
	return true
}

func hasTag(tags []contracts.Tag, want s3api.Tag) bool {
	for _, t := range tags {
		if t.Key == want.Key && t.Value == want.Value {
			return true
		}
	}
	return false
}

// earliest is the earliest time among the enabled rules of c that at
// says apply, and that rule's ID; a tie goes to the rule first in the
// document. Several rules that apply combine so that the soonest action
// is taken ("if two expiration policies overlap, the shorter expiration
// policy is honored", S3 User Guide, "How Amazon S3 handles conflicts in
// lifecycle configurations").
func (c *Configuration) earliest(
	at func(r *Rule) (time.Time, bool),
) (when time.Time, ruleID string, ok bool) {
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.Status != StatusEnabled {
			continue
		}
		if t, applies := at(r); applies && (!ok || t.Before(when)) {
			when, ruleID, ok = t, r.ID, true
		}
	}
	return when, ruleID, ok
}

// expiresAt is when r's Expiration by Days or Date expires a current
// version last modified at lastModified: a Date applies on that date, and
// keeps applying after it ("The date-based action is not a one-time
// action", S3 User Guide).
func (r *Rule) expiresAt(lastModified time.Time) (time.Time, bool) {
	e := r.Expiration
	switch {
	case e == nil:
		return time.Time{}, false
	case e.Days != nil:
		return EligibleAt(lastModified, *e.Days), true
	case e.Date != nil:
		return e.Date.UTC(), true
	}
	return time.Time{}, false
}

// Expiry is when o expires: the earliest expiry by the Expiration Days or
// Date of the enabled rules that admit o, and that rule's ID, or ok false
// when none does. A Date already past is answered as it is.
func (c *Configuration) Expiry(o Object) (at time.Time, ruleID string, ok bool) {
	at, ruleID, ok = c.earliest(func(r *Rule) (time.Time, bool) {
		t, has := r.expiresAt(o.LastModified)
		p := r.predicate()
		return t, has && p.admitsKey(o.Key, o.Size) && p.admitsTags(o.Tags)
	})
	return at, ruleID, ok
}

// ExpirationHeader is the x-amz-expiration value of an expiry at at by
// the rule ruleID: expiry-date="Sun, 19 Jan 2014 00:00:00 GMT",
// rule-id="<ruleID>", as the GetObject API reference's sample answers it,
// with the rule ID URL-encoded ("The value of rule-id is URL-encoded.").
func ExpirationHeader(at time.Time, ruleID string) string {
	return `expiry-date="` + at.UTC().Format(http.TimeFormat) + `", rule-id="` +
		uriEncode(ruleID) + `"`
}

// uriEncode is SigV4's UriEncode: the unreserved characters A-Z, a-z, 0-9,
// "-", ".", "_" and "~" as they are, and every other byte as %XX.
func uriEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if unreserved(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0xF])
	}
	return b.String()
}

func unreserved(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

// AbortDate is when an upload of key initiated at initiated is aborted:
// the earliest by the AbortIncompleteMultipartUpload of the enabled rules
// whose prefix admits key, and that rule's ID, or ok false when none
// does. Only the prefix applies ("determined by the key name prefix", S3
// User Guide); sizes do not, an upload having none yet (L9), and a rule
// with tags cannot abort uploads (V19).
func (c *Configuration) AbortDate(
	key string,
	initiated time.Time,
) (at time.Time, ruleID string, ok bool) {
	at, ruleID, ok = c.earliest(func(r *Rule) (time.Time, bool) {
		a := r.AbortIncompleteMultipartUpload
		if a == nil || a.DaysAfterInitiation == nil {
			return time.Time{}, false
		}
		p := r.predicate()
		return EligibleAt(initiated, *a.DaysAfterInitiation), strings.HasPrefix(key, p.prefix)
	})
	return at, ruleID, ok
}

// MayMatch reports whether the prefix of an enabled rule admits key, so
// that the versions of a key it does not admit need not be read.
func (c *Configuration) MayMatch(key string) bool {
	for i := range c.Rules {
		r := &c.Rules[i]
		if p := r.predicate(); r.Status == StatusEnabled && strings.HasPrefix(key, p.prefix) {
			return true
		}
	}
	return false
}

// HasUploadRules reports whether an enabled rule aborts incomplete
// uploads.
func (c *Configuration) HasUploadRules() bool {
	for i := range c.Rules {
		if c.Rules[i].Status == StatusEnabled && c.Rules[i].AbortIncompleteMultipartUpload != nil {
			return true
		}
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
