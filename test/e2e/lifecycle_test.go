//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The lifecycle tests run the server on a fakeClock that starts at
// lifecycleEpoch, so every object they write was last modified then, and
// move it to the midnights the rules make objects eligible at.

// startLifecycle starts a server on a new fakeClock at lifecycleEpoch,
// with env over startLive's, and returns its client and the clock.
func startLifecycle(t *testing.T, env map[string]string) (*s3.Client, *fakeClock) {
	t.Helper()
	clk := newFakeClock(lifecycleEpoch)
	return clientFor(startLiveWith(t, env, liveOptions{clock: clk})), clk
}

// day is midnight UTC of January d, 2014, plus the clock time hms, given as
// hours, minutes and seconds.
func day(d int, hms ...int) time.Time {
	at := time.Date(2014, time.January, d, 0, 0, 0, 0, time.UTC)
	units := []time.Duration{time.Hour, time.Minute, time.Second}
	for i, n := range hms {
		at = at.Add(time.Duration(n) * units[i])
	}
	return at
}

// putLifecycle puts rules as bucket's lifecycle configuration.
func putLifecycle(t *testing.T, c *s3.Client, bucket string, rules ...types.LifecycleRule) {
	t.Helper()
	_, err := c.PutBucketLifecycleConfiguration(t.Context(),
		&s3.PutBucketLifecycleConfigurationInput{
			Bucket:                 aws.String(bucket),
			LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: rules},
		})
	requireNoErr(t, err, "put the lifecycle configuration of "+bucket)
}

// getLifecycle returns bucket's lifecycle configuration.
func getLifecycle(
	t *testing.T,
	c *s3.Client,
	bucket string,
) *s3.GetBucketLifecycleConfigurationOutput {
	t.Helper()
	out, err := c.GetBucketLifecycleConfiguration(t.Context(),
		&s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucket)})
	requireNoErr(t, err, "get the lifecycle configuration of "+bucket)
	return out
}

// enabled is an Enabled rule id with filter and the expiration exp.
func enabled(
	id string,
	filter *types.LifecycleRuleFilter,
	exp *types.LifecycleExpiration,
) types.LifecycleRule {
	return types.LifecycleRule{
		ID: aws.String(id), Status: types.ExpirationStatusEnabled, Filter: filter,
		Expiration: exp,
	}
}

func prefixFilter(p string) *types.LifecycleRuleFilter {
	return &types.LifecycleRuleFilter{Prefix: aws.String(p)}
}

func days(n int32) *types.LifecycleExpiration {
	return &types.LifecycleExpiration{Days: aws.Int32(n)}
}

// keysOf lists bucket's keys, at most 1,000.
func keysOf(t *testing.T, c *s3.Client, bucket string) []string {
	t.Helper()
	out, err := c.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	requireNoErr(t, err, "list "+bucket)
	keys := make([]string, 0, len(out.Contents))
	for _, o := range out.Contents {
		keys = append(keys, aws.ToString(o.Key))
	}
	return keys
}

// requireKeys checks that bucket holds exactly want, sorted.
func requireKeys(t *testing.T, c *s3.Client, bucket string, want ...string) {
	t.Helper()
	if got := keysOf(t, c, bucket); !slices.Equal(got, want) {
		t.Fatalf("%s holds %q, want %q", bucket, got, want)
	}
}

// eventuallyKeys waits until bucket holds exactly want.
func eventuallyKeys(t *testing.T, c *s3.Client, bucket string, want ...string) {
	t.Helper()
	eventually(t, bucket+" to hold "+strings.Join(want, ", "), func() bool {
		return slices.Equal(keysOf(t, c, bucket), want)
	})
}

// expiresOn is the x-amz-expiration value of an expiry at midnight UTC of
// January d, 2014, by the rule ruleID, URL-encoded.
func expiresOn(d int, ruleID string) string {
	return `expiry-date="` + day(d).Format(http.TimeFormat) + `", rule-id="` + ruleID + `"`
}

// requireExpiration checks that GetObject and HeadObject of bucket/key, of
// its version versionID when that is not "", answer x-amz-expiration want,
// none when want is "".
func requireExpiration(t *testing.T, c *s3.Client, bucket, key, versionID, want string) {
	t.Helper()
	var version *string
	if versionID != "" {
		version = aws.String(versionID)
	}
	out, _ := get(t, c, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: version,
	})
	requireEqual(t, aws.ToString(out.Expiration), want, "GetObject expiration of "+key)
	head, err := c.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: version,
	})
	requireNoErr(t, err, "head "+key)
	requireEqual(t, aws.ToString(head.Expiration), want, "HeadObject expiration of "+key)
}

// sameJSON checks that got and want encode to the same JSON.
func sameJSON(t *testing.T, got, want any, what string) {
	t.Helper()
	g, err := json.Marshal(got)
	requireNoErr(t, err, "encode "+what)
	w, err := json.Marshal(want)
	requireNoErr(t, err, "encode "+what)
	if !bytes.Equal(g, w) {
		t.Fatalf("%s:\n got %s\nwant %s", what, g, w)
	}
}

// everyElementRules are rules using every element of a configuration the
// SDK models; the last has no ID.
func everyElementRules() []types.LifecycleRule {
	return []types.LifecycleRule{
		enabled("prefix", prefixFilter("logs/"), days(3)),
		enabled("tag", &types.LifecycleRuleFilter{
			Tag: &types.Tag{Key: aws.String("team"), Value: aws.String("x")},
		}, &types.LifecycleExpiration{Date: aws.Time(day(20))}),
		{
			ID: aws.String("and"), Status: types.ExpirationStatusDisabled,
			Filter: &types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{
				Prefix: aws.String("data/"), ObjectSizeGreaterThan: aws.Int64(1),
				ObjectSizeLessThan: aws.Int64(1 << 20),
				Tags: []types.Tag{
					{Key: aws.String("a"), Value: aws.String("1")},
					{Key: aws.String("b"), Value: aws.String("2")},
				},
			}},
			Expiration: days(30),
			Transitions: []types.Transition{
				{Days: aws.Int32(0), StorageClass: types.TransitionStorageClassGlacierIr},
				{Days: aws.Int32(10), StorageClass: types.TransitionStorageClassStandardIa},
			},
		},
		enabled("size", &types.LifecycleRuleFilter{ObjectSizeGreaterThan: aws.Int64(100)},
			days(7)),
		{
			ID: aws.String("versions"), Status: types.ExpirationStatusEnabled,
			Filter: &types.LifecycleRuleFilter{},
			NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{
				NoncurrentDays: aws.Int32(5), NewerNoncurrentVersions: aws.Int32(2),
			},
			NoncurrentVersionTransitions: []types.NoncurrentVersionTransition{{
				NoncurrentDays: aws.Int32(1), NewerNoncurrentVersions: aws.Int32(1),
				StorageClass: types.TransitionStorageClassGlacier,
			}},
			AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{
				DaysAfterInitiation: aws.Int32(2),
			},
		},
		{
			Status: types.ExpirationStatusEnabled, Filter: prefixFilter("markers/"),
			Expiration: &types.LifecycleExpiration{ExpiredObjectDeleteMarker: aws.Bool(true)},
		},
	}
}

// The SDK's configuration of every element round-trips: each rule as it
// was put, a rule put without an ID with the one generated for it, and
// x-amz-transition-default-minimum-object-size echoed.
func TestBucketLifecycleConfigurationRoundTrip(t *testing.T) {
	c, _ := startLifecycle(t, nil)
	ctx := t.Context()
	createBucket(t, c, "lcy")
	bucket := aws.String("lcy")

	_, err := c.GetBucketLifecycleConfiguration(ctx,
		&s3.GetBucketLifecycleConfigurationInput{Bucket: bucket})
	requireAPIError(t, err, "NoSuchLifecycleConfiguration",
		"The lifecycle configuration does not exist")
	requireResponse(t, err, http.StatusNotFound)

	rules := everyElementRules()
	put, err := c.PutBucketLifecycleConfiguration(ctx,
		&s3.PutBucketLifecycleConfigurationInput{
			Bucket:                 bucket,
			LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: rules},
		})
	requireNoErr(t, err, "put")
	requireEqual(t, put.TransitionDefaultMinimumObjectSize,
		types.TransitionDefaultMinimumObjectSizeAllStorageClasses128k, "put's default size")

	got := getLifecycle(t, c, "lcy")
	requireEqual(t, got.TransitionDefaultMinimumObjectSize,
		types.TransitionDefaultMinimumObjectSizeAllStorageClasses128k, "get's default size")
	if len(got.Rules) != len(rules) {
		t.Fatalf("got %d rules, want %d", len(got.Rules), len(rules))
	}
	generated := aws.ToString(got.Rules[len(rules)-1].ID)
	if generated == "" {
		t.Fatal("the rule put without an ID has none")
	}
	rules[len(rules)-1].ID = aws.String(generated)
	sameJSON(t, got.Rules, rules, "the rules")
	requireEqual(t, aws.ToString(getLifecycle(t, c, "lcy").Rules[len(rules)-1].ID), generated,
		"the generated ID, read again")

	// A second Put replaces the first, and its header value is echoed.
	put, err = c.PutBucketLifecycleConfiguration(ctx,
		&s3.PutBucketLifecycleConfigurationInput{
			Bucket: bucket,
			LifecycleConfiguration: &types.BucketLifecycleConfiguration{
				Rules: rules[:1],
			},
			TransitionDefaultMinimumObjectSize: types.
				TransitionDefaultMinimumObjectSizeVariesByStorageClass,
		})
	requireNoErr(t, err, "put again")
	requireEqual(t, put.TransitionDefaultMinimumObjectSize,
		types.TransitionDefaultMinimumObjectSizeVariesByStorageClass, "put's echoed size")
	got = getLifecycle(t, c, "lcy")
	requireEqual(t, got.TransitionDefaultMinimumObjectSize,
		types.TransitionDefaultMinimumObjectSizeVariesByStorageClass, "get's stored size")
	sameJSON(t, got.Rules, rules[:1], "the replacing rules")
	requireLifecycleDeletes(t, c, rules[:1])
}

// requireLifecycleDeletes checks, on the bucket lcy, which has a
// configuration, that Delete answers 204, also with nothing to delete, and
// that each operation on a bucket that does not exist is NoSuchBucket; and
// that a configuration of the deprecated form, a top-level Prefix and no
// Filter, as PutBucketLifecycle sends it, is answered in that form. rules
// is a valid configuration.
func requireLifecycleDeletes(t *testing.T, c *s3.Client, rules []types.LifecycleRule) {
	t.Helper()
	ctx := t.Context()
	bucket := aws.String("lcy")
	for i := range 2 {
		_, err := c.DeleteBucketLifecycle(ctx, &s3.DeleteBucketLifecycleInput{Bucket: bucket})
		requireNoErr(t, err, "delete "+strconv.Itoa(i))
	}
	_, err := c.GetBucketLifecycleConfiguration(ctx,
		&s3.GetBucketLifecycleConfigurationInput{Bucket: bucket})
	requireCode(t, err, "NoSuchLifecycleConfiguration")
	requireResponse(t, err, http.StatusNotFound)

	missing := aws.String("no-such-bucket")
	_, err = c.GetBucketLifecycleConfiguration(ctx,
		&s3.GetBucketLifecycleConfigurationInput{Bucket: missing})
	requireCode(t, err, "NoSuchBucket")
	_, err = c.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket:                 missing,
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: rules},
	})
	requireCode(t, err, "NoSuchBucket")
	_, err = c.DeleteBucketLifecycle(ctx, &s3.DeleteBucketLifecycleInput{Bucket: missing})
	requireCode(t, err, "NoSuchBucket")

	createBucket(t, c, "lc-v1")
	v1 := []types.LifecycleRule{{
		ID: aws.String("v1"), Prefix: aws.String("logs/"), Status: types.ExpirationStatusEnabled,
		Expiration: days(1),
	}}
	putLifecycle(t, c, "lc-v1", v1...)
	sameJSON(t, getLifecycle(t, c, "lc-v1").Rules, v1, "the deprecated form")
}

// lifecycleRequest builds a lifecycle request of bucket with body, signed
// with the test credentials and the headers in header, which are signed
// too.
func lifecycleRequest(
	t *testing.T,
	l *liveServer,
	method, bucket, body string,
	header map[string]string,
) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, l.URL+"/"+bucket+"?lifecycle",
		strings.NewReader(body))
	requireNoErr(t, err, "build request")
	sum := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", hash)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	err = v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }).
		SignHTTP(t.Context(), testCreds, req, hash, "s3", "us-east-1", time.Now())
	requireNoErr(t, err, "sign request")
	return req
}

// lifecycleDoc is a configuration of rules, each the inside of a Rule.
func lifecycleDoc(rules ...string) string {
	var b strings.Builder
	b.WriteString(`<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	for _, r := range rules {
		b.WriteString("<Rule>" + r + "</Rule>")
	}
	b.WriteString("</LifecycleConfiguration>")
	return b.String()
}

// The parts of a valid rule, for the validation cases to vary.
const (
	vID     = "<ID>r</ID>"
	vFilter = "<Filter><Prefix>p/</Prefix></Filter>"
	vStatus = "<Status>Enabled</Status>"
	vExpire = "<Expiration><Days>1</Days></Expiration>"
	vRule   = vID + vFilter + vStatus + vExpire
)

// validationCase is a configuration refused with status, code and message.
type validationCase struct {
	header                   map[string]string
	name, doc, code, message string
}

const malformed = "The XML you provided was not well-formed or did not validate against " +
	"our published schema."

// m is a case refused with MalformedXML.
func m(name, doc string) validationCase {
	return validationCase{name: name, doc: doc, code: "MalformedXML", message: malformed}
}

// arg is a case refused with InvalidArgument and msg.
func arg(name, doc, msg string) validationCase {
	return validationCase{name: name, doc: doc, code: "InvalidArgument", message: msg}
}

// req is a case refused with InvalidRequest and msg.
func req(name, doc, msg string) validationCase {
	return validationCase{name: name, doc: doc, code: "InvalidRequest", message: msg}
}

// rule is a configuration of one rule made of parts.
func rule(parts ...string) string { return lifecycleDoc(strings.Join(parts, "")) }

// validationCases are the refusals of the validation table, V1 to V29,
// and its first error first (L30).
func validationCases() []validationCase {
	return slices.Concat(documentCases(), filterCases(), actionCases(), noncurrentCases())
}

// documentCases are the refusals of a document and of a rule's header
// (V1 to V8).
func documentCases() []validationCase {
	thousandAndOne := make([]string, 0, 1001)
	for i := range 1001 {
		thousandAndOne = append(thousandAndOne,
			"<ID>r"+strconv.Itoa(i)+"</ID>"+vFilter+vStatus+vExpire)
	}
	return []validationCase{
		m("V1 not XML", "not xml"),
		m("V1 another root", strings.ReplaceAll(lifecycleDoc(vRule),
			"LifecycleConfiguration", "Configuration")),
		m("V1 Days not a number", rule(vID, vFilter, vStatus,
			"<Expiration><Days>x</Days></Expiration>")),
		m("V1 Date not a date", rule(vID, vFilter, vStatus,
			"<Expiration><Date>yesterday</Date></Expiration>")),
		m("V2 no rule", lifecycleDoc()),
		req("V3 1,001 rules", lifecycleDoc(thousandAndOne...),
			"The number of lifecycle rules should not exceed allowed limit of 1000 rules."),
		arg("V4 a 256-character ID", rule("<ID>"+strings.Repeat("é", 256)+"</ID>", vFilter,
			vStatus, vExpire), "ID length should not exceed allowed limit of 255"),
		arg("V5 a repeated ID", lifecycleDoc(vRule, vRule),
			"Rule ID must be unique. Found same ID for more than one rule"),
		m("V6 an unknown Status", rule(vID, vFilter, "<Status>On</Status>", vExpire)),
		m("V6 no Status", rule(vID, vFilter, vExpire)),
		m("V7 neither Filter nor Prefix", rule(vID, vStatus, vExpire)),
		m("V7 both Filter and Prefix", rule(vID, vFilter, "<Prefix>p/</Prefix>", vStatus,
			vExpire)),
		req("V8 a Prefix rule and a Filter rule",
			lifecycleDoc("<ID>a</ID><Prefix>a/</Prefix>"+vStatus+vExpire, vRule),
			"Filter element can only be used in Lifecycle V2."),
	}
}

// filterCases are the refusals of a Filter (V9 to V13).
func filterCases() []validationCase {
	return []validationCase{
		m("V9 a Filter with two predicates", rule(vID,
			"<Filter><Prefix>p/</Prefix><Tag><Key>k</Key><Value>v</Value></Tag></Filter>",
			vStatus, vExpire)),
		m("V9 a repeated Prefix", rule(vID, "<Filter><Prefix>a/</Prefix><Prefix>b/</Prefix>"+
			"</Filter>", vStatus, vExpire)),
		m("V10 an And of one predicate", rule(vID,
			"<Filter><And><Prefix>p/</Prefix></And></Filter>", vStatus, vExpire)),
		m("V10 an And of two Prefixes", rule(vID, "<Filter><And><Prefix>a/</Prefix>"+
			"<Prefix>b/</Prefix></And></Filter>", vStatus, vExpire)),
		arg("V11 an And repeating a tag key", rule(vID, "<Filter><And>"+
			"<Tag><Key>k</Key><Value>1</Value></Tag><Tag><Key>k</Key><Value>2</Value></Tag>"+
			"</And></Filter>", vStatus, vExpire), "Duplicate Tag Keys are not allowed."),
		{
			name: "V12 a reserved tag key", code: "InvalidTag",
			message: "Your TagKey cannot be prefixed with aws:",
			doc: rule(vID, "<Filter><Tag><Key>aws:x</Key><Value>v</Value></Tag></Filter>",
				vStatus, vExpire),
		},
		arg("V13 a negative ObjectSizeGreaterThan", rule(vID,
			"<Filter><ObjectSizeGreaterThan>-1</ObjectSizeGreaterThan></Filter>", vStatus,
			vExpire), "ObjectSizeGreaterThan must be a non-negative integer"),
		arg("V13 an ObjectSizeLessThan of 0", rule(vID,
			"<Filter><ObjectSizeLessThan>0</ObjectSizeLessThan></Filter>", vStatus, vExpire),
			"ObjectSizeLessThan must be a positive integer"),
		arg("V13 bounds that admit nothing", rule(vID, "<Filter><And>"+
			"<ObjectSizeGreaterThan>10</ObjectSizeGreaterThan>"+
			"<ObjectSizeLessThan>10</ObjectSizeLessThan></And></Filter>", vStatus, vExpire),
			"ObjectSizeGreaterThan must be less than ObjectSizeLessThan"),
	}
}

// actionCases are the refusals of a rule's actions on current versions
// and uploads (V14 to V22).
func actionCases() []validationCase {
	tagged := `<Filter><Tag><Key>team</Key><Value>x</Value></Tag></Filter>`
	return []validationCase{
		m("V14 no action", rule(vID, vFilter, vStatus)),
		m("V15 an empty Expiration", rule(vID, vFilter, vStatus, "<Expiration></Expiration>")),
		m("V15 Days and Date", rule(vID, vFilter, vStatus,
			"<Expiration><Days>1</Days><Date>2014-02-01T00:00:00Z</Date></Expiration>")),
		arg("V16 ExpiredObjectDeleteMarker with Days", rule(vID, vFilter, vStatus,
			"<Expiration><Days>1</Days><ExpiredObjectDeleteMarker>true"+
				"</ExpiredObjectDeleteMarker></Expiration>"),
			"ExpiredObjectDeleteMarker cannot be specified with Days or Date in a "+
				"Lifecycle Expiration Policy."),
		arg("V17 Days of 0", rule(vID, vFilter, vStatus,
			"<Expiration><Days>0</Days></Expiration>"),
			"'Days' for Expiration action must be a positive integer"),
		arg("V18 a Date not at midnight", rule(vID, vFilter, vStatus,
			"<Expiration><Date>2014-02-01T10:00:00Z</Date></Expiration>"),
			"'Date' must be at midnight GMT"),
		req("V19 ExpiredObjectDeleteMarker with a tag filter", rule(vID, tagged, vStatus,
			"<Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker>"+
				"</Expiration>"), "ExpiredObjectDeleteMarker cannot be specified with Tags."),
		req("V19 AbortIncompleteMultipartUpload with a tag filter", rule(vID, tagged, vStatus,
			"<AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation>"+
				"</AbortIncompleteMultipartUpload>"),
			"AbortIncompleteMultipartUpload cannot be specified with Tags."),
		arg("V20 DaysAfterInitiation of 0", rule(vID, vFilter, vStatus,
			"<AbortIncompleteMultipartUpload><DaysAfterInitiation>0</DaysAfterInitiation>"+
				"</AbortIncompleteMultipartUpload>"),
			"'DaysAfterInitiation' for AbortIncompleteMultipartUpload action must be a "+
				"positive integer"),
		arg("V20 no DaysAfterInitiation", rule(vID, vFilter, vStatus,
			"<AbortIncompleteMultipartUpload></AbortIncompleteMultipartUpload>"),
			"'DaysAfterInitiation' for AbortIncompleteMultipartUpload action must be a "+
				"positive integer"),
		m("V21 no NoncurrentDays", rule(vID, vFilter, vStatus,
			"<NoncurrentVersionExpiration><NewerNoncurrentVersions>1"+
				"</NewerNoncurrentVersions></NoncurrentVersionExpiration>")),
		arg("V22 NoncurrentDays of 0", rule(vID, vFilter, vStatus,
			"<NoncurrentVersionExpiration><NoncurrentDays>0</NoncurrentDays>"+
				"</NoncurrentVersionExpiration>"),
			"'NoncurrentDays' for NoncurrentVersionExpiration action must be a positive "+
				"integer"),
	}
}

// noncurrentCases are the refusals of NewerNoncurrentVersions,
// transitions, the header and the size, and the first of two errors
// (V23 to V29, L30).
func noncurrentCases() []validationCase {
	return []validationCase{
		arg("V23 NewerNoncurrentVersions of 0", rule(vID, vFilter, vStatus,
			"<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays>"+
				"<NewerNoncurrentVersions>0</NewerNoncurrentVersions>"+
				"</NoncurrentVersionExpiration>"),
			"NewerNoncurrentVersions for NoncurrentVersionExpiration action must be a "+
				"positive integer"),
		arg("V23 NewerNoncurrentVersions of 101", rule(vID, vFilter, vStatus,
			"<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays>"+
				"<NewerNoncurrentVersions>101</NewerNoncurrentVersions>"+
				"</NoncurrentVersionExpiration>"),
			"NewerNoncurrentVersions for NoncurrentVersionExpiration action must not "+
				"exceed 100"),
		req("V24 NewerNoncurrentVersions without a Filter", rule(vID, "<Prefix>p/</Prefix>",
			vStatus, "<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays>"+
				"<NewerNoncurrentVersions>1</NewerNoncurrentVersions>"+
				"</NoncurrentVersionExpiration>"),
			"NewerNoncurrentVersions element can only be used with a Filter element."),
		m("V25 an unknown StorageClass", rule(vID, vFilter, vStatus,
			"<Transition><Days>1</Days><StorageClass>STANDARD</StorageClass></Transition>")),
		m("V25 a Transition without Days or Date", rule(vID, vFilter, vStatus,
			"<Transition><StorageClass>GLACIER</StorageClass></Transition>")),
		arg("V26 Transition Days of -1", rule(vID, vFilter, vStatus,
			"<Transition><Days>-1</Days><StorageClass>GLACIER</StorageClass></Transition>"),
			"'Days' in Transition action must be greater than or equal to zero"),
		arg("V26 NoncurrentVersionTransition NoncurrentDays of -1", rule(vID, vFilter,
			vStatus, "<NoncurrentVersionTransition><NoncurrentDays>-1</NoncurrentDays>"+
				"<StorageClass>GLACIER</StorageClass></NoncurrentVersionTransition>"),
			"'NoncurrentDays' in NoncurrentVersionTransition action must be greater than "+
				"or equal to zero"),
		{
			name: "V27 an unknown default minimum object size", doc: lifecycleDoc(vRule),
			code:    "InvalidArgument",
			message: "Invalid value for header x-amz-transition-default-minimum-object-size",
			header:  map[string]string{"X-Amz-Transition-Default-Minimum-Object-Size": "128K"},
		},
		{
			name: "V28 a document over 1 MiB", code: "MaxMessageLengthExceeded",
			message: "Your request was too big.",
			doc: lifecycleDoc(vID + vFilter + vStatus + vExpire +
				"<Unknown>" + strings.Repeat("x", 1<<20) + "</Unknown>"),
		},
		arg("V29 Expiration Days and Transition Date", rule(vID, vFilter, vStatus, vExpire,
			"<Transition><Date>2014-02-01T00:00:00Z</Date><StorageClass>GLACIER"+
				"</StorageClass></Transition>"),
			"Days and Date cannot both be used in the Expiration and Transition actions "+
				"of one rule"),
		arg("L30 the first rule's error wins", lifecycleDoc(
			vID+vFilter+vStatus+"<Expiration><Days>0</Days></Expiration>",
			"<ID>"+strings.Repeat("x", 256)+"</ID>"+vFilter+vStatus+vExpire),
			"'Days' for Expiration action must be a positive integer"),
	}
}

// Every refusal of the validation table is answered 400 with its code and
// message, and leaves the configuration the bucket had.
func TestBucketLifecycleValidation(t *testing.T) {
	l := startLive(t, nil)
	c := clientFor(l)
	createBucket(t, c, "lcv")
	kept := []types.LifecycleRule{enabled("kept", prefixFilter("kept/"), days(1))}
	putLifecycle(t, c, "lcv", kept...)

	for _, tc := range validationCases() {
		t.Run(tc.name, func(t *testing.T) {
			res := sendLive(t, lifecycleRequest(t, l, http.MethodPut, "lcv", tc.doc, tc.header))
			requireEqual(t, res.StatusCode, http.StatusBadRequest, "status of "+res.Body)
			var e s3Error
			requireNoErr(t, xml.Unmarshal([]byte(res.Body), &e), "decode "+res.Body)
			requireEqual(t, e.Code, tc.code, "code")
			requireEqual(t, e.Message, tc.message, "message")
		})
	}
	sameJSON(t, getLifecycle(t, c, "lcv").Rules, kept, "the configuration kept")

	// Accepted: an empty Filter and an empty Prefix, which admit every
	// object, ExpiredObjectDeleteMarker false, elements S3 does not
	// define, and noncurrent actions in a bucket never versioned.
	accepted := lifecycleDoc(
		"<ID>a</ID><Filter></Filter>"+vStatus+vExpire,
		"<ID>b</ID><Filter><Prefix></Prefix></Filter>"+vStatus+
			"<Expiration><ExpiredObjectDeleteMarker>false</ExpiredObjectDeleteMarker>"+
			"</Expiration><Unknown>1</Unknown>",
		"<ID>c</ID>"+vFilter+vStatus+"<NoncurrentVersionExpiration><NoncurrentDays>1"+
			"</NoncurrentDays></NoncurrentVersionExpiration>")
	res := sendLive(t, lifecycleRequest(t, l, http.MethodPut, "lcv", accepted, nil))
	requireEqual(t, res.StatusCode, http.StatusOK, "accepted: "+res.Body)
	requireEqual(t, res.Body, "", "accepted body")
	requireEqual(t, res.Header.Get("X-Amz-Transition-Default-Minimum-Object-Size"),
		"all_storage_classes_128K", "accepted header")
	res = sendLive(t, lifecycleRequest(t, l, http.MethodGet, "lcv", "", nil))
	requireEqual(t, res.StatusCode, http.StatusOK, "get")
	requireEqual(t, res.Header.Get("Content-Type"), "application/xml", "get content type")
	want := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`<Rule><ID>a</ID><Filter></Filter><Status>Enabled</Status>` +
		`<Expiration><Days>1</Days></Expiration></Rule>` +
		`<Rule><ID>b</ID><Filter><Prefix></Prefix></Filter><Status>Enabled</Status>` +
		`<Expiration><ExpiredObjectDeleteMarker>false</ExpiredObjectDeleteMarker>` +
		`</Expiration></Rule>` +
		`<Rule><ID>c</ID><Filter><Prefix>p/</Prefix></Filter><Status>Enabled</Status>` +
		`<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays>` +
		`</NoncurrentVersionExpiration></Rule></LifecycleConfiguration>`
	requireEqual(t, res.Body, want, "the stored document")
}

// In a bucket never versioned, a rule's matching objects are deleted at
// the first sweep from the midnight it makes them eligible at, and only
// those: by prefix, tag and size, never by a Disabled rule.
func TestLifecycleExpiresObjects(t *testing.T) {
	c, clk := startLifecycle(t, nil)
	ctx := t.Context()
	createBucket(t, c, "exp")
	for key, size := range map[string]int{
		"logs/a": 1, "keep/b": 1, "tagged": 1, "tagged-other": 1, "big": 200, "small": 5,
	} {
		put(t, c, "exp", key, bytes.Repeat([]byte("x"), size))
	}
	for key, team := range map[string]string{"tagged": "x", "tagged-other": "y"} {
		_, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
			Bucket: aws.String("exp"), Key: aws.String(key), Tagging: tags("team", team),
		})
		requireNoErr(t, err, "tag "+key)
	}
	disabled := enabled("disabled", prefixFilter("keep/"), days(1))
	disabled.Status = types.ExpirationStatusDisabled
	putLifecycle(t, c, "exp",
		enabled("logs", prefixFilter("logs/"), days(3)),
		enabled("team", &types.LifecycleRuleFilter{
			Tag: &types.Tag{Key: aws.String("team"), Value: aws.String("x")},
		}, days(3)),
		enabled("big objects", &types.LifecycleRuleFilter{
			ObjectSizeGreaterThan: aws.Int64(100),
		}, days(3)),
		disabled,
	)
	all := []string{"big", "keep/b", "logs/a", "small", "tagged", "tagged-other"}
	requireExpiresHeaders(t, c)

	// Created 1/15 10:30 with 3 days, they are eligible on 1/19 at 00:00
	// UTC (the User Guide's example): the sweep at 1/18 23:59 leaves them,
	// and the tick at 1/19 00:00, the scheduler's next, expires them.
	advance(t, clk, day(18, 23, 59))
	requireKeys(t, c, "exp", all...)
	advance(t, clk, day(19))
	eventuallyKeys(t, c, "exp", "keep/b", "small", "tagged-other")

	// A Date already past expires a matching object at the next tick after
	// the configuration is put.
	put(t, c, "exp", "dated/x", []byte("x"))
	putLifecycle(t, c, "exp", enabled("dated", prefixFilter("dated/"),
		&types.LifecycleExpiration{Date: aws.Time(day(1))}))
	requireKeys(t, c, "exp", "dated/x", "keep/b", "small", "tagged-other")
	advance(t, clk, day(19, 0, 1))
	eventuallyKeys(t, c, "exp", "keep/b", "small", "tagged-other")
}

// requireExpiresHeaders checks the x-amz-expiration of TestLifecycleExpiresObjects'
// objects before they expire: each matching object's is its rule's
// 1/19 midnight, the rule ID URL-encoded, on GetObject, HeadObject,
// PutObject, CopyObject and CompleteMultipartUpload, and the others have
// none, keep/b's rule being Disabled.
func requireExpiresHeaders(t *testing.T, c *s3.Client) {
	t.Helper()
	ctx := t.Context()
	for key, want := range map[string]string{
		"logs/a": expiresOn(19, "logs"), "tagged": expiresOn(19, "team"),
		"big":    expiresOn(19, "big%20objects"),
		"keep/b": "", "small": "", "tagged-other": "",
	} {
		requireExpiration(t, c, "exp", key, "", want)
	}

	// The writes answer it for the object they write: logs/a is written
	// again, by each, as it was.
	out := put(t, c, "exp", "logs/a", []byte("x"))
	requireEqual(t, aws.ToString(out.Expiration), expiresOn(19, "logs"), "PutObject")
	out = put(t, c, "exp", "keep/b", []byte("x"))
	requireEqual(t, aws.ToString(out.Expiration), "", "PutObject under a Disabled rule")
	cp, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("exp"), Key: aws.String("logs/a"), CopySource: aws.String("exp/keep/b"),
	})
	requireNoErr(t, err, "copy keep/b to logs/a")
	requireEqual(t, aws.ToString(cp.Expiration), expiresOn(19, "logs"), "CopyObject")
	id := createUpload(t, c, "exp", "logs/a")
	part, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String("exp"), Key: aws.String("logs/a"), UploadId: aws.String(id),
		PartNumber: aws.Int32(1), Body: strings.NewReader("x"),
	})
	requireNoErr(t, err, "upload part")
	done, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("exp"), Key: aws.String("logs/a"), UploadId: aws.String(id),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{ETag: part.ETag, PartNumber: aws.Int32(1)},
		}},
	})
	requireNoErr(t, err, "complete")
	requireEqual(t, aws.ToString(done.Expiration), expiresOn(19, "logs"),
		"CompleteMultipartUpload")
}

// versionsOf returns bucket's versions and delete markers of key, newest
// first, each marked as a marker or not.
func versionsOf(t *testing.T, c *s3.Client, bucket, key string) []entry {
	t.Helper()
	out := listVersions(t, c, s3.ListObjectVersionsInput{
		Bucket: aws.String(bucket), Prefix: aws.String(key),
	})
	var got []entry
	for _, v := range out.Versions {
		if aws.ToString(v.Key) == key {
			got = append(got, entry{
				Key: key, VersionID: aws.ToString(v.VersionId), Latest: aws.ToBool(v.IsLatest),
			})
		}
	}
	for _, m := range out.DeleteMarkers {
		if aws.ToString(m.Key) == key {
			got = append(got, entry{
				Key: key, VersionID: aws.ToString(m.VersionId), Latest: aws.ToBool(m.IsLatest),
				Marker: true,
			})
		}
	}
	return got
}

// versionedRules are TestLifecycleVersionedBucket's rules: Expiration
// and NoncurrentVersionExpiration under exp/, NoncurrentVersionExpiration
// and ExpiredObjectDeleteMarker under eodm/, and NoncurrentVersionExpiration
// keeping two versions under newer/.
func versionedRules() []types.LifecycleRule {
	return []types.LifecycleRule{
		{
			ID: aws.String("exp"), Status: types.ExpirationStatusEnabled,
			Filter: prefixFilter("exp/"), Expiration: days(1),
			NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{
				NoncurrentDays: aws.Int32(1),
			},
		},
		{
			ID: aws.String("eodm-versions"), Status: types.ExpirationStatusEnabled,
			Filter: prefixFilter("eodm/"),
			NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{
				NoncurrentDays: aws.Int32(1),
			},
		},
		enabled("eodm-markers", prefixFilter("eodm/"),
			&types.LifecycleExpiration{ExpiredObjectDeleteMarker: aws.Bool(true)}),
		{
			ID: aws.String("newer"), Status: types.ExpirationStatusEnabled,
			Filter: prefixFilter("newer/"),
			NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{
				NoncurrentDays: aws.Int32(1), NewerNoncurrentVersions: aws.Int32(2),
			},
		},
	}
}

// requireExpiredToMarker checks, once lifecycle expired exp/k of ver on
// 1/17, that its current version is a delete marker of that midnight and
// v1, its version, is kept as noncurrent.
func requireExpiredToMarker(t *testing.T, c *s3.Client, v1 string) {
	t.Helper()
	eventually(t, "exp/k to get a marker", func() bool {
		return len(versionsOf(t, c, "ver", "exp/k")) == 2
	})
	out := listVersions(t, c, s3.ListObjectVersionsInput{
		Bucket: aws.String("ver"), Prefix: aws.String("exp/"),
	})
	requireEqual(t, len(out.DeleteMarkers), 1, "exp/ markers")
	marker := out.DeleteMarkers[0]
	requireEqual(t, aws.ToBool(marker.IsLatest), true, "the marker is latest")
	requireEqual(t, aws.ToTime(marker.LastModified).UTC(), day(17), "the marker's time")
	requireEqual(t, len(out.Versions), 1, "exp/ versions")
	requireEqual(t, aws.ToString(out.Versions[0].VersionId), v1, "the version kept")
	requireEqual(t, aws.ToBool(out.Versions[0].IsLatest), false, "the version is noncurrent")
	_, err := c.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("ver"), Key: aws.String("exp/k"),
	})
	requireCode(t, err, "NoSuchKey")
}

// In an Enabled bucket, Expiration adds a delete marker at midnight and
// keeps the version as noncurrent, NoncurrentVersionExpiration removes
// noncurrent versions but the newest it is told to keep, and an
// ExpiredObjectDeleteMarker rule removes a marker left without versions.
func TestLifecycleVersionedBucket(t *testing.T) {
	c, clk := startLifecycle(t, nil)
	ctx := t.Context()
	versionedBucket(t, c, "ver")
	expV1 := putVersion(t, c, "ver", "exp/k", "one")
	putVersion(t, c, "ver", "eodm/k", "one")
	_, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("ver"), Key: aws.String("eodm/k"),
	})
	requireNoErr(t, err, "delete eodm/k")
	newer := make([]string, 0, 4)
	for i := range 4 {
		newer = append(newer, putVersion(t, c, "ver", "newer/k", strconv.Itoa(i)))
	}
	putLifecycle(t, c, "ver", versionedRules()...)
	// The current version answers its expiry, by version too, and rules
	// with no Expiration by Days or Date answer none.
	requireExpiration(t, c, "ver", "exp/k", "", expiresOn(17, "exp"))
	requireExpiration(t, c, "ver", "exp/k", expV1, expiresOn(17, "exp"))
	requireExpiration(t, c, "ver", "newer/k", "", "")

	// Everything was written 1/15 10:30, so a day's rules make it eligible
	// on 1/17 at 00:00.
	advance(t, clk, day(16, 23, 59))
	requireEqual(t, len(versionsOf(t, c, "ver", "exp/k")), 1, "exp/k versions on 1/16")
	requireEqual(t, len(versionsOf(t, c, "ver", "eodm/k")), 2, "eodm/k versions on 1/16")
	requireEqual(t, len(versionsOf(t, c, "ver", "newer/k")), 4, "newer/k versions on 1/16")
	advance(t, clk, day(17))

	requireExpiredToMarker(t, c, expV1)
	// A noncurrent version answers none, though its rule's Expiration
	// admits it.
	requireExpiration(t, c, "ver", "exp/k", expV1, "")

	// eodm/k's noncurrent version went, and its marker with it.
	requireEqual(t, len(versionsOf(t, c, "ver", "eodm/k")), 0, "eodm/k versions on 1/17")

	// newer/k keeps its current version and the two newest noncurrent.
	got := versionsOf(t, c, "ver", "newer/k")
	want := []entry{
		{Key: "newer/k", VersionID: newer[3], Latest: true},
		{Key: "newer/k", VersionID: newer[2]},
		{Key: "newer/k", VersionID: newer[1]},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("newer/k versions %+v, want %+v", got, want)
	}

	// exp/k's version became noncurrent on 1/17, so it goes on 1/19, and
	// the marker, left alone, with it.
	advance(t, clk, day(18, 23, 59))
	requireEqual(t, len(versionsOf(t, c, "ver", "exp/k")), 2, "exp/k versions on 1/18")
	advance(t, clk, day(19))
	eventually(t, "exp/k to be gone", func() bool {
		return len(versionsOf(t, c, "ver", "exp/k")) == 0
	})
}

// In a Suspended bucket, Expiration replaces the current null version
// with a null delete marker, which removes its data, and keeps a current
// version that is not null as noncurrent, removing the key's noncurrent
// null version (S3 User Guide, "Expiring objects").
func TestLifecycleSuspendedBucket(t *testing.T) {
	c, clk := startLifecycle(t, nil)
	ctx := t.Context()
	createBucket(t, c, "sus")
	put(t, c, "sus", "null/k", []byte("null"))
	put(t, c, "sus", "mixed/k", []byte("null"))
	setVersioning(t, c, "sus", types.BucketVersioningStatusEnabled)
	kept := putVersion(t, c, "sus", "mixed/k", "kept")
	setVersioning(t, c, "sus", types.BucketVersioningStatusSuspended)
	putLifecycle(t, c, "sus", enabled("all", &types.LifecycleRuleFilter{}, days(1)))

	advance(t, clk, day(16, 23, 59))
	requireEqual(t, len(versionsOf(t, c, "sus", "mixed/k")), 2, "mixed/k versions on 1/16")
	advance(t, clk, day(17))

	eventually(t, "null/k to be a marker", func() bool {
		got := versionsOf(t, c, "sus", "null/k")
		return len(got) == 1 && got[0].Marker
	})
	got := versionsOf(t, c, "sus", "null/k")
	want := []entry{{Key: "null/k", VersionID: "null", Latest: true, Marker: true}}
	if !slices.Equal(got, want) {
		t.Fatalf("null/k versions %+v, want %+v", got, want)
	}
	_, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("sus"), Key: aws.String("null/k"), VersionId: aws.String("null"),
	})
	requireResponse(t, err, http.StatusMethodNotAllowed)

	got = versionsOf(t, c, "sus", "mixed/k")
	want = []entry{
		{Key: "mixed/k", VersionID: "null", Latest: true, Marker: true},
		{Key: "mixed/k", VersionID: kept},
	}
	slices.SortFunc(got, func(a, b entry) int {
		if a.Latest != b.Latest {
			if a.Latest {
				return -1
			}
			return 1
		}
		return 0
	})
	if !slices.Equal(got, want) {
		t.Fatalf("mixed/k versions %+v, want %+v", got, want)
	}
	body, _ := getVersion(t, c, "sus", "mixed/k", kept)
	requireEqual(t, body, "kept", "the version kept")
}

// createUpload starts a multipart upload of bucket/key.
func createUpload(t *testing.T, c *s3.Client, bucket, key string) string {
	t.Helper()
	out, err := c.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	requireNoErr(t, err, "create upload "+key)
	return aws.ToString(out.UploadId)
}

// uploadKeys lists the keys of bucket's multipart uploads.
func uploadKeys(t *testing.T, c *s3.Client, bucket string) []string {
	t.Helper()
	out, err := c.ListMultipartUploads(t.Context(),
		&s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
	requireNoErr(t, err, "list uploads")
	keys := make([]string, 0, len(out.Uploads))
	for _, u := range out.Uploads {
		keys = append(keys, aws.ToString(u.Key))
	}
	return keys
}

// requireAbortHeaders checks that ListParts of stale, an upload of tmp/a,
// and CreateMultipartUpload under tmp/ answer the abort rule's 1/17
// midnight and ID, and that those of kept, an upload of keep/b, and of
// another under keep/ answer neither. The uploads it creates are aborted.
func requireAbortHeaders(t *testing.T, c *s3.Client, stale, kept string) {
	t.Helper()
	ctx := t.Context()
	for key, id := range map[string]string{"tmp/a": stale, "keep/b": kept} {
		out, err := c.ListParts(ctx, &s3.ListPartsInput{
			Bucket: aws.String("mpu"), Key: aws.String(key), UploadId: aws.String(id),
		})
		requireNoErr(t, err, "list parts of "+key)
		requireAbort(t, key, out.AbortDate, out.AbortRuleId)
	}
	for _, key := range []string{"tmp/new", "keep/new"} {
		out, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String("mpu"), Key: aws.String(key),
		})
		requireNoErr(t, err, "create upload "+key)
		requireAbort(t, key, out.AbortDate, out.AbortRuleId)
		_, err = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String("mpu"), Key: aws.String(key), UploadId: out.UploadId,
		})
		requireNoErr(t, err, "abort upload "+key)
	}
}

// requireAbort checks an upload of key's AbortDate and AbortRuleId: the
// abort rule's 1/17 midnight under tmp/, and none otherwise.
func requireAbort(t *testing.T, key string, date *time.Time, ruleID *string) {
	t.Helper()
	if !strings.HasPrefix(key, "tmp/") {
		if date != nil || ruleID != nil {
			t.Fatalf("%s: abort date %v and rule %v, want none", key, date, aws.ToString(ruleID))
		}
		return
	}
	if date == nil {
		t.Fatalf("%s: no abort date", key)
	}
	requireEqual(t, date.UTC(), day(17), key+" abort date")
	requireEqual(t, aws.ToString(ruleID), "abort", key+" abort rule")
}

// An upload left incomplete is aborted from the midnight its rule makes it
// eligible at, by prefix; one outside the prefix stays, and one completed
// is untouched.
func TestLifecycleAbortsIncompleteUploads(t *testing.T) {
	c, clk := startLifecycle(t, nil)
	ctx := t.Context()
	createBucket(t, c, "mpu")
	stale := createUpload(t, c, "mpu", "tmp/a")
	kept := createUpload(t, c, "mpu", "keep/b")
	done := createUpload(t, c, "mpu", "tmp/done")
	part, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String("mpu"), Key: aws.String("tmp/done"), UploadId: aws.String(done),
		PartNumber: aws.Int32(1), Body: strings.NewReader("done"),
	})
	requireNoErr(t, err, "upload part")
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("mpu"), Key: aws.String("tmp/done"), UploadId: aws.String(done),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{ETag: part.ETag, PartNumber: aws.Int32(1)},
		}},
	})
	requireNoErr(t, err, "complete")
	putLifecycle(t, c, "mpu", types.LifecycleRule{
		ID: aws.String("abort"), Status: types.ExpirationStatusEnabled,
		Filter: prefixFilter("tmp/"),
		AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{
			DaysAfterInitiation: aws.Int32(1),
		},
	})
	requireAbortHeaders(t, c, stale, kept)

	advance(t, clk, day(16, 23, 59))
	if got := uploadKeys(t, c, "mpu"); !slices.Equal(got, []string{"keep/b", "tmp/a"}) {
		t.Fatalf("uploads on 1/16 %q", got)
	}
	advance(t, clk, day(17))
	eventually(t, "tmp/a's upload to be aborted", func() bool {
		return slices.Equal(uploadKeys(t, c, "mpu"), []string{"keep/b"})
	})
	_, err = c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String("mpu"), Key: aws.String("tmp/a"), UploadId: aws.String(stale),
		PartNumber: aws.Int32(1), Body: strings.NewReader("late"),
	})
	requireCode(t, err, "NoSuchUpload")
	_, body := get(t, c, &s3.GetObjectInput{Bucket: aws.String("mpu"), Key: aws.String("tmp/done")})
	requireEqual(t, string(body), "done", "the completed upload's object")
}

// A configuration survives a restart, and the restarted server applies
// it. The restarted server gets a clock of its own, at the time the first
// one stopped at (see advance).
func TestLifecycleSurvivesRestart(t *testing.T) {
	env := map[string]string{"TULE_Storage_RootDir": t.TempDir()}
	var stoppedAt time.Time
	if !t.Run("configured", func(t *testing.T) {
		c, clk := startLifecycle(t, env)
		createBucket(t, c, "rst")
		put(t, c, "rst", "logs/a", []byte("a"))
		put(t, c, "rst", "keep/b", []byte("b"))
		putLifecycle(t, c, "rst", enabled("logs", prefixFilter("logs/"), days(3)))
		advance(t, clk, day(16))
		requireKeys(t, c, "rst", "keep/b", "logs/a")
		stoppedAt = clk.Now()
	}) {
		return
	}
	t.Run("restarted", func(t *testing.T) {
		clk := newFakeClock(stoppedAt)
		c := clientFor(startLiveWith(t, env, liveOptions{clock: clk}))
		got := getLifecycle(t, c, "rst")
		sameJSON(t, got.Rules,
			[]types.LifecycleRule{enabled("logs", prefixFilter("logs/"), days(3))},
			"the configuration after a restart")
		advance(t, clk, day(18, 23, 59))
		requireKeys(t, c, "rst", "keep/b", "logs/a")
		advance(t, clk, day(19))
		eventuallyKeys(t, c, "rst", "keep/b")
	})
}

// A configuration file left from an earlier bucket of the same name, as a
// server that does not know lifecycle configurations leaves one when it
// deletes the bucket and creates it again, is not the new bucket's: GET
// answers NoSuchLifecycleConfiguration, no object operation answers
// x-amz-expiration by it, and no sweep applies it.
func TestLifecycleOfAnEarlierBucketIsIgnored(t *testing.T) {
	root := t.TempDir()
	c, clk := startLifecycle(t, map[string]string{"TULE_Storage_RootDir": root})
	ctx := t.Context()
	createBucket(t, c, "gen")
	putLifecycle(t, c, "gen", enabled("logs", prefixFilter("logs/"), days(1)))
	file := filepath.Join(root, ".tule", "config", "lifecycle", "gen")
	left, err := os.ReadFile(file) //nolint:gosec // G304: a file of this test's own root
	requireNoErr(t, err, "read the configuration file")

	_, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("gen")})
	requireNoErr(t, err, "delete the bucket")
	createBucket(t, c, "gen")
	//nolint:gosec // G703: a file of this test's own root
	requireNoErr(t, os.WriteFile(file, left, 0o600), "leave the configuration file")
	out := put(t, c, "gen", "logs/a", []byte("a"))
	requireEqual(t, aws.ToString(out.Expiration), "", "PutObject expiration")
	requireExpiration(t, c, "gen", "logs/a", "", "")

	_, err = c.GetBucketLifecycleConfiguration(ctx,
		&s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("gen")})
	requireCode(t, err, "NoSuchLifecycleConfiguration")
	requireResponse(t, err, http.StatusNotFound)

	advance(t, clk, day(17))
	advance(t, clk, day(17, 0, 1))
	requireKeys(t, c, "gen", "logs/a")
}
