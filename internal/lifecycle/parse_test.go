package lifecycle

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"tule.dev/object-store/internal/s3api"
)

func doc(rules ...string) string {
	return `<LifecycleConfiguration xmlns="` + xmlns + `">` + strings.Join(rules, "") +
		`</LifecycleConfiguration>`
}

func rule(inner string) string {
	return "<Rule>" + inner + "</Rule>"
}

const (
	enabled    = "<Status>Enabled</Status>"
	logsFilter = "<Filter><Prefix>logs/</Prefix></Filter>"
	expire3    = "<Expiration><Days>3</Days></Expiration>"
)

// validRule is a rule with every element a valid rule needs.
func validRule(id string) string {
	return rule("<ID>" + id + "</ID>" + logsFilter + enabled + expire3)
}

func requireError(t *testing.T, err error, status int, code, message string) {
	t.Helper()
	var s3Err *s3api.Error
	if !errors.As(err, &s3Err) {
		t.Fatalf("got %v, want %s", err, code)
	}
	if s3Err.Status != status || s3Err.Code != code || message != "" && s3Err.Message != message {
		t.Fatalf("got %d %s %q, want %d %s %q", s3Err.Status, s3Err.Code, s3Err.Message,
			status, code, message)
	}
}

func manyRules(n int) string {
	rules := make([]string, n)
	for i := range rules {
		rules[i] = validRule("r" + strconv.Itoa(i))
	}
	return doc(rules...)
}

const (
	mx = "MalformedXML"
	ia = "InvalidArgument"
	ir = "InvalidRequest"

	tagFilter = "<Tag><Key>k</Key><Value>v</Value></Tag>"
)

// filterRule is a rule with filter and actions.
func filterRule(filter, actions string) string {
	return doc(rule("<ID>r</ID><Filter>" + filter + "</Filter>" + enabled + actions))
}

// withActions is a rule of the logs/ prefix with actions.
func withActions(actions string) string {
	return filterRule("<Prefix>logs/</Prefix>", actions)
}

// refuseCases are the documents Parse refuses (design table V1 to V28),
// with the code and message; "" is the standard MalformedXML message.
var refuseCases = []struct {
	name    string
	body    string
	code    string
	message string
}{
	{"V1 not XML", "not xml", mx, ""},
	{"V1 wrong root", "<Foo>" + validRule("r") + "</Foo>", mx, ""},
	{"V1 non-integer Days", withActions("<Expiration><Days>x</Days></Expiration>"), mx, ""},
	{
		"V1 Days over 32 bits",
		withActions("<Expiration><Days>2147483648</Days></Expiration>"), mx, "",
	},
	{
		"V1 non-integer size",
		filterRule("<ObjectSizeLessThan>a</ObjectSizeLessThan>", expire3),
		mx,
		"",
	},
	{
		"V1 unparsable Date",
		withActions("<Expiration><Date>soon</Date></Expiration>"), mx, "",
	},
	{
		"V1 empty ObjectSizeGreaterThan",
		filterRule("<ObjectSizeGreaterThan></ObjectSizeGreaterThan>", expire3), mx, "",
	},
	{"V1 empty Days", withActions("<Expiration><Days></Days></Expiration>"), mx, ""},
	{"V1 empty Days, self-closed", withActions("<Expiration><Days/></Expiration>"), mx, ""},
	{
		"V1 empty ExpiredObjectDeleteMarker",
		withActions("<Expiration><ExpiredObjectDeleteMarker/></Expiration>"), mx, "",
	},
	{
		"V1 non-boolean ExpiredObjectDeleteMarker", withActions(
			"<Expiration><ExpiredObjectDeleteMarker>yes</ExpiredObjectDeleteMarker></Expiration>"),
		mx, "",
	},
	{"V1 empty Date", withActions("<Expiration><Date></Date></Expiration>"), mx, ""},
	{"V1 empty NoncurrentDays", withActions("<NoncurrentVersionExpiration><NoncurrentDays>" +
		"</NoncurrentDays></NoncurrentVersionExpiration>"), mx, ""},
	{"V1 empty NewerNoncurrentVersions", withActions("<NoncurrentVersionExpiration>" +
		"<NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions></NewerNoncurrentVersions>" +
		"</NoncurrentVersionExpiration>"), mx, ""},
	{"V1 empty DaysAfterInitiation", withActions("<AbortIncompleteMultipartUpload>" +
		"<DaysAfterInitiation></DaysAfterInitiation></AbortIncompleteMultipartUpload>"), mx, ""},
	{"V1 empty Transition Days", withActions("<Transition><Days></Days>" +
		"<StorageClass>GLACIER</StorageClass></Transition>"), mx, ""},
	{"V1 empty And size", filterRule("<And><Prefix>a</Prefix><ObjectSizeLessThan>"+
		"</ObjectSizeLessThan></And>", expire3), mx, ""},
	{
		"V1 a type error in a later rule wins over an earlier rule's check",
		doc(rule("<ID>a</ID>"+logsFilter+enabled+"<Expiration><Days>0</Days></Expiration>"),
			rule("<ID>b</ID>"+logsFilter+enabled+"<Expiration><Days>x</Days></Expiration>")),
		mx, "",
	},
	{
		"V1 repeated Expiration Days",
		withActions("<Expiration><Days>3650</Days><Days>1</Days></Expiration>"), mx, "",
	},
	{"V1 repeated ExpiredObjectDeleteMarker", withActions("<Expiration>" +
		"<ExpiredObjectDeleteMarker>false</ExpiredObjectDeleteMarker>" +
		"<ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>"), mx, ""},
	{"V1 repeated Expiration", withActions(expire3 + expire3), mx, ""},
	{"V1 repeated Transition StorageClass", withActions("<Transition><Days>1</Days>" +
		"<StorageClass>GLACIER</StorageClass><StorageClass>STANDARD_IA</StorageClass>" +
		"</Transition>"), mx, ""},
	{"V1 repeated Transition Date", withActions("<Transition><Date>2030-01-01</Date>" +
		"<Date>2031-01-01</Date><StorageClass>GLACIER</StorageClass></Transition>"), mx, ""},
	{
		"V1 repeated NoncurrentVersionExpiration NoncurrentDays",
		withActions("<NoncurrentVersionExpiration><NoncurrentDays>30</NoncurrentDays>" +
			"<NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>"), mx, "",
	},
	{"V1 repeated NoncurrentVersionTransition NewerNoncurrentVersions", withActions(
		"<NoncurrentVersionTransition><NoncurrentDays>1</NoncurrentDays>" +
			"<NewerNoncurrentVersions>1</NewerNoncurrentVersions><NewerNoncurrentVersions>2" +
			"</NewerNoncurrentVersions><StorageClass>GLACIER</StorageClass>" +
			"</NoncurrentVersionTransition>"), mx, ""},
	{"V1 repeated DaysAfterInitiation", withActions("<AbortIncompleteMultipartUpload>" +
		"<DaysAfterInitiation>7</DaysAfterInitiation><DaysAfterInitiation>1" +
		"</DaysAfterInitiation></AbortIncompleteMultipartUpload>"), mx, ""},
	{
		"V1 repeated AbortIncompleteMultipartUpload", withActions(
			"<AbortIncompleteMultipartUpload><DaysAfterInitiation>7</DaysAfterInitiation>" +
				"</AbortIncompleteMultipartUpload><AbortIncompleteMultipartUpload>" +
				"<DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload>"),
		mx, "",
	},
	{"V1 repeated tag Value", filterRule("<Tag><Key>k</Key><Value>a</Value><Value>b</Value>"+
		"</Tag>", expire3), mx, ""},
	{"V1 repeated tag Key in And", filterRule("<And><Prefix>a</Prefix><Tag><Key>k</Key>"+
		"<Key>j</Key><Value>v</Value></Tag></And>", expire3), mx, ""},
	{
		"V1 repeated ID", doc(rule("<ID>a</ID><ID>b</ID>" + logsFilter + enabled + expire3)),
		mx, "",
	},
	{
		"V1 ObjectSizeLessThan over 64 bits",
		filterRule("<ObjectSizeLessThan>9223372036854775808</ObjectSizeLessThan>", expire3),
		mx, "",
	},
	{"V2 no rule", doc(), mx, ""},
	{
		"V3 1001 rules", manyRules(MaxRules + 1), ir,
		"The number of lifecycle rules should not exceed allowed limit of 1000 rules.",
	},
	{
		"V4 256-character ID",
		doc(rule("<ID>" + strings.Repeat("é", 256) + "</ID>" + logsFilter + enabled + expire3)),
		ia, "ID length should not exceed allowed limit of 255",
	},
	{
		"V5 duplicate ID", doc(validRule("a"), validRule("b"), validRule("a")), ia,
		"Rule ID must be unique. Found same ID for more than one rule",
	},
	{"V6 no Status", doc(rule(logsFilter + expire3)), mx, ""},
	{
		"V6 lower-case Status",
		doc(rule(logsFilter + "<Status>enabled</Status>" + expire3)), mx, "",
	},
	{"V6 repeated Status", doc(rule(logsFilter + enabled + enabled + expire3)), mx, ""},
	{"V7 neither Filter nor Prefix", doc(rule(enabled + expire3)), mx, ""},
	{
		"V7 both Filter and Prefix",
		doc(rule(logsFilter + "<Prefix>logs/</Prefix>" + enabled + expire3)), mx, "",
	},
	{
		"V8 Prefix and Filter rules mixed",
		doc(validRule("a"), rule("<Prefix>x/</Prefix>"+enabled+expire3)), ir,
		"Filter element can only be used in Lifecycle V2.",
	},
	{"V9 two predicates", filterRule("<Prefix>a</Prefix>"+tagFilter, expire3), mx, ""},
	{"V9 repeated Prefix", filterRule("<Prefix>a</Prefix><Prefix>b</Prefix>", expire3), mx, ""},
	{"V9 two tags outside And", filterRule(tagFilter+tagFilter, expire3), mx, ""},
	{"V9 two Filters", doc(rule(logsFilter + logsFilter + enabled + expire3)), mx, ""},
	{
		"V9 repeated top-level Prefix",
		doc(rule("<Prefix>a</Prefix><Prefix>b</Prefix>" + enabled + expire3)), mx, "",
	},
	{"V10 And with one predicate", filterRule("<And><Prefix>a</Prefix></And>", expire3), mx, ""},
	{
		"V10 And with two Prefixes",
		filterRule("<And><Prefix>a</Prefix><Prefix>b</Prefix></And>", expire3), mx, "",
	},
	{"V10 And with a repeated size", filterRule("<And><ObjectSizeLessThan>5</ObjectSizeLessThan>"+
		"<ObjectSizeLessThan>6</ObjectSizeLessThan></And>", expire3), mx, ""},
	{
		"V11 duplicate tag keys",
		filterRule("<And><Tag><Key>k</Key><Value>1</Value></Tag><Tag><Key>k</Key><Value>2</Value>"+
			"</Tag></And>", expire3), ia, "Duplicate Tag Keys are not allowed.",
	},
	{
		"V12 reserved tag key", filterRule("<Tag><Key>aws:x</Key><Value>v</Value></Tag>", expire3),
		"InvalidTag", "Your TagKey cannot be prefixed with aws:",
	},
	{
		"V12 long tag value",
		filterRule("<Tag><Key>k</Key><Value>"+strings.Repeat("v", 257)+"</Value></Tag>", expire3),
		"InvalidTag", "The TagValue you have provided is too long, max 256",
	},
	{"V13 negative GreaterThan", filterRule("<ObjectSizeGreaterThan>-1</ObjectSizeGreaterThan>",
		expire3), ia, "ObjectSizeGreaterThan must be a non-negative integer"},
	{
		"V13 LessThan 0", filterRule("<ObjectSizeLessThan>0</ObjectSizeLessThan>", expire3), ia,
		"ObjectSizeLessThan must be a positive integer",
	},
	{
		"V13 GreaterThan equal to LessThan",
		filterRule("<And><ObjectSizeGreaterThan>10</ObjectSizeGreaterThan>"+
			"<ObjectSizeLessThan>10</ObjectSizeLessThan></And>", expire3), ia,
		"ObjectSizeGreaterThan must be less than ObjectSizeLessThan",
	},
	{
		"V13 GreaterThan over 50 TB", filterRule("<ObjectSizeGreaterThan>54975581388801"+
			"</ObjectSizeGreaterThan>", expire3), ia,
		"ObjectSizeGreaterThan and ObjectSizeLessThan must not exceed 50 TB (54975581388800 bytes)",
	},
	{
		"V13 LessThan over 50 TB in And", filterRule("<And><Prefix>a</Prefix>"+
			"<ObjectSizeLessThan>54975581388801</ObjectSizeLessThan></And>", expire3), ia,
		"ObjectSizeGreaterThan and ObjectSizeLessThan must not exceed 50 TB (54975581388800 bytes)",
	},
	{"V14 no action", withActions(""), mx, ""},
	{"V15 empty Expiration", withActions("<Expiration></Expiration>"), mx, ""},
	{"V15 Days and Date", withActions("<Expiration><Days>1</Days>" +
		"<Date>2030-01-01T00:00:00Z</Date></Expiration>"), mx, ""},
	{
		"V16 ExpiredObjectDeleteMarker with Days", withActions("<Expiration><Days>1</Days>" +
			"<ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>"), ia,
		"ExpiredObjectDeleteMarker cannot be specified with Days or Date in a Lifecycle " +
			"Expiration Policy.",
	},
	{
		"V17 Days 0", withActions("<Expiration><Days>0</Days></Expiration>"), ia,
		"'Days' for Expiration action must be a positive integer",
	},
	{
		"V18 Expiration Date not at midnight",
		withActions("<Expiration><Date>2030-01-01T10:00:00Z</Date></Expiration>"), ia,
		"'Date' must be at midnight GMT",
	},
	{
		"V18 Expiration Date at another zone's midnight",
		withActions("<Expiration><Date>2030-01-01T00:00:00+02:00</Date></Expiration>"), ia,
		"'Date' must be at midnight GMT",
	},
	{
		"V18 Transition Date not at midnight", withActions("<Transition>" +
			"<Date>2030-01-01T00:00:01Z</Date><StorageClass>GLACIER</StorageClass></Transition>"),
		ia, "'Date' must be at midnight GMT",
	},
	{"V19 ExpiredObjectDeleteMarker with a tag", filterRule(tagFilter,
		"<Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker>"+
			"</Expiration>"), ir, "ExpiredObjectDeleteMarker cannot be specified with Tags."},
	{
		"V19 AbortIncompleteMultipartUpload with And tags",
		filterRule("<And><Prefix>a</Prefix>"+tagFilter+"</And>", "<AbortIncompleteMultipartUpload>"+
			"<DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload>"),
		ir, "AbortIncompleteMultipartUpload cannot be specified with Tags.",
	},
	{
		"V20 DaysAfterInitiation missing",
		withActions("<AbortIncompleteMultipartUpload></AbortIncompleteMultipartUpload>"), ia,
		"'DaysAfterInitiation' for AbortIncompleteMultipartUpload action must be a " +
			"positive integer",
	},
	{
		"V20 DaysAfterInitiation 0", withActions("<AbortIncompleteMultipartUpload>" +
			"<DaysAfterInitiation>0</DaysAfterInitiation></AbortIncompleteMultipartUpload>"), ia,
		"'DaysAfterInitiation' for AbortIncompleteMultipartUpload action must be a " +
			"positive integer",
	},
	{
		"V21 NoncurrentVersionExpiration without NoncurrentDays",
		withActions("<NoncurrentVersionExpiration><NewerNoncurrentVersions>1" +
			"</NewerNoncurrentVersions></NoncurrentVersionExpiration>"), mx, "",
	},
	{
		"V22 NoncurrentDays 0", withActions("<NoncurrentVersionExpiration>" +
			"<NoncurrentDays>0</NoncurrentDays></NoncurrentVersionExpiration>"), ia,
		"'NoncurrentDays' for NoncurrentVersionExpiration action must be a positive integer",
	},
	{
		"V23 NewerNoncurrentVersions 0", withActions("<NoncurrentVersionExpiration>" +
			"<NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>0" +
			"</NewerNoncurrentVersions></NoncurrentVersionExpiration>"), ia,
		"NewerNoncurrentVersions for NoncurrentVersionExpiration action must be a " +
			"positive integer",
	},
	{
		"V23 NewerNoncurrentVersions 101", withActions("<NoncurrentVersionExpiration>" +
			"<NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>101" +
			"</NewerNoncurrentVersions></NoncurrentVersionExpiration>"), ia,
		"NewerNoncurrentVersions for NoncurrentVersionExpiration action must not exceed 100",
	},
	{
		"V23 transition NewerNoncurrentVersions 101", withActions(
			"<NoncurrentVersionTransition><NoncurrentDays>1</NoncurrentDays>" +
				"<NewerNoncurrentVersions>101</NewerNoncurrentVersions>" +
				"<StorageClass>GLACIER</StorageClass></NoncurrentVersionTransition>"), ia,
		"NewerNoncurrentVersions for NoncurrentVersionTransition action must not exceed 100",
	},
	{
		"V24 NewerNoncurrentVersions without a Filter", doc(rule("<Prefix>a</Prefix>" +
			enabled + "<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays>" +
			"<NewerNoncurrentVersions>2</NewerNoncurrentVersions>" +
			"</NoncurrentVersionExpiration>")), ir,
		"NewerNoncurrentVersions element can only be used with a Filter element.",
	},
	{
		"V24 transition NewerNoncurrentVersions without a Filter", doc(rule(
			"<Prefix>a</Prefix>" + enabled + "<NoncurrentVersionTransition>" +
				"<NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>2" +
				"</NewerNoncurrentVersions><StorageClass>GLACIER</StorageClass>" +
				"</NoncurrentVersionTransition>")), ir,
		"NewerNoncurrentVersions element can only be used with a Filter element.",
	},
	{
		"V25 Transition without StorageClass",
		withActions("<Transition><Days>1</Days></Transition>"), mx, "",
	},
	{"V25 Transition to STANDARD", withActions("<Transition><Days>1</Days>" +
		"<StorageClass>STANDARD</StorageClass></Transition>"), mx, ""},
	{
		"V25 Transition without Days or Date",
		withActions("<Transition><StorageClass>GLACIER</StorageClass></Transition>"), mx, "",
	},
	{"V25 Transition with Days and Date", withActions("<Transition><Days>1</Days>" +
		"<Date>2030-01-01T00:00:00Z</Date><StorageClass>GLACIER</StorageClass>" +
		"</Transition>"), mx, ""},
	{
		"V25 NoncurrentVersionTransition to an unknown class",
		withActions("<NoncurrentVersionTransition><NoncurrentDays>1</NoncurrentDays>" +
			"<StorageClass>COLD</StorageClass></NoncurrentVersionTransition>"), mx, "",
	},
	{
		"V26 Transition Days -1", withActions("<Transition><Days>-1</Days>" +
			"<StorageClass>GLACIER</StorageClass></Transition>"), ia,
		"'Days' in Transition action must be greater than or equal to zero",
	},
	{
		"V26 NoncurrentVersionTransition without NoncurrentDays",
		withActions("<NoncurrentVersionTransition><StorageClass>GLACIER</StorageClass>" +
			"</NoncurrentVersionTransition>"), ia,
		"'NoncurrentDays' in NoncurrentVersionTransition action must be greater than or " +
			"equal to zero",
	},
	{
		"V26 NoncurrentDays -1 in NoncurrentVersionTransition",
		withActions("<NoncurrentVersionTransition><NoncurrentDays>-1</NoncurrentDays>" +
			"<StorageClass>GLACIER</StorageClass></NoncurrentVersionTransition>"), ia,
		"'NoncurrentDays' in NoncurrentVersionTransition action must be greater than or " +
			"equal to zero",
	},
	{
		"V29 Transition Days with Expiration Date", withActions("<Transition><Days>30</Days>" +
			"<StorageClass>GLACIER</StorageClass></Transition>" +
			"<Expiration><Date>2030-01-01</Date></Expiration>"), ia,
		"Days and Date cannot both be used in the Expiration and Transition actions of one rule",
	},
	{
		"V29 Transition Date with Expiration Days", withActions("<Transition>" +
			"<Date>2030-01-01</Date><StorageClass>GLACIER</StorageClass></Transition>" + expire3), ia,
		"Days and Date cannot both be used in the Expiration and Transition actions of one rule",
	},
	{
		"V29 two Transitions mixing them", withActions("<Transition><Days>30</Days>" +
			"<StorageClass>STANDARD_IA</StorageClass></Transition><Transition>" +
			"<Date>2030-01-01</Date><StorageClass>GLACIER</StorageClass></Transition>"), ia,
		"Days and Date cannot both be used in the Expiration and Transition actions of one rule",
	},
	{
		"V28 document over MaxDocument",
		doc(validRule("r")) + strings.Repeat(" ", MaxDocument),
		"MaxMessageLengthExceeded", "Your request was too big.",
	},
	{
		"L30 the first rule's error wins", doc(
			rule("<ID>a</ID>"+logsFilter+enabled+"<Expiration><Days>0</Days></Expiration>"),
			rule("<ID>"+strings.Repeat("x", 256)+"</ID>"+logsFilter+enabled+expire3)), ia,
		"'Days' for Expiration action must be a positive integer",
	},
	{
		"L30 a rule's ID is checked before its actions", doc(
			rule("<ID>" + strings.Repeat("x", 256) + "</ID>" + logsFilter + enabled)), ia,
		"ID length should not exceed allowed limit of 255",
	},
	{
		"L30 rule errors come before cross-rule errors",
		doc(validRule("a"), validRule("a"), rule(logsFilter+enabled)), mx, "",
	},
}

func TestParseRefuses(t *testing.T) {
	for _, tc := range refuseCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			requireError(t, err, http.StatusBadRequest, tc.code, tc.message)
		})
	}
}

// acceptCases are valid documents of every shape.
var acceptCases = []struct {
	name string
	body string
}{
	{"an empty Filter", filterRule("", expire3)},
	{"an empty Prefix", filterRule("<Prefix></Prefix>", expire3)},
	{"a top-level Prefix", doc(rule("<Prefix>logs/</Prefix>" + enabled + expire3))},
	{"an empty top-level Prefix", doc(rule("<Prefix></Prefix>" + enabled + expire3))},
	{"And with tags and sizes", filterRule("<And><Prefix>a/</Prefix><Tag><Key>k</Key><Value>v"+
		"</Value></Tag><Tag><Key>e</Key></Tag><ObjectSizeGreaterThan>0"+
		"</ObjectSizeGreaterThan><ObjectSizeLessThan>10</ObjectSizeLessThan></And>", expire3)},
	{"And with two tags only", filterRule("<And><Tag><Key>a</Key><Value>1</Value></Tag>"+
		"<Tag><Key>b</Key><Value>2</Value></Tag></And>", expire3)},
	{"a tag without a value", filterRule("<Tag><Key>k</Key></Tag>", expire3)},
	{"ExpiredObjectDeleteMarker false", filterRule("", "<Expiration><ExpiredObjectDeleteMarker>"+
		"false</ExpiredObjectDeleteMarker></Expiration>")},
	{"a Date without a time", filterRule("", "<Expiration><Date>2030-01-01</Date></Expiration>")},
	{"Transition Days 0", filterRule("", "<Transition><Days>0</Days><StorageClass>GLACIER"+
		"</StorageClass></Transition>")},
	{"Transition by Date", filterRule("", "<Transition><Date>2030-01-01T00:00:00.000Z</Date>"+
		"<StorageClass>DEEP_ARCHIVE</StorageClass></Transition>")},
	{"NewerNoncurrentVersions 100", filterRule("", "<NoncurrentVersionExpiration><NoncurrentDays>1"+
		"</NoncurrentDays><NewerNoncurrentVersions>100</NewerNoncurrentVersions>"+
		"</NoncurrentVersionExpiration>")},
	{
		"noncurrent actions without NewerNoncurrentVersions and a top-level Prefix",
		doc(rule("<Prefix>a</Prefix>" + enabled + "<NoncurrentVersionExpiration>" +
			"<NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>")),
	},
	{"a Disabled rule", doc(rule(logsFilter + "<Status>Disabled</Status>" + expire3))},
	{"unknown elements", filterRule("<Prefix>a</Prefix><Unknown>1</Unknown>", expire3+"<Extra/>")},
	{"sizes at 50 TB", filterRule("<And><ObjectSizeGreaterThan>1</ObjectSizeGreaterThan>"+
		"<ObjectSizeLessThan>54975581388800</ObjectSizeLessThan></And>", expire3)},
	{"Days in every action", filterRule("", "<Transition><Days>30</Days><StorageClass>"+
		"STANDARD_IA</StorageClass></Transition><Transition><Days>90</Days><StorageClass>"+
		"GLACIER</StorageClass></Transition><Expiration><Days>365</Days></Expiration>")},
	{"Date in every action", filterRule("", "<Transition><Date>2030-01-01</Date>"+
		"<StorageClass>GLACIER</StorageClass></Transition>"+
		"<Expiration><Date>2031-01-01</Date></Expiration>")},
	{"Transition Days beside ExpiredObjectDeleteMarker and noncurrent days", filterRule("",
		"<Transition><Days>30</Days><StorageClass>GLACIER</StorageClass></Transition>"+
			"<Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker>"+
			"</Expiration><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays>"+
			"</NoncurrentVersionExpiration>")},
	{"surrounding spaces in numbers", filterRule("<ObjectSizeLessThan> 10 </ObjectSizeLessThan>",
		"<Expiration><Days>\n 3 \n</Days></Expiration>")},
	{"1000 rules", manyRules(MaxRules)},
	{
		"a 255-character ID",
		doc(rule("<ID>" + strings.Repeat("é", 255) + "</ID>" + logsFilter + enabled + expire3)),
	},
	// The API reference's Example 4, as it is given: its root is
	// LifeCycleConfiguration and one NoncurrentDays is 0.
	{"PutBucketLifecycleConfiguration Example 4", `<LifeCycleConfiguration>
  <Rule>
    <ID>DeleteAfterBecomingNonCurrent</ID>
    <Filter>
       <Prefix>logs/</Prefix>
    </Filter>
    <Status>Enabled</Status>
    <NoncurrentVersionExpiration>
      <NoncurrentDays>1</NoncurrentDays>
    </NoncurrentVersionExpiration>
  </Rule>
  <Rule>
    <ID>TransitionSoonAfterBecomingNonCurrent</ID>
    <Filter>
       <Prefix>documents/</Prefix>
    </Filter>
    <Status>Enabled</Status>
    <NoncurrentVersionTransition>
      <NoncurrentDays>0</NoncurrentDays>
      <StorageClass>GLACIER</StorageClass>
    </NoncurrentVersionTransition>
  </Rule>
</LifeCycleConfiguration>`},
}

func TestParseAccepts(t *testing.T) {
	for _, tc := range acceptCases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if c.Xmlns != xmlns || len(c.Rules) == 0 {
				t.Errorf("configuration %+v", c)
			}
		})
	}
}

func TestParseGeneratesIDs(t *testing.T) {
	c, err := Parse([]byte(doc(
		rule(logsFilter+enabled+expire3),
		rule("<ID></ID>"+logsFilter+enabled+expire3),
		validRule("kept"),
		rule(logsFilter+enabled+expire3),
	)))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, r := range c.Rules {
		if r.ID == "" || seen[r.ID] {
			t.Errorf("rule %d: ID %q is empty or repeated", i, r.ID)
		}
		seen[r.ID] = true
	}
	if c.Rules[2].ID != "kept" {
		t.Errorf("the given ID became %q", c.Rules[2].ID)
	}
}

func TestParseTransitionDefault(t *testing.T) {
	for in, want := range map[string]string{
		"":                         TransitionDefault128K,
		"all_storage_classes_128K": TransitionDefault128K,
		"varies_by_storage_class":  TransitionDefaultVaries,
	} {
		got, err := ParseTransitionDefault(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"all_storage_classes_128k", "128K", " varies_by_storage_class"} {
		_, err := ParseTransitionDefault(in)
		requireError(t, err, http.StatusBadRequest, "InvalidArgument",
			"Invalid value for header x-amz-transition-default-minimum-object-size")
	}
}
