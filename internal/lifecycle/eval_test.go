package lifecycle

import (
	"testing"
	"time"

	"tule.dev/object-store/internal/contracts"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func mustParse(t *testing.T, rules ...string) *Configuration {
	t.Helper()
	c, err := Parse([]byte(doc(rules...)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEligibleAt(t *testing.T) {
	for _, tc := range []struct {
		name string
		t    string
		want string
		days int
	}{
		// S3 User Guide, "Lifecycle configuration elements".
		{"created 1/15/2014 10:30 with 3 days", "2014-01-15T10:30:00Z", "2014-01-19T00:00:00Z", 3},
		{"noncurrent 1/2/2014 11:30 with 5 days", "2014-01-02T11:30:00Z", "2014-01-08T00:00:00Z", 5},
		{"exactly midnight rounds to the next", "2014-01-15T00:00:00Z", "2014-01-19T00:00:00Z", 3},
		{
			"a nanosecond before midnight", "2014-01-14T23:59:59.999999999Z",
			"2014-01-18T00:00:00Z", 3,
		},
		{"days 0", "2014-01-15T10:30:00Z", "2014-01-16T00:00:00Z", 0},
		{"days 0 at midnight", "2014-01-15T00:00:00Z", "2014-01-16T00:00:00Z", 0},
		{"onto a leap day", "2024-02-28T10:00:00Z", "2024-03-01T00:00:00Z", 1},
		{"over a leap day", "2024-02-28T10:00:00Z", "2024-03-02T00:00:00Z", 2},
		{"from a leap day", "2024-02-29T23:00:00Z", "2025-03-01T00:00:00Z", 365},
		{"no leap day in 2023", "2023-02-28T10:00:00Z", "2023-03-02T00:00:00Z", 1},
		{"over year end", "2014-12-31T10:00:00Z", "2015-01-02T00:00:00Z", 1},
		{"onto year end", "2014-12-30T10:00:00Z", "2015-01-01T00:00:00Z", 1},
		{"another zone is read in UTC", "2014-01-15T01:00:00+02:00", "2014-01-18T00:00:00Z", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EligibleAt(at(tc.t), tc.days)
			if !got.Equal(at(tc.want)) || got.Location() != time.UTC {
				t.Errorf("got %v, want %s", got, tc.want)
			}
		})
	}
	if got := Midnight(at("2014-01-15T01:00:00+02:00")); !got.Equal(at("2014-01-14T00:00:00Z")) {
		t.Errorf("Midnight %v", got)
	}
}

func TestExpiryMatching(t *testing.T) {
	lm := at("2014-01-15T10:30:00Z")
	tags := []contracts.Tag{{Key: "team", Value: "a"}, {Key: "empty", Value: ""}}
	exp := func(id, filter string) string {
		return rule("<ID>" + id + "</ID><Filter>" + filter + "</Filter>" + enabled + expire3)
	}
	for _, tc := range []struct {
		name   string
		filter string
		key    string
		tags   []contracts.Tag
		size   int64
		match  bool
	}{
		{"prefix", "<Prefix>logs/</Prefix>", "logs/a", nil, 1, true},
		{"another prefix", "<Prefix>logs/</Prefix>", "log/a", nil, 1, false},
		{"empty filter", "", "anything", nil, 0, true},
		{"empty prefix", "<Prefix></Prefix>", "anything", nil, 0, true},
		{"tag", "<Tag><Key>team</Key><Value>a</Value></Tag>", "k", tags, 1, true},
		{"tag of another value", "<Tag><Key>team</Key><Value>b</Value></Tag>", "k", tags, 1, false},
		{"tag absent", "<Tag><Key>team</Key><Value>a</Value></Tag>", "k", nil, 1, false},
		{"tag without a value, empty value", "<Tag><Key>empty</Key></Tag>", "k", tags, 1, true},
		{"tag without a value, set value", "<Tag><Key>team</Key></Tag>", "k", tags, 1, false},
		{"extra object tags", "<And><Prefix>k</Prefix><Tag><Key>team</Key><Value>a</Value>" +
			"</Tag></And>", "k", tags, 1, true},
		{"every And tag", "<And><Tag><Key>team</Key><Value>a</Value></Tag><Tag><Key>x</Key>" +
			"<Value>y</Value></Tag></And>", "k", tags, 1, false},
		{
			"GreaterThan at the bound", "<ObjectSizeGreaterThan>10</ObjectSizeGreaterThan>",
			"k", nil, 10, false,
		},
		{
			"GreaterThan above", "<ObjectSizeGreaterThan>10</ObjectSizeGreaterThan>",
			"k", nil, 11, true,
		},
		{"LessThan at the bound", "<ObjectSizeLessThan>10</ObjectSizeLessThan>", "k", nil, 10, false},
		{"LessThan below", "<ObjectSizeLessThan>10</ObjectSizeLessThan>", "k", nil, 9, true},
		{"range", "<And><ObjectSizeGreaterThan>1</ObjectSizeGreaterThan>" +
			"<ObjectSizeLessThan>3</ObjectSizeLessThan></And>", "k", nil, 2, true},
		{"range bound", "<And><ObjectSizeGreaterThan>1</ObjectSizeGreaterThan>" +
			"<ObjectSizeLessThan>3</ObjectSizeLessThan></And>", "k", nil, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mustParse(t, exp("r", tc.filter))
			when, id, ok := c.Expiry(
				Object{Key: tc.key, Size: tc.size, LastModified: lm, Tags: tc.tags},
			)
			if ok != tc.match {
				t.Fatalf("matched %v, want %v", ok, tc.match)
			}
			if ok && (id != "r" || !when.Equal(at("2014-01-19T00:00:00Z"))) {
				t.Errorf("got %v %q", when, id)
			}
		})
	}
}

func TestExpiryTopLevelPrefix(t *testing.T) {
	lm := at("2014-01-15T10:30:00Z")
	c := mustParse(t, rule("<Prefix>a</Prefix>"+enabled+expire3))
	if _, _, ok := c.Expiry(Object{Key: "ab", LastModified: lm}); !ok {
		t.Error("a top-level Prefix does not match")
	}
	if _, _, ok := c.Expiry(Object{Key: "b", LastModified: lm}); ok {
		t.Error("top-level Prefix a matches key b")
	}
	if c.MayMatch("b") {
		t.Error("top-level Prefix a may match key b")
	}
}

func TestExpiryChoosesTheEarliest(t *testing.T) {
	lm := at("2014-01-15T10:30:00Z")
	o := Object{Key: "logs/a", Size: 5, LastModified: lm}
	days := func(id, n string) string {
		return rule("<ID>" + id + "</ID>" + logsFilter + enabled + "<Expiration><Days>" + n +
			"</Days></Expiration>")
	}
	date := func(id, d string) string {
		return rule("<ID>" + id + "</ID>" + logsFilter + enabled + "<Expiration><Date>" + d +
			"</Date></Expiration>")
	}
	for _, tc := range []struct {
		name  string
		want  string
		id    string
		rules []string
	}{
		{
			"the shorter", "2014-01-19T00:00:00Z", "short",
			[]string{days("long", "10"), days("short", "3")},
		},
		{
			"a tie goes to the first", "2014-01-19T00:00:00Z", "a",
			[]string{days("a", "3"), days("b", "3")},
		},
		{
			"a Date in the past", "2010-01-01T00:00:00Z", "past",
			[]string{days("d", "3"), date("past", "2010-01-01")},
		},
		{
			"a Date in the future", "2030-01-01T00:00:00Z", "future",
			[]string{date("future", "2030-01-01")},
		},
		{"a Disabled rule is ignored", "2014-01-19T00:00:00Z", "on", []string{
			rule("<ID>off</ID>" + logsFilter + "<Status>Disabled</Status>" +
				"<Expiration><Days>1</Days></Expiration>"),
			days("on", "3"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			when, id, ok := mustParse(t, tc.rules...).Expiry(o)
			if !ok || id != tc.id || !when.Equal(at(tc.want)) {
				t.Errorf("got %v %q %v, want %s %q", when, id, ok, tc.want, tc.id)
			}
		})
	}
	for name, r := range map[string]string{
		"noncurrent only": rule(logsFilter + enabled + "<NoncurrentVersionExpiration>" +
			"<NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>"),
		"marker only": rule(logsFilter + enabled + "<Expiration><ExpiredObjectDeleteMarker>" +
			"true</ExpiredObjectDeleteMarker></Expiration>"),
		"disabled": rule(logsFilter + "<Status>Disabled</Status>" + expire3),
		"transition only": rule(logsFilter + enabled + "<Transition><Days>0</Days>" +
			"<StorageClass>GLACIER</StorageClass></Transition>"),
		"abort only": rule(logsFilter + enabled + "<AbortIncompleteMultipartUpload>" +
			"<DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload>"),
	} {
		if _, _, ok := mustParse(t, r).Expiry(o); ok {
			t.Errorf("%s: expires", name)
		}
	}
}

func TestExpirationHeader(t *testing.T) {
	for id, want := range map[string]string{
		"rule1": `expiry-date="Sun, 19 Jan 2014 00:00:00 GMT", rule-id="rule1"`,
		"my rule/é~-_.": `expiry-date="Sun, 19 Jan 2014 00:00:00 GMT", ` +
			`rule-id="my%20rule%2F%C3%A9~-_."`,
		`a"b+c`: `expiry-date="Sun, 19 Jan 2014 00:00:00 GMT", rule-id="a%22b%2Bc"`,
	} {
		if got := ExpirationHeader(at("2014-01-19T02:00:00+02:00"), id); got != want {
			t.Errorf("%q: got %s, want %s", id, got, want)
		}
	}
}

func TestAbortDate(t *testing.T) {
	initiated := at("2014-01-15T10:30:00Z")
	abort := func(id, filter, days string) string {
		return rule("<ID>" + id + "</ID><Filter>" + filter + "</Filter>" + enabled +
			"<AbortIncompleteMultipartUpload><DaysAfterInitiation>" + days +
			"</DaysAfterInitiation></AbortIncompleteMultipartUpload>")
	}
	c := mustParse(t,
		abort("long", "<Prefix>up/</Prefix>", "10"),
		abort("sized", "<And><Prefix>up/</Prefix><ObjectSizeGreaterThan>100"+
			"</ObjectSizeGreaterThan></And>", "3"),
		abort("other", "<Prefix>other/</Prefix>", "1"),
		rule("<ID>off</ID><Filter></Filter><Status>Disabled</Status>"+
			"<AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation>"+
			"</AbortIncompleteMultipartUpload>"),
		validRule("expire"),
	)
	when, id, ok := c.AbortDate("up/a", initiated)
	if !ok || id != "sized" || !when.Equal(at("2014-01-19T00:00:00Z")) {
		t.Errorf("up/a: got %v %q %v; the size bound must not apply to an upload", when, id, ok)
	}
	if _, _, ok = c.AbortDate("logs/a", initiated); ok {
		t.Error("logs/a: aborted by a rule without the action or by a Disabled rule")
	}
	if !c.HasUploadRules() {
		t.Error("HasUploadRules false")
	}
	if mustParse(t, validRule("e"), validRule("f")).HasUploadRules() {
		t.Error("HasUploadRules true without an abort rule")
	}
	disabled := mustParse(t, rule("<Filter></Filter><Status>Disabled</Status>"+
		"<AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation>"+
		"</AbortIncompleteMultipartUpload>"))
	if disabled.HasUploadRules() {
		t.Error("HasUploadRules true for a Disabled rule")
	}
}

func TestMayMatch(t *testing.T) {
	c := mustParse(t,
		validRule("logs"),
		rule("<Filter><And><Prefix>data/</Prefix><Tag><Key>k</Key><Value>v</Value></Tag>"+
			"</And></Filter>"+enabled+expire3),
		rule("<Filter><Prefix>off/</Prefix></Filter><Status>Disabled</Status>"+expire3),
	)
	for key, want := range map[string]bool{
		"logs/a": true, "data/x": true, "off/a": false, "other": false, "log": false,
	} {
		if got := c.MayMatch(key); got != want {
			t.Errorf("%s: got %v, want %v", key, got, want)
		}
	}
	if !mustParse(t, rule("<Filter></Filter>"+enabled+expire3)).MayMatch("anything") {
		t.Error("an empty filter does not admit every key")
	}
}
