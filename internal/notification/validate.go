package notification

import (
	"strings"
	"unicode/utf8"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// The validation of a configuration. Shape errors are MalformedXML, the
// schema's error; the others are InvalidArgument. S3 documents the
// constraints cited below and none of the codes or messages: those marked
// OBS are strings S3 was seen to answer, not documentation, and the others
// are choices.
//
// First, for the whole document (Parse and decode in parse.go),
// MalformedXML for V1: a body that is not XML or has another root, a
// configuration without its ARN element or without an Event, an element
// repeated where one is allowed, and two EventBridgeConfiguration
// elements.
//
// Then the checks of each configuration, in document order and in this
// order, the first that fails being answered:
//
//   - V2 an Event that is not one of S3's event types, s3:TestEvent
//     included;
//   - V3 a FilterRule Name other than prefix or suffix, in any case (it is
//     stored in lower case), V4 two prefix or two suffix rules, V5 a Value
//     over 1,024 characters;
//   - V6 an Id over 255 characters;
//   - V7 an ARN that does not parse, names another service than its
//     element's, or another partition than aws;
//   - V8 an ARN of another region than the bucket's, or of a FIFO queue or
//     topic, which are never skipped.
//
// Then, across configurations: V6 a repeated Id, and V9 two
// configurations that overlap.
const (
	// V2 (OBS).
	msgEvent = "The event is not supported for notifications"
	// V3 (OBS).
	msgRuleName = "filter rule name must be either prefix or suffix"
	// V4 (OBS).
	msgTwoPrefixes = "Cannot specify more than one prefix rule in a filter."
	msgTwoSuffixes = "Cannot specify more than one suffix rule in a filter."
	// V5: "The maximum length is 1,024 characters." (FilterRule).
	msgValueTooLong = "The filter rule value cannot be longer than 1024 characters."
	// V6: "An optional unique identifier for configurations in a
	// notification configuration." (QueueConfiguration).
	msgIDTooLong   = "The configuration Id cannot be longer than 255 characters."
	msgDuplicateID = "Configuration Id must be unique."
	// V7 (OBS).
	msgARN = "The ARN is not well formed"
	// V8: "The Amazon SQS queue must be in the same AWS Region as your
	// Amazon S3 bucket" and the same for topics and functions; "Amazon
	// Simple Queue Service FIFO (First-In-First-Out) queues aren't
	// supported", "SNS FIFO is not allowed" (S3 User Guide, "Event
	// notification types and destinations"). The region message is OBS.
	msgRegion = "The notification destination service region is not valid for the bucket " +
		"location constraint"
	msgFIFO = "FIFO queues and topics are not supported as notification destinations"
	// V9 (OBS). "Notification configurations that use Filter cannot define
	// filtering rules with overlapping prefixes, overlapping suffixes, or
	// prefix and suffix overlapping" (S3 User Guide, "Configuring event
	// notifications using object key name filtering").
	msgOverlap = "Configurations overlap. Configurations on the same bucket cannot share a " +
		"common event type."
	// N21.
	msgSkip = "Invalid value for header x-amz-skip-destination-validation"
)

// checkConfigs runs the checks of each configuration in document order,
// then those across configurations (V6, V9).
func checkConfigs(configs []decodedConfig) error {
	for i := range configs {
		if err := checkConfig(&configs[i]); err != nil {
			return err
		}
	}
	seen := make(map[string]bool, len(configs))
	for i := range configs {
		id := configs[i].config.ID
		if id == "" {
			continue
		}
		if seen[id] {
			return s3api.ErrInvalidArgument(msgDuplicateID)
		}
		seen[id] = true
	}
	return checkOverlap(configs)
}

// checkConfig runs the checks of one configuration, in the order above
// (V2 to V8). It stores the filter rule names in lower case.
func checkConfig(d *decodedConfig) error {
	c := &d.config
	for _, ev := range c.Events {
		if _, ok := eventTypes[ev]; !ok {
			return s3api.ErrInvalidArgument(msgEvent)
		}
	}
	if err := checkFilter(c.Filter); err != nil {
		return err
	}
	if utf8.RuneCountInString(c.ID) > MaxID {
		return s3api.ErrInvalidArgument(msgIDTooLong)
	}
	arn, err := ParseDestinationARN(d.kind.typ, c.ARN)
	if err != nil {
		return err
	}
	arg := []s3api.ErrorArgument{{Name: d.kind.arn, Value: c.ARN}}
	switch {
	case arn.Region != contracts.Region:
		return argumentError(msgRegion, arg)
	case d.kind.typ != functionKind.typ && strings.HasSuffix(arn.Resource, ".fifo"):
		return argumentError(msgFIFO, arg)
	}
	return nil
}

// argumentError is an InvalidArgument about the named values args.
func argumentError(msg string, args []s3api.ErrorArgument) *s3api.Error {
	e := s3api.ErrInvalidArgument(msg)
	e.Arguments = args
	return e
}

// checkFilter checks a filter's rules (V3 to V5) and stores their names in
// lower case.
func checkFilter(f *Filter) error {
	if f == nil {
		return nil
	}
	prefixes, suffixes := 0, 0
	for i := range f.S3Key.Rules {
		r := &f.S3Key.Rules[i]
		r.Name = strings.ToLower(r.Name)
		switch r.Name {
		case RulePrefix:
			prefixes++
		case RuleSuffix:
			suffixes++
		default:
			return s3api.ErrInvalidArgument(msgRuleName)
		}
		switch {
		case prefixes > 1:
			return s3api.ErrInvalidArgument(msgTwoPrefixes)
		case suffixes > 1:
			return s3api.ErrInvalidArgument(msgTwoSuffixes)
		case utf8.RuneCountInString(r.Value) > MaxFilterValue:
			return s3api.ErrInvalidArgument(msgValueTooLong)
		}
	}
	return nil
}

// checkOverlap refuses two configurations, of any destination types (N12),
// whose expanded event types intersect and whose filters both admit some
// key (V9). "If a given string can end with both suffixes, the two
// suffixes are considered overlapping. ... The same is true for prefixes",
// and "Any existing notification configuration rule is assumed to have a
// default prefix and suffix that match any other prefix and suffix" (S3
// User Guide): two filters admit a common key exactly when their prefixes
// overlap and their suffixes overlap, the key being the longer prefix
// followed by the longer suffix.
func checkOverlap(configs []decodedConfig) error {
	for i := range configs {
		for j := i + 1; j < len(configs); j++ {
			a, b := &configs[i].config, &configs[j].config
			if !shareEvent(a.Events, b.Events) {
				continue
			}
			ap, as := a.Filter.affixes()
			bp, bs := b.Filter.affixes()
			if (strings.HasPrefix(ap, bp) || strings.HasPrefix(bp, ap)) &&
				(strings.HasSuffix(as, bs) || strings.HasSuffix(bs, as)) {
				return s3api.ErrInvalidArgument(msgOverlap)
			}
		}
	}
	return nil
}

// affixes returns f's prefix and suffix, "" for a rule it does not have.
func (f *Filter) affixes() (prefix, suffix string) {
	if f == nil {
		return "", ""
	}
	for _, r := range f.S3Key.Rules {
		switch r.Name {
		case RulePrefix:
			prefix = r.Value
		case RuleSuffix:
			suffix = r.Value
		}
	}
	return prefix, suffix
}

// shareEvent reports whether two configurations' event types, wildcards
// expanded, have one in common.
func shareEvent(a, b []string) bool {
	set := make(map[contracts.EventName]bool)
	for _, ev := range a {
		for _, name := range eventTypes[ev] {
			set[name] = true
		}
	}
	for _, ev := range b {
		for _, name := range eventTypes[ev] {
			if set[name] {
				return true
			}
		}
	}
	return false
}
