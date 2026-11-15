package lifecycle

import (
	"bytes"
	"testing"
)

func TestEncodeIsCanonical(t *testing.T) {
	c, err := Parse([]byte(doc(
		rule("<Filter><And><Prefix>a/</Prefix><Tag><Key>k</Key><Value>v</Value></Tag>"+
			"<ObjectSizeGreaterThan>1</ObjectSizeGreaterThan></And></Filter><ID>v2</ID>"+enabled+
			"<NoncurrentVersionExpiration><NewerNoncurrentVersions>2</NewerNoncurrentVersions>"+
			"<NoncurrentDays>3</NoncurrentDays></NoncurrentVersionExpiration>"+
			"<NoncurrentVersionTransition><NoncurrentDays>0</NoncurrentDays>"+
			"<StorageClass>GLACIER_IR</StorageClass></NoncurrentVersionTransition>"+
			"<Transition><Date>2029-01-01</Date><StorageClass>GLACIER</StorageClass></Transition>"+
			"<Expiration><Date>2030-01-01</Date></Expiration>"),
		rule("<ID>empty</ID><Filter></Filter><Status>Disabled</Status><Expiration>"+
			"<ExpiredObjectDeleteMarker>false</ExpiredObjectDeleteMarker></Expiration>"+
			"<AbortIncompleteMultipartUpload><DaysAfterInitiation>7</DaysAfterInitiation>"+
			"</AbortIncompleteMultipartUpload>"),
		rule("<ID>tag</ID><Filter><Tag><Key>e</Key></Tag></Filter>"+enabled+
			"<Expiration><Date>2030-01-02T00:00:00Z</Date></Expiration>"),
	)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Encode("g1", c, TransitionDefaultVaries)
	if err != nil {
		t.Fatal(err)
	}
	s, decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	const want = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`<Rule><ID>v2</ID><Filter><And><ObjectSizeGreaterThan>1</ObjectSizeGreaterThan>` +
		`<Prefix>a/</Prefix><Tag><Key>k</Key><Value>v</Value></Tag></And></Filter>` +
		`<Status>Enabled</Status><Expiration><Date>2030-01-01T00:00:00.000Z</Date></Expiration>` +
		`<Transition><Date>2029-01-01T00:00:00.000Z</Date><StorageClass>GLACIER</StorageClass>` +
		`</Transition>` +
		`<NoncurrentVersionTransition><NoncurrentDays>0</NoncurrentDays>` +
		`<StorageClass>GLACIER_IR</StorageClass></NoncurrentVersionTransition>` +
		`<NoncurrentVersionExpiration><NewerNoncurrentVersions>2</NewerNoncurrentVersions>` +
		`<NoncurrentDays>3</NoncurrentDays></NoncurrentVersionExpiration></Rule>` +
		`<Rule><ID>empty</ID><Filter></Filter><Status>Disabled</Status><Expiration>` +
		`<ExpiredObjectDeleteMarker>false</ExpiredObjectDeleteMarker></Expiration>` +
		`<AbortIncompleteMultipartUpload><DaysAfterInitiation>7</DaysAfterInitiation>` +
		`</AbortIncompleteMultipartUpload></Rule>` +
		`<Rule><ID>tag</ID><Filter><Tag><Key>e</Key><Value></Value></Tag></Filter>` +
		`<Status>Enabled</Status><Expiration><Date>2030-01-02T00:00:00.000Z</Date>` +
		`</Expiration></Rule></LifecycleConfiguration>`
	if s.Document != want {
		t.Errorf("document\n%s\nwant\n%s", s.Document, want)
	}
	if s.Generation != "g1" || s.TransitionDefaultMinimumObjectSize != TransitionDefaultVaries {
		t.Errorf("stored %+v", s)
	}

	requireStable(t, raw, s, decoded)
}

// requireStable checks that raw, the stored form of a configuration that
// decoded as s and decoded, encodes again to itself from decoded and from
// its document parsed again.
func requireStable(t *testing.T, raw []byte, s *Stored, decoded *Configuration) {
	t.Helper()
	again, err := Encode(s.Generation, decoded, s.TransitionDefaultMinimumObjectSize)
	if err != nil || !bytes.Equal(again, raw) {
		t.Errorf("re-encoding the decoded configuration changed it (%v):\n%s\n%s", err, again, raw)
	}
	reparsed, err := Parse([]byte(s.Document))
	if err != nil {
		t.Fatalf("the stored document does not parse: %v", err)
	}
	third, err := Encode(s.Generation, reparsed, s.TransitionDefaultMinimumObjectSize)
	if err != nil || !bytes.Equal(third, raw) {
		t.Errorf("the stored document does not parse back to itself (%v):\n%s", err, third)
	}
}

func TestEncodeKeepsATopLevelPrefix(t *testing.T) {
	c, err := Parse([]byte(doc(rule("<ID>v1</ID><Prefix>logs/</Prefix>" + enabled + expire3))))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Encode("g", c, TransitionDefault128K)
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	const rule = `<Rule><ID>v1</ID><Prefix>logs/</Prefix><Status>Enabled</Status>` +
		`<Expiration><Days>3</Days></Expiration></Rule>`
	if !bytes.Contains([]byte(s.Document), []byte(rule)) {
		t.Errorf("document %s", s.Document)
	}
}

func TestDecodeRefusesACorruptFile(t *testing.T) {
	for name, raw := range map[string][]byte{
		"not JSON":         []byte("<x>"),
		"not XML inside":   []byte(`{"generation":"g","document":"<LifecycleConfiguration>"}`),
		"an empty file":    nil,
		"a wrong document": []byte(`{"generation":"g","document":"<Other></Other>"}`),
	} {
		if _, _, err := Decode(raw); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}
