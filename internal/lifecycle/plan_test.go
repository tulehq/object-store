package lifecycle

import (
	"errors"
	"maps"
	"testing"
	"time"

	"tule.dev/object-store/internal/contracts"
)

var (
	created = at("2014-01-15T10:30:00Z") // eligible for 3 days at 2014-01-19
	before  = at("2014-01-18T23:59:59Z")
	due     = at("2014-01-19T00:00:00Z")
)

func version(id string, lm time.Time) Version {
	return Version{VersionID: id, LastModified: lm, Size: 5}
}

func marker(id string, lm time.Time) Version {
	return Version{VersionID: id, LastModified: lm, Marker: true}
}

// noncurrent returns v as a noncurrent version since since.
func noncurrent(v Version, since time.Time) Version {
	v.NoncurrentSince = since
	return v
}

const expireRule = "<Rule><ID>exp</ID>" + logsFilter + enabled + expire3 + "</Rule>"

// expirationCases are the plans of Expiration Days and Date (rule "exp"
// is Days 3 on logs/) by versioning state and current version.
var expirationCases = []struct {
	name       string
	rule       string
	versioning contracts.Versioning
	now        time.Time
	markerTime time.Time
	rules      []string
	versions   []Version
}{
	{
		"Unversioned, not yet", "", contracts.Unversioned, before,
		time.Time{},
		[]string{expireRule},
		[]Version{version("", created)},
	},
	{
		"Unversioned, deleted with no marker", "exp", contracts.Unversioned, due,
		time.Time{},
		[]string{expireRule},
		[]Version{version("", created)},
	},
	{
		"Enabled, a marker at midnight of now", "exp", contracts.VersioningEnabled,
		due.Add(5 * time.Hour), due,
		[]string{expireRule},
		[]Version{version("v1", created)},
	},
	{
		"Enabled, a past Date covers a version of today", "date", contracts.VersioningEnabled,
		due.Add(2 * time.Hour), due.Add(time.Hour),
		[]string{rule("<ID>date</ID>" + logsFilter + enabled +
			"<Expiration><Date>2010-01-01</Date></Expiration>")},
		[]Version{version("v1", due.Add(time.Hour))},
	},
	{
		"Enabled, a future Date", "", contracts.VersioningEnabled, due,
		time.Time{},
		[]string{rule("<ID>date</ID>" + logsFilter + enabled +
			"<Expiration><Date>2030-01-01</Date></Expiration>")},
		[]Version{version("v1", created)},
	},
	{
		"Suspended, a null current", "exp", contracts.VersioningSuspended, due, due,
		[]string{expireRule},
		[]Version{version("null", created)},
	},
	{
		"Suspended, a non-null current over a noncurrent null", "exp",
		contracts.VersioningSuspended, due, due,
		[]string{expireRule},
		[]Version{version("v2", created), noncurrent(version("null", created), created)},
	},
	{
		"Enabled, a legacy null current", "exp", contracts.VersioningEnabled, due, due,
		[]string{expireRule},
		[]Version{version("null", created)},
	},
	{
		"a current marker over versions is left alone", "", contracts.VersioningEnabled, due,
		time.Time{},
		[]string{expireRule},
		[]Version{marker("m", created), noncurrent(version("v1", created), created)},
	},
	{
		"a key the rule does not admit", "", contracts.VersioningEnabled, due,
		time.Time{},
		[]string{rule("<ID>x</ID><Filter><Prefix>other/</Prefix></Filter>" + enabled + expire3)},
		[]Version{version("v1", created)},
	},
	{
		"a Disabled rule", "", contracts.VersioningEnabled, due,
		time.Time{},
		[]string{rule("<ID>x</ID>" + logsFilter + "<Status>Disabled</Status>" + expire3)},
		[]Version{version("v1", created)},
	},
	{
		"transitions are never applied", "", contracts.VersioningEnabled, due,
		time.Time{},
		[]string{rule("<ID>x</ID>" + logsFilter + enabled + "<Transition><Days>0</Days>" +
			"<StorageClass>GLACIER</StorageClass></Transition>")},
		[]Version{version("v1", created)},
	},
	{
		"the earliest rule names the expiry", "exp", contracts.VersioningEnabled,
		due.AddDate(0, 0, 30), due.AddDate(0, 0, 30),
		[]string{rule("<ID>long</ID>" + logsFilter + enabled +
			"<Expiration><Days>20</Days></Expiration>"), expireRule},
		[]Version{version("v1", created)},
	},
}

func TestPlanExpiration(t *testing.T) {
	for _, tc := range expirationCases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustParse(t, tc.rules...)
			plan, err := c.Decider(tc.now).Plan(&Key{
				Key: "logs/a", Versioning: tc.versioning, Versions: tc.versions,
			})
			if err != nil {
				t.Fatal(err)
			}
			if plan.ExpireCurrentRule != tc.rule || !plan.MarkerTime.Equal(tc.markerTime) ||
				plan.Remove != nil {
				t.Errorf("got %+v, want rule %q, marker time %v", plan, tc.rule, tc.markerTime)
			}
		})
	}
}

// nve is a NoncurrentVersionExpiration rule on logs/; newer "" leaves
// NewerNoncurrentVersions out.
func nve(id, days, newer string) string {
	inner := "<NoncurrentDays>" + days + "</NoncurrentDays>"
	if newer != "" {
		inner += "<NewerNoncurrentVersions>" + newer + "</NewerNoncurrentVersions>"
	}
	return rule("<ID>" + id + "</ID>" + logsFilter + enabled +
		"<NoncurrentVersionExpiration>" + inner + "</NoncurrentVersionExpiration>")
}

// five is five noncurrent versions under a current one, each noncurrent
// since created; with NoncurrentDays 3 they are eligible at due.
var five = func() []Version {
	vs := make([]Version, 0, 6)
	vs = append(vs, version("cur", created))
	for _, id := range []string{"n1", "n2", "n3", "n4", "n5"} {
		vs = append(vs, noncurrent(version(id, created), created))
	}
	return vs
}()

// noncurrentCases are the removals of NoncurrentVersionExpiration.
var noncurrentCases = []struct {
	name       string
	versioning contracts.Versioning
	now        time.Time
	want       map[string]string
	rules      []string
	versions   []Version
}{
	{
		"NoncurrentDays, not yet", contracts.VersioningEnabled, before, nil,
		[]string{nve("n", "3", "")},
		five,
	},
	{
		"NoncurrentDays", contracts.VersioningEnabled, due,
		map[string]string{"n1": "n", "n2": "n", "n3": "n", "n4": "n", "n5": "n"},
		[]string{nve("n", "3", "")},
		five,
	},
	{
		"NewerNoncurrentVersions 2 keeps the two newest", contracts.VersioningEnabled, due,
		map[string]string{"n3": "n", "n4": "n", "n5": "n"},
		[]string{nve("n", "3", "2")},
		five,
	},
	{
		"NewerNoncurrentVersions 2, not old enough", contracts.VersioningEnabled, before, nil,
		[]string{nve("n", "3", "2")},
		five,
	},
	{
		"NewerNoncurrentVersions 5 keeps all five", contracts.VersioningEnabled, due, nil,
		[]string{nve("n", "3", "5")},
		five,
	},
	{
		"two overlapping rules: the earliest", contracts.VersioningEnabled, due.AddDate(0, 0, 10),
		map[string]string{"n1": "long", "n2": "long", "n3": "long", "n4": "long", "n5": "short"},
		[]string{nve("long", "10", ""), nve("short", "3", "4")},
		five,
	},
	{
		"a tie goes to the first", contracts.VersioningEnabled, due,
		map[string]string{"n1": "a"},
		[]string{nve("a", "3", ""), nve("b", "3", "")},
		five[:2],
	},
	{
		"Suspended", contracts.VersioningSuspended, due,
		map[string]string{"n1": "n"},
		[]string{nve("n", "3", "")},
		five[:2],
	},
	{
		"no effect Unversioned", contracts.Unversioned, due, nil,
		[]string{nve("n", "3", "")},
		five[:2],
	},
	{
		"markers count and expire", contracts.VersioningEnabled, due,
		map[string]string{"v1": "n"},
		[]string{nve("n", "3", "1")},
		[]Version{
			version("cur", created),
			noncurrent(marker("m1", created), created),
			noncurrent(version("v1", created), created),
		},
	},
	{
		"a noncurrent marker expires", contracts.VersioningEnabled, due,
		map[string]string{"m1": "n"},
		[]string{nve("n", "3", "")},
		[]Version{version("cur", created), noncurrent(marker("m1", created), created)},
	},
	{
		"markers have size 0", contracts.VersioningEnabled, due,
		map[string]string{"m1": "empty"},
		[]string{rule("<ID>empty</ID><Filter><ObjectSizeLessThan>1</ObjectSizeLessThan>" +
			"</Filter>" + enabled + "<NoncurrentVersionExpiration><NoncurrentDays>3" +
			"</NoncurrentDays></NoncurrentVersionExpiration>")},
		[]Version{
			version("cur", created), noncurrent(marker("m1", created), created),
			noncurrent(version("v1", created), created),
		},
	},
	{
		"since is the noncurrent time, not LastModified", contracts.VersioningEnabled, due, nil,
		[]string{nve("n", "3", "")},
		[]Version{version("cur", due), noncurrent(version("v1", created), due)},
	},
}

func TestPlanNoncurrent(t *testing.T) {
	for _, tc := range noncurrentCases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := mustParse(t, tc.rules...).Decider(tc.now).Plan(&Key{
				Key: "logs/a", Versioning: tc.versioning, Versions: tc.versions,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(plan.Remove, tc.want) || plan.ExpireCurrentRule != "" {
				t.Errorf("got %+v, want %v", plan, tc.want)
			}
		})
	}
}

// TestPlanUsesTheStateAsRead checks that noncurrent expiry is decided
// before this plan's Expiration: the version Expiration makes noncurrent
// is not also removed.
func TestPlanUsesTheStateAsRead(t *testing.T) {
	c := mustParse(t, rule("<ID>both</ID>"+logsFilter+enabled+expire3+
		"<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays>"+
		"</NoncurrentVersionExpiration>"))
	plan, err := c.Decider(due).Plan(&Key{
		Key: "logs/a", Versioning: contracts.VersioningEnabled,
		Versions: []Version{version("v1", created)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ExpireCurrentRule != "both" || plan.Remove != nil {
		t.Errorf("plan %+v", plan)
	}
}

// TestPlanBothActions checks a Suspended plan that both removes a
// noncurrent null and expires the current version, the plan whose Remove
// storage must apply first.
func TestPlanBothActions(t *testing.T) {
	c := mustParse(t, rule("<ID>both</ID>"+logsFilter+enabled+expire3+
		"<NoncurrentVersionExpiration><NoncurrentDays>3</NoncurrentDays>"+
		"</NoncurrentVersionExpiration>"))
	plan, err := c.Decider(due).Plan(&Key{
		Key: "logs/a", Versioning: contracts.VersioningSuspended,
		Versions: []Version{version("v2", created), noncurrent(version("null", created), created)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ExpireCurrentRule != "both" || !plan.MarkerTime.Equal(due) ||
		!maps.Equal(plan.Remove, map[string]string{"null": "both"}) {
		t.Errorf("plan %+v", plan)
	}
}

func TestPlanReadsTagsOnlyWhenNeeded(t *testing.T) {
	calls := 0
	tagged := version("v1", created)
	tagged.Tags = func() ([]contracts.Tag, error) {
		calls++
		return []contracts.Tag{{Key: "team", Value: "a"}}, nil
	}
	k := &Key{Key: "logs/a", Versioning: contracts.VersioningEnabled, Versions: []Version{tagged}}

	if _, err := mustParse(t, validRule("plain")).Decider(due).Plan(k); err != nil || calls != 0 {
		t.Fatalf("a rule without tags read them %d times: %v", calls, err)
	}
	tagRule := func(id, value string) string {
		return rule("<ID>" + id + "</ID><Filter><Tag><Key>team</Key><Value>" + value +
			"</Value></Tag></Filter>" + enabled + expire3)
	}
	plan, err := mustParse(t, tagRule("b", "b"), tagRule("a", "a")).Decider(due).Plan(k)
	if err != nil || plan.ExpireCurrentRule != "a" || calls != 1 {
		t.Errorf("plan %+v, %v, tags read %d times, want once", plan, err, calls)
	}

	failing := version("v1", created)
	errTags := errors.New("tags unreadable")
	failing.Tags = func() ([]contracts.Tag, error) { return nil, errTags }
	_, err = mustParse(t, tagRule("a", "a")).Decider(due).Plan(&Key{
		Key: "logs/a", Versioning: contracts.VersioningEnabled, Versions: []Version{failing},
	})
	if !errors.Is(err, errTags) {
		t.Errorf("got %v, want the tag error", err)
	}

	untagged := version("v1", created)
	plan, err = mustParse(t, tagRule("a", "a")).Decider(due).Plan(&Key{
		Key: "logs/a", Versioning: contracts.VersioningEnabled, Versions: []Version{untagged},
	})
	if err != nil || plan.ExpireCurrentRule != "" {
		t.Errorf("a version without tags: %+v, %v", plan, err)
	}
}

func TestPlanRefusesANoncurrentVersionWithoutItsTime(t *testing.T) {
	c := mustParse(t, rule("<ID>n</ID>"+logsFilter+enabled+"<NoncurrentVersionExpiration>"+
		"<NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>"))
	_, err := c.Decider(due).Plan(&Key{
		Key: "logs/a", Versioning: contracts.VersioningEnabled,
		Versions: []Version{version("v2", created), version("v1", created)},
	})
	if !errors.Is(err, errNoNoncurrentTime) {
		t.Errorf("got %v", err)
	}
	plan, err := c.Decider(due).Plan(&Key{Key: "logs/a", Versioning: contracts.VersioningEnabled})
	if err != nil || plan.ExpireCurrentRule != "" || plan.Remove != nil {
		t.Errorf("a key without versions: %+v, %v", plan, err)
	}
}

func TestRemoveSoleMarker(t *testing.T) {
	eodm := func(id, filter, value string) string {
		return rule("<ID>" + id + "</ID><Filter>" + filter + "</Filter>" + enabled +
			"<Expiration><ExpiredObjectDeleteMarker>" + value +
			"</ExpiredObjectDeleteMarker></Expiration>")
	}
	m := marker("m", created)
	for _, tc := range []struct {
		name  string
		now   time.Time
		want  string
		rules []string
	}{
		{"ExpiredObjectDeleteMarker at once", created, "e", []string{eodm("e", "", "true")}},
		{"ExpiredObjectDeleteMarker false", due, "", []string{eodm("e", "", "false")}},
		{"ExpiredObjectDeleteMarker under another prefix", due, "", []string{eodm("e", "<Prefix>x/</Prefix>", "true")}},
		{"Days, a young marker", before, "", []string{validRule("d")}},
		{"Days, an old marker", due, "d", []string{validRule("d")}},
		{"a past Date", created, "date", []string{rule("<ID>date</ID>" + logsFilter + enabled +
			"<Expiration><Date>2010-01-01</Date></Expiration>")}},
		{"a tag rule never admits a marker", due, "", []string{rule("<ID>t</ID><Filter><Tag><Key>k" +
			"</Key><Value></Value></Tag></Filter>" + enabled + expire3)}},
		{"ObjectSizeGreaterThan 0 never admits a marker", due, "", []string{eodm("e",
			"<ObjectSizeGreaterThan>0</ObjectSizeGreaterThan>", "true")}},
		{"ObjectSizeLessThan 1 admits a marker", due, "e", []string{eodm("e",
			"<ObjectSizeLessThan>1</ObjectSizeLessThan>", "true")}},
		{"a Disabled rule", due, "", []string{rule("<ID>e</ID><Filter></Filter>" +
			"<Status>Disabled</Status><Expiration><ExpiredObjectDeleteMarker>true" +
			"</ExpiredObjectDeleteMarker></Expiration>")}},
		{"ExpiredObjectDeleteMarker wins over Days", due, "e", []string{
			validRule("d"),
			eodm("e", "", "true"),
		}},
		{"noncurrent rules do not remove it", due, "", []string{rule("<ID>n</ID>" + logsFilter + enabled +
			"<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays>" +
			"</NoncurrentVersionExpiration>")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mustParse(t, tc.rules...).Decider(tc.now).RemoveSoleMarker("logs/a", m)
			if err != nil || got != tc.want {
				t.Errorf("got %q, %v, want %q", got, err, tc.want)
			}
		})
	}
	_, err := mustParse(t, eodm("e", "", "true")).Decider(due).
		RemoveSoleMarker("logs/a", version("v1", created))
	if err == nil {
		t.Error("a version that is not a marker was accepted")
	}
}
