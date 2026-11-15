package notification

import (
	"bytes"
	"encoding/xml"
	"testing"
)

// The examples of the S3 User Guide's "Configuring event notifications
// using object key name filtering", verbatim but for the placeholder
// markup around two values, and the closing tag the second invalid
// example lacks its ">" in. Their destinations are in us-west-2 and their
// function ARNs lack "function:", so Parse refuses them before it reaches
// V9 (V7, V8); overlapOf runs the decode and the overlap check alone.
const (
	ugValidPrefixAndSuffix = `<NotificationConfiguration>
  <QueueConfiguration>
      <Id>1</Id>
      <Filter>
          <S3Key>
              <FilterRule>
                  <Name>prefix</Name>
                  <Value>images/</Value>
              </FilterRule>
              <FilterRule>
                  <Name>suffix</Name>
                  <Value>jpg</Value>
              </FilterRule>
          </S3Key>
     </Filter>
     <Queue>arn:aws:sqs:us-west-2:444455556666:s3notificationqueue</Queue>
     <Event>s3:ObjectCreated:Put</Event>
  </QueueConfiguration>
</NotificationConfiguration>`

	ugValidPrefixes = `<NotificationConfiguration>
  <QueueConfiguration>
     <Id>1</Id>
     <Filter>
            <S3Key>
                <FilterRule>
                    <Name>prefix</Name>
                    <Value>images/</Value>
                </FilterRule>
            </S3Key>
     </Filter>
     <Queue>arn:aws:sqs:us-west-2:444455556666:sqs-queue-A</Queue>
     <Event>s3:ObjectCreated:Put</Event>
  </QueueConfiguration>
  <QueueConfiguration>
     <Id>2</Id>
     <Filter>
            <S3Key>
                <FilterRule>
                    <Name>prefix</Name>
                    <Value>logs/</Value>
                </FilterRule>
            </S3Key>
     </Filter>
     <Queue>arn:aws:sqs:us-west-2:444455556666:sqs-queue-B</Queue>
     <Event>s3:ObjectCreated:Put</Event>
  </QueueConfiguration>
</NotificationConfiguration>`

	ugValidSuffixes = `<NotificationConfiguration>
  <CloudFunctionConfiguration>
     <Id>1</Id>
     <Filter>
            <S3Key>
                <FilterRule>
                    <Name>suffix</Name>
                    <Value>.jpg</Value>
                </FilterRule>
            </S3Key>
     </Filter>
     <CloudFunction>arn:aws:lambda:us-west-2:444455556666:cloud-function-A</CloudFunction>
     <Event>s3:ObjectCreated:Put</Event>
  </CloudFunctionConfiguration>
  <CloudFunctionConfiguration>
     <Id>2</Id>
     <Filter>
            <S3Key>
                <FilterRule>
                    <Name>suffix</Name>
                    <Value>.png</Value>
                </FilterRule>
            </S3Key>
     </Filter>
     <CloudFunction>arn:aws:lambda:us-west-2:444455556666:cloud-function-B</CloudFunction>
     <Event>s3:ObjectCreated:Put</Event>
  </CloudFunctionConfiguration>
</NotificationConfiguration>`

	ugValidCommonPrefix = `<NotificationConfiguration>
  <CloudFunctionConfiguration>
     <Id>1</Id>
     <Filter>
            <S3Key>
                <FilterRule>
                    <Name>prefix</Name>
                    <Value>images</Value>
                </FilterRule>
                <FilterRule>
                    <Name>suffix</Name>
                    <Value>.jpg</Value>
                </FilterRule>
            </S3Key>
     </Filter>
     <CloudFunction>arn:aws:lambda:us-west-2:444455556666:cloud-function-A</CloudFunction>
     <Event>s3:ObjectCreated:Put</Event>
  </CloudFunctionConfiguration>
  <CloudFunctionConfiguration>
     <Id>2</Id>
     <Filter>
            <S3Key>
                <FilterRule>
                    <Name>prefix</Name>
                    <Value>images</Value>
                </FilterRule>
                <FilterRule>
                    <Name>suffix</Name>
                    <Value>.png</Value>
                </FilterRule>
            </S3Key>
     </Filter>
     <CloudFunction>arn:aws:lambda:us-west-2:444455556666:cloud-function-B</CloudFunction>
     <Event>s3:ObjectCreated:Put</Event>
  </CloudFunctionConfiguration>
</NotificationConfiguration>`

	ugInvalidRootPrefix = `<NotificationConfiguration>
     <TopicConfiguration>
         <Topic>arn:aws:sns:us-west-2:444455556666:sns-notification-one</Topic>
         <Event>s3:ObjectCreated:*</Event>
    </TopicConfiguration>
    <TopicConfiguration>
         <Topic>arn:aws:sns:us-west-2:444455556666:sns-notification-two</Topic>
         <Event>s3:ObjectCreated:*</Event>
         <Filter>
             <S3Key>
                 <FilterRule>
                     <Name>prefix</Name>
                     <Value>images</Value>
                 </FilterRule>
            </S3Key>
        </Filter>
    </TopicConfiguration>
</NotificationConfiguration>`

	ugInvalidSuffixes = ` <NotificationConfiguration>
     <TopicConfiguration>
         <Topic>arn:aws:sns:us-west-2:444455556666:sns-topic-one</Topic>
         <Event>s3:ObjectCreated:*</Event>
         <Filter>
             <S3Key>
                 <FilterRule>
                     <Name>suffix</Name>
                     <Value>jpg</Value>
                 </FilterRule>
            </S3Key>
        </Filter>
    </TopicConfiguration>
    <TopicConfiguration>
         <Topic>arn:aws:sns:us-west-2:444455556666:sns-topic-two</Topic>
         <Event>s3:ObjectCreated:Put</Event>
         <Filter>
             <S3Key>
                 <FilterRule>
                     <Name>suffix</Name>
                     <Value>pg</Value>
                 </FilterRule>
            </S3Key>
        </Filter>
    </TopicConfiguration>
</NotificationConfiguration>`

	ugInvalidPrefixAndSuffix = `<NotificationConfiguration>
     <TopicConfiguration>
         <Topic>arn:aws:sns:us-west-2:444455556666:sns-topic-one</Topic>
         <Event>s3:ObjectCreated:*</Event>
         <Filter>
             <S3Key>
                 <FilterRule>
                     <Name>prefix</Name>
                     <Value>images</Value>
                 </FilterRule>
                 <FilterRule>
                     <Name>suffix</Name>
                     <Value>jpg</Value>
                 </FilterRule>
            </S3Key>
        </Filter>
    </TopicConfiguration>
    <TopicConfiguration>
         <Topic>arn:aws:sns:us-west-2:444455556666:sns-topic-two</Topic>
         <Event>s3:ObjectCreated:Put</Event>
         <Filter>
             <S3Key>
                 <FilterRule>
                     <Name>suffix</Name>
                     <Value>jpg</Value>
                 </FilterRule>
            </S3Key>
        </Filter>
    </TopicConfiguration>
</NotificationConfiguration>`
)

// overlapOf decodes body as Parse does and runs the overlap check alone.
func overlapOf(t *testing.T, body string) error {
	t.Helper()
	var w wireDocument
	if err := xml.NewDecoder(bytes.NewReader([]byte(body))).Decode(&w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	configs := make([]decodedConfig, 0, len(w.entries))
	for i := range w.entries {
		d, ok := w.entries[i].decode()
		if !ok {
			t.Fatalf("configuration %d does not decode", i)
		}
		if err := checkFilter(d.config.Filter); err != nil {
			t.Fatalf("configuration %d filter: %v", i, err)
		}
		configs = append(configs, d)
	}
	return checkOverlap(configs)
}

func TestUserGuideFilterExamples(t *testing.T) {
	for name, body := range map[string]string{
		"valid prefix and suffix":      ugValidPrefixAndSuffix,
		"valid non-overlapping prefix": ugValidPrefixes,
		"valid non-overlapping suffix": ugValidSuffixes,
		"valid common prefix":          ugValidCommonPrefix,
	} {
		if err := overlapOf(t, body); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, body := range map[string]string{
		"invalid root prefix":       ugInvalidRootPrefix,
		"invalid suffixes":          ugInvalidSuffixes,
		"invalid prefix and suffix": ugInvalidPrefixAndSuffix,
	} {
		t.Run(name, func(t *testing.T) {
			requireError(t, overlapOf(t, body), ia, msgOverlap)
		})
	}
}

// TestUserGuideDifferentEventTypes is the guide's example of overlapping
// filters for different event types: "the prefix image/ for the
// ObjectCreated:Put event type and the prefix image/ for the
// ObjectRemoved:* event type".
func TestUserGuideDifferentEventTypes(t *testing.T) {
	if _, err := Parse([]byte(doc(
		queue(ev("s3:ObjectCreated:Put")+filter(filterRule("prefix", "image/"))),
		queue(ev("s3:ObjectRemoved:*")+filter(filterRule("prefix", "image/"))),
	))); err != nil {
		t.Fatal(err)
	}
}

func TestOverlap(t *testing.T) {
	both := func(a, b string) string { return doc(queue(a), queue(b)) }
	pre := func(v string) string { return filterRule("prefix", v) }
	suf := func(v string) string { return filterRule("suffix", v) }
	for _, tc := range []struct {
		name    string
		body    string
		overlap bool
	}{
		{"same event, no filters", both(ev(put), ev(put)), true},
		{"wildcard and member", both(ev("s3:ObjectCreated:*"), ev("s3:ObjectCreated:Copy")), true},
		{"two wildcards", both(ev("s3:ObjectTagging:*"), ev("s3:ObjectTagging:*")), true},
		{"different families", both(ev("s3:ObjectCreated:*"), ev("s3:ObjectRemoved:*")), false},
		{
			"ObjectRemoved and LifecycleExpiration",
			both(ev("s3:ObjectRemoved:*"), ev("s3:LifecycleExpiration:*")), false,
		},
		{
			"one shared among many",
			both(ev("s3:ObjectAcl:Put")+ev(put), ev("s3:ObjectRemoved:*")+ev(put)), true,
		},
		{"nested prefixes", both(ev(put)+filter(pre("a/")), ev(put)+filter(pre("a/b/"))), true},
		{"equal prefixes", both(ev(put)+filter(pre("a")), ev(put)+filter(pre("a"))), true},
		{"disjoint prefixes", both(ev(put)+filter(pre("a")), ev(put)+filter(pre("b"))), false},
		{"a prefix and a suffix", both(ev(put)+filter(pre("a")), ev(put)+filter(suf("b"))), true},
		{"disjoint suffixes", both(ev(put)+filter(suf(".jpg")), ev(put)+filter(suf(".png"))), false},
		{
			"common prefix, disjoint suffixes",
			both(ev(put)+filter(pre("x/"), suf("1")), ev(put)+filter(pre("x/"), suf("2"))), false,
		},
		{
			"disjoint prefixes, common suffix",
			both(ev(put)+filter(pre("x/"), suf("1")), ev(put)+filter(pre("y/"), suf("1"))), false,
		},
		{
			"an empty prefix value is the root",
			both(ev(put)+filter(pre("")), ev(put)+filter(pre("a"))), true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			if tc.overlap {
				requireError(t, err, ia, msgOverlap)
			} else if err != nil {
				t.Error(err)
			}
		})
	}
}
