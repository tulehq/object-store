package lifecycle

import (
	"slices"
	"unicode/utf8"

	"tule.dev/object-store/internal/s3api"
)

// The validation of a configuration. Shape and type errors are
// MalformedXML, the schema's error; the others are InvalidArgument or
// InvalidRequest. S3 documents the constraints cited below; except for
// the InvalidRequest of NewerNoncurrentVersions without a Filter, which
// the S3 User Guide names, it documents neither the codes nor the
// messages, so those are choices. The messages marked OBS are strings S3
// or S3-compatible servers were seen to answer, not documentation.
//
// First, for the whole document (decode in parse.go), MalformedXML for V1
// a body that is not XML or has another root, an element repeated where
// one is allowed, and a number, boolean or date that is empty or does not
// parse, including a count of days or versions that does not fit S3's
// 32-bit Integer; and V2 no rule; then V3 more than 1,000 rules.
//
// Then the checks of each rule, in document order and in this order, the
// first that fails being answered:
//
//   - V4 an ID over 255 characters, V6 a Status other than Enabled or
//     Disabled;
//   - V7 neither or both of a Filter and a top-level Prefix;
//   - V9 a Filter with more than one predicate, V10 an And with fewer
//     than two, V11 duplicate tag keys, V12 a tag outside the object tag
//     limits (s3api.CheckTag), V13 a negative ObjectSizeGreaterThan, an
//     ObjectSizeLessThan under 1, a size over 50 TB, or bounds that admit
//     nothing;
//   - V14 no action;
//   - V15 an empty Expiration or one with both Days and Date, V16
//     ExpiredObjectDeleteMarker with Days or Date, V17 Days under 1, V18
//     a Date not at midnight UTC;
//   - V19 ExpiredObjectDeleteMarker or AbortIncompleteMultipartUpload in
//     a rule whose filter uses tags;
//   - V20 DaysAfterInitiation missing or under 1;
//   - V21 NoncurrentVersionExpiration without NoncurrentDays, V22
//     NoncurrentDays under 1, V23 NewerNoncurrentVersions outside 1 to
//     100;
//   - V24 NewerNoncurrentVersions in a rule without a Filter;
//   - V25 a Transition StorageClass missing or unknown, a Transition with
//     neither or both of Days and Date, V26 a Transition's Days under 0,
//     V18 its Date not at midnight UTC; then the same for each
//     NoncurrentVersionTransition, its NoncurrentDays missing or under 0,
//     and its NewerNoncurrentVersions outside 1 to 100;
//   - V29 Days in one of the rule's Expiration and Transitions and Date
//     in another.
//
// Then, across rules: V5 a repeated ID, and V8 rules with a top-level
// Prefix mixed with rules with a Filter.
const (
	// V3: "An Amazon S3 Lifecycle configuration can have up to 1,000
	// rules." (PutBucketLifecycleConfiguration API reference).
	msgTooManyRules = "The number of lifecycle rules should not exceed allowed limit of 1000 rules."
	// V4: "The value cannot be longer than 255 characters." (LifecycleRule).
	msgIDTooLong = "ID length should not exceed allowed limit of 255"
	// V5 (OBS, MinIO): "The <ID> element uniquely identifies a rule"
	// (S3 User Guide, "Lifecycle configuration elements").
	msgDuplicateID = "Rule ID must be unique. Found same ID for more than one rule"
	// V8 (OBS, a forum report of S3's answer); not documented by AWS.
	msgMixedPrefix = "Filter element can only be used in Lifecycle V2."
	// V11: "each tag key must be unique" (S3 User Guide).
	msgDuplicateTagKeys = "Duplicate Tag Keys are not allowed."
	// V13: "the ObjectSizeGreaterThan integer must be less than the
	// ObjectSizeLessThan value" (S3 User Guide).
	msgSizeRange       = "ObjectSizeGreaterThan must be less than ObjectSizeLessThan"
	msgSizeGreaterThan = "ObjectSizeGreaterThan must be a non-negative integer"
	msgSizeLessThan    = "ObjectSizeLessThan must be a positive integer"
	// V13: "Maximum filter size is 50 TB." (S3 User Guide, "Adding
	// filters to Lifecycle rules").
	msgSizeMaximum = "ObjectSizeGreaterThan and ObjectSizeLessThan must not exceed 50 TB " +
		"(54975581388800 bytes)"
	// V16: the sentence of the LifecycleExpiration API reference.
	msgMarkerWithDays = "ExpiredObjectDeleteMarker cannot be specified with Days or Date in a " +
		"Lifecycle Expiration Policy."
	// V17: "The value must be a non-zero positive integer."
	// (LifecycleExpiration).
	msgExpirationDays = "'Days' for Expiration action must be a positive integer"
	// V18 (OBS, MinIO): "The time is always midnight UTC."
	// (LifecycleExpiration, Transition).
	msgDateMidnight = "'Date' must be at midnight GMT"
	// V19: "You can't specify this lifecycle action in a rule that has a
	// filter that uses object tags." (S3 User Guide, for both actions).
	msgMarkerWithTags = "ExpiredObjectDeleteMarker cannot be specified with Tags."
	msgAbortWithTags  = "AbortIncompleteMultipartUpload cannot be specified with Tags."
	// V20: the AbortIncompleteMultipartUpload API reference is silent; a
	// positive count is required as for Days and NoncurrentDays (L19).
	msgDaysAfterInitiation = "'DaysAfterInitiation' for AbortIncompleteMultipartUpload " +
		"action must be a positive integer"
	// V22: "The value must be a non-zero positive integer."
	// (NoncurrentVersionExpiration).
	msgNoncurrentDays = "'NoncurrentDays' for NoncurrentVersionExpiration action must be " +
		"a positive integer"
	// V23: "You can specify up to 100 noncurrent versions to retain."
	// (NoncurrentVersionExpiration, NoncurrentVersionTransition); "(between
	// 1 and 100)" (S3 User Guide). The positive-integer message is OBS (a
	// blog's report of S3's answer).
	msgNewerExpirationPositive = "NewerNoncurrentVersions for NoncurrentVersionExpiration " +
		"action must be a positive integer"
	msgNewerExpirationMax = "NewerNoncurrentVersions for NoncurrentVersionExpiration " +
		"action must not exceed 100"
	msgNewerTransitionPositive = "NewerNoncurrentVersions for NoncurrentVersionTransition " +
		"action must be a positive integer"
	msgNewerTransitionMax = "NewerNoncurrentVersions for NoncurrentVersionTransition " +
		"action must not exceed 100"
	// V24: "If you don't specify a <Filter> element, Amazon S3 generates an
	// InvalidRequest error" (S3 User Guide).
	msgNewerWithoutFilter = "NewerNoncurrentVersions element can only be used with a " +
		"Filter element."
	// V26: "The value can be 0 or any positive integer." (Transition); the
	// API reference's Example 4 puts a NoncurrentDays of 0.
	msgTransitionDays           = "'Days' in Transition action must be greater than or equal to zero"
	msgNoncurrentTransitionDays = "'NoncurrentDays' in NoncurrentVersionTransition action " +
		"must be greater than or equal to zero"
	// V29: "you can't use both Date and Days in the same rule" (S3 User
	// Guide, "Examples of S3 Lifecycle configurations"); it names no error,
	// and InvalidArgument and the message are choices.
	msgDaysAndDate = "Days and Date cannot both be used in the Expiration and Transition " +
		"actions of one rule"
	// V27: the header's Valid Values (PutBucketLifecycleConfiguration).
	msgTransitionDefault = "Invalid value for header x-amz-transition-default-minimum-object-size"

	maxIDLength     = 255
	maxNewerVersion = 100

	// maxFilterSize is the 50 TB "maximum filter size", read as 50 TiB,
	// 50 * 2^40 bytes. AWS does not say which; the binary reading is the
	// larger, so no size S3 takes under the decimal one, 50 * 10^12, is
	// refused here.
	maxFilterSize = 50 << 40
)

// storageClasses are a transition's Valid Values (Transition and
// NoncurrentVersionTransition API references).
var storageClasses = []string{
	"GLACIER", "STANDARD_IA", "ONEZONE_IA", "INTELLIGENT_TIERING", "DEEP_ARCHIVE", "GLACIER_IR",
}

// checkRule runs the checks of one rule, in the order above (V4 to V29).
func checkRule(r *Rule) error {
	if err := checkHeader(r); err != nil {
		return err
	}
	if (r.Filter == nil) == (r.Prefix == nil) {
		return s3api.ErrMalformedXML()
	}
	if r.Filter != nil {
		if err := checkFilter(r.Filter); err != nil {
			return err
		}
	}
	return checkActions(r)
}

// checkHeader checks a rule's ID and Status (V4, V6).
func checkHeader(r *Rule) error {
	if utf8.RuneCountInString(r.ID) > maxIDLength {
		return s3api.ErrInvalidArgument(msgIDTooLong)
	}
	if r.Status != StatusEnabled && r.Status != StatusDisabled {
		return s3api.ErrMalformedXML()
	}
	return nil
}

// checkFilter checks the shape of a Filter and its tags and sizes (V9 to
// V13).
func checkFilter(f *Filter) error {
	if count(f.And != nil, f.ObjectSizeGreaterThan != nil, f.ObjectSizeLessThan != nil,
		f.Prefix != nil, f.Tag != nil) > 1 {
		return s3api.ErrMalformedXML()
	}
	if a := f.And; a != nil && count(a.ObjectSizeGreaterThan != nil,
		a.ObjectSizeLessThan != nil, a.Prefix != nil)+len(a.Tags) < 2 {
		return s3api.ErrMalformedXML()
	}
	tags := []s3api.Tag(nil)
	gt, lt := f.ObjectSizeGreaterThan, f.ObjectSizeLessThan
	if f.Tag != nil {
		tags = []s3api.Tag{*f.Tag}
	}
	if f.And != nil {
		tags, gt, lt = f.And.Tags, f.And.ObjectSizeGreaterThan, f.And.ObjectSizeLessThan
	}
	if err := checkTags(tags); err != nil {
		return err
	}
	return checkSizes(gt, lt)
}

func checkTags(tags []s3api.Tag) error {
	seen := make(map[string]bool, len(tags))
	for _, tag := range tags {
		if seen[tag.Key] {
			return s3api.ErrInvalidArgument(msgDuplicateTagKeys)
		}
		seen[tag.Key] = true
	}
	for _, tag := range tags {
		if err := s3api.CheckTag(tag); err != nil {
			return err //nolint:wrapcheck // the S3 error the request fails with
		}
	}
	return nil
}

func checkSizes(gt, lt *int64) error {
	switch {
	case gt != nil && *gt < 0:
		return s3api.ErrInvalidArgument(msgSizeGreaterThan)
	case lt != nil && *lt < 1:
		return s3api.ErrInvalidArgument(msgSizeLessThan)
	case overMaximum(gt) || overMaximum(lt):
		return s3api.ErrInvalidArgument(msgSizeMaximum)
	case gt != nil && lt != nil && *gt >= *lt:
		return s3api.ErrInvalidArgument(msgSizeRange)
	}
	return nil
}

// overMaximum reports whether a size bound is over maxFilterSize.
func overMaximum(n *int64) bool {
	return n != nil && *n > maxFilterSize
}

// checkActions checks a rule's actions (V14 to V26).
func checkActions(r *Rule) error {
	if r.Expiration == nil && len(r.Transitions) == 0 &&
		len(r.NoncurrentVersionTransitions) == 0 && r.NoncurrentVersionExpiration == nil &&
		r.AbortIncompleteMultipartUpload == nil {
		return s3api.ErrMalformedXML()
	}
	for _, check := range []func(*Rule) error{
		func(r *Rule) error { return checkExpiration(r.Expiration) },
		checkTagged,
		func(r *Rule) error { return checkAbort(r.AbortIncompleteMultipartUpload) },
		func(r *Rule) error { return checkNoncurrentExpiration(r.NoncurrentVersionExpiration) },
		checkNewerWithFilter,
		checkTransitions,
		checkDaysAndDate,
	} {
		if err := check(r); err != nil {
			return err
		}
	}
	return nil
}

// checkNewerWithFilter refuses NewerNoncurrentVersions in a rule without
// a Filter (V24).
func checkNewerWithFilter(r *Rule) error {
	if r.Filter == nil && newerNoncurrent(r) {
		return s3api.ErrInvalidRequest(msgNewerWithoutFilter)
	}
	return nil
}

// checkDaysAndDate refuses a rule whose Expiration and Transitions mix
// Days and Date (V29).
func checkDaysAndDate(r *Rule) error {
	days, date := false, false
	if e := r.Expiration; e != nil {
		days, date = e.Days != nil, e.Date != nil
	}
	for _, t := range r.Transitions {
		days, date = days || t.Days != nil, date || t.Date != nil
	}
	if days && date {
		return s3api.ErrInvalidArgument(msgDaysAndDate)
	}
	return nil
}

// count is how many of conds hold.
func count(conds ...bool) int {
	n := 0
	for _, c := range conds {
		if c {
			n++
		}
	}
	return n
}

// checkTransitions checks a rule's transitions (V25, V26, V18, V23).
func checkTransitions(r *Rule) error {
	for i := range r.Transitions {
		if err := checkTransition(&r.Transitions[i]); err != nil {
			return err
		}
	}
	for i := range r.NoncurrentVersionTransitions {
		if err := checkNoncurrentTransition(&r.NoncurrentVersionTransitions[i]); err != nil {
			return err
		}
	}
	return nil
}

// checkExpiration checks an Expiration (V15 to V18).
func checkExpiration(e *Expiration) error {
	if e == nil {
		return nil
	}
	timed := e.Days != nil || e.Date != nil
	switch {
	case !timed && e.ExpiredObjectDeleteMarker == nil, e.Days != nil && e.Date != nil:
		return s3api.ErrMalformedXML()
	case e.ExpiredObjectDeleteMarker != nil && timed:
		return s3api.ErrInvalidArgument(msgMarkerWithDays)
	case e.Days != nil && *e.Days < 1:
		return s3api.ErrInvalidArgument(msgExpirationDays)
	}
	return checkMidnight(e.Date)
}

func checkMidnight(d *Date) error {
	if d != nil && !d.Equal(Midnight(d.Time)) {
		return s3api.ErrInvalidArgument(msgDateMidnight)
	}
	return nil
}

// checkTagged refuses the actions a tag filter may not have (V19).
func checkTagged(r *Rule) error {
	if !usesTags(r) {
		return nil
	}
	if r.Expiration != nil && r.Expiration.ExpiredObjectDeleteMarker != nil {
		return s3api.ErrInvalidRequest(msgMarkerWithTags)
	}
	if r.AbortIncompleteMultipartUpload != nil {
		return s3api.ErrInvalidRequest(msgAbortWithTags)
	}
	return nil
}

func usesTags(r *Rule) bool {
	f := r.Filter
	return f != nil && (f.Tag != nil || f.And != nil && len(f.And.Tags) > 0)
}

// checkAbort checks an AbortIncompleteMultipartUpload (V20).
func checkAbort(a *AbortIncompleteMultipartUpload) error {
	switch {
	case a == nil:
		return nil
	case a.DaysAfterInitiation == nil || *a.DaysAfterInitiation < 1:
		return s3api.ErrInvalidArgument(msgDaysAfterInitiation)
	}
	return nil
}

// checkNoncurrentExpiration checks a NoncurrentVersionExpiration (V21 to
// V23).
func checkNoncurrentExpiration(e *NoncurrentVersionExpiration) error {
	switch {
	case e == nil:
		return nil
	case e.NoncurrentDays == nil:
		return s3api.ErrMalformedXML()
	case *e.NoncurrentDays < 1:
		return s3api.ErrInvalidArgument(msgNoncurrentDays)
	}
	return checkNewer(e.NewerNoncurrentVersions, msgNewerExpirationPositive,
		msgNewerExpirationMax)
}

func checkNewer(n *int, positive, maximum string) error {
	switch {
	case n == nil:
		return nil
	case *n < 1:
		return s3api.ErrInvalidArgument(positive)
	case *n > maxNewerVersion:
		return s3api.ErrInvalidArgument(maximum)
	}
	return nil
}

// newerNoncurrent reports whether a noncurrent action of r names
// NewerNoncurrentVersions.
func newerNoncurrent(r *Rule) bool {
	if e := r.NoncurrentVersionExpiration; e != nil && e.NewerNoncurrentVersions != nil {
		return true
	}
	for _, t := range r.NoncurrentVersionTransitions {
		if t.NewerNoncurrentVersions != nil {
			return true
		}
	}
	return false
}

// checkTransition checks a Transition (V25, V26, V18).
func checkTransition(t *Transition) error {
	switch {
	case !slices.Contains(storageClasses, t.StorageClass), (t.Days == nil) == (t.Date == nil):
		return s3api.ErrMalformedXML()
	case t.Days != nil && *t.Days < 0:
		return s3api.ErrInvalidArgument(msgTransitionDays)
	}
	return checkMidnight(t.Date)
}

// checkNoncurrentTransition checks a NoncurrentVersionTransition (V25,
// V26, V23).
func checkNoncurrentTransition(t *NoncurrentVersionTransition) error {
	switch {
	case !slices.Contains(storageClasses, t.StorageClass):
		return s3api.ErrMalformedXML()
	case t.NoncurrentDays == nil || *t.NoncurrentDays < 0:
		return s3api.ErrInvalidArgument(msgNoncurrentTransitionDays)
	}
	return checkNewer(t.NewerNoncurrentVersions, msgNewerTransitionPositive,
		msgNewerTransitionMax)
}

// checkRules runs the checks of each rule in document order, then those
// across rules (V5, V8).
func checkRules(rules []Rule) error {
	for i := range rules {
		if err := checkRule(&rules[i]); err != nil {
			return err
		}
	}
	seen := make(map[string]bool, len(rules))
	for i := range rules {
		id := rules[i].ID
		if id == "" {
			continue
		}
		if seen[id] {
			return s3api.ErrInvalidArgument(msgDuplicateID)
		}
		seen[id] = true
	}
	prefixed := 0
	for i := range rules {
		if rules[i].Prefix != nil {
			prefixed++
		}
	}
	if prefixed != 0 && prefixed != len(rules) {
		return s3api.ErrInvalidRequest(msgMixedPrefix)
	}
	return nil
}
