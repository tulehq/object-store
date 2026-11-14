package s3api

import (
	"crypto/md5" //nolint:gosec // S3's integrity check of a customer key is MD5
	"encoding/base64"
	"encoding/xml"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func requireS3Error(t *testing.T, err error, status int, code string) {
	t.Helper()
	var s3Err *Error
	if !errors.As(err, &s3Err) || s3Err.Status != status || s3Err.Code != code {
		t.Fatalf("got %v, want %d %s", err, status, code)
	}
}

func taggingDoc(kv ...string) []byte {
	var b strings.Builder
	b.WriteString(`<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TagSet>`)
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteString("<Tag><Key>" + kv[i] + "</Key><Value>" + kv[i+1] + "</Value></Tag>")
	}
	b.WriteString("</TagSet></Tagging>")
	return []byte(b.String())
}

func TestParseTagging(t *testing.T) {
	got, err := ParseTagging(taggingDoc("team", "storage", "k", "", "é", "a b:c/d=e+f-g@h_i.j"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.TagSet.Tags) != 3 || got.TagSet.Tags[2].Value != "a b:c/d=e+f-g@h_i.j" {
		t.Errorf("tags %+v", got.TagSet.Tags)
	}

	many := make([]string, 0, 102)
	for i := range 51 {
		many = append(many, "k"+strconv.Itoa(i), "v")
	}
	if _, err = ParseTagging(taggingDoc(many[:100]...)); err != nil {
		t.Errorf("50 tags: %v", err)
	}
	for name, doc := range map[string][]byte{
		"51 tags":           taggingDoc(many...),
		"a long key":        taggingDoc(strings.Repeat("k", 129), "v"),
		"a long value":      taggingDoc("k", strings.Repeat("v", 257)),
		"an empty key":      taggingDoc("", "v"),
		"a key with *":      taggingDoc("a*", "v"),
		"a value with <":    taggingDoc("k", "&lt;"),
		"the aws: prefix":   taggingDoc("aws:x", "v"),
		"a key given twice": taggingDoc("k", "1", "k", "2"),
	} {
		_, err = ParseTagging(doc)
		requireS3Error(t, err, http.StatusBadRequest, "InvalidTag")
		t.Logf("%s: %v", name, err)
	}
	for _, doc := range []string{"", "<Tagging/>", "<Other><TagSet/></Other>", "<Tagging><"} {
		_, err = ParseTagging([]byte(doc))
		requireS3Error(t, err, http.StatusBadRequest, "MalformedXML")
	}
}

func corsDoc(rules ...string) []byte {
	return []byte("<CORSConfiguration>" + strings.Join(rules, "") + "</CORSConfiguration>")
}

func TestParseCORSConfiguration(t *testing.T) {
	ok := `<CORSRule><ID>r</ID><AllowedOrigin>http://*.example.test</AllowedOrigin>` +
		`<AllowedMethod>GET</AllowedMethod><AllowedHeader>x-amz-*</AllowedHeader>` +
		`<ExposeHeader>ETag</ExposeHeader><MaxAgeSeconds>60</MaxAgeSeconds></CORSRule>`
	got, err := ParseCORSConfiguration(corsDoc(ok))
	if err != nil {
		t.Fatal(err)
	}
	if r := got.Rules[0]; r.ID != "r" || *r.MaxAgeSeconds != 60 || r.ExposeHeaders[0] != "ETag" {
		t.Errorf("rule %+v", r)
	}

	rule := func(inner string) string { return "<CORSRule>" + inner + "</CORSRule>" }
	origin := "<AllowedOrigin>*</AllowedOrigin>"
	get := "<AllowedMethod>GET</AllowedMethod>"
	for name, doc := range map[string][]byte{
		"PATCH":            corsDoc(rule(origin + "<AllowedMethod>PATCH</AllowedMethod>")),
		"a lowercase get":  corsDoc(rule(origin + "<AllowedMethod>get</AllowedMethod>")),
		"two in an origin": corsDoc(rule("<AllowedOrigin>*.*</AllowedOrigin>" + get)),
		"two in a header":  corsDoc(rule(origin + get + "<AllowedHeader>*-*</AllowedHeader>")),
		"an expose *":      corsDoc(rule(origin + get + "<ExposeHeader>x-*</ExposeHeader>")),
		"a long ID":        corsDoc(rule(origin + get + "<ID>" + strings.Repeat("i", 256) + "</ID>")),
		"a negative age":   corsDoc(rule(origin + get + "<MaxAgeSeconds>-1</MaxAgeSeconds>")),
		"101 rules":        corsDoc(strings.Repeat(rule(origin+get), 101)),
	} {
		_, err = ParseCORSConfiguration(doc)
		requireS3Error(t, err, http.StatusBadRequest, "InvalidRequest")
		t.Logf("%s: %v", name, err)
	}
	for name, doc := range map[string][]byte{
		"no rules":     corsDoc(),
		"no method":    corsDoc(rule(origin)),
		"no origin":    corsDoc(rule(get)),
		"a bad maxage": corsDoc(rule(origin + get + "<MaxAgeSeconds>x</MaxAgeSeconds>")),
	} {
		_, err = ParseCORSConfiguration(doc)
		requireS3Error(t, err, http.StatusBadRequest, "MalformedXML")
		t.Logf("%s: %v", name, err)
	}
	if _, err = ParseCORSConfiguration(corsDoc(strings.Repeat(rule(origin+get), 100))); err != nil {
		t.Errorf("100 rules: %v", err)
	}
}

func TestCORSMatch(t *testing.T) {
	c := &CORSConfiguration{Rules: []CORSRule{
		{
			ID: "dev", AllowedOrigins: []string{"http://localhost:3000"},
			AllowedMethods: []string{"PUT"}, AllowedHeaders: []string{"Content-Type", "x-amz-*"},
		},
		{
			ID:             "sub",
			AllowedOrigins: []string{"https://*.example.test"},
			AllowedMethods: []string{"GET"},
		},
		{ID: "any", AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET", "HEAD"}},
	}}
	for _, m := range []struct {
		origin, method, want string
		headers              []string
	}{
		{"http://localhost:3000", "PUT", "dev", []string{"content-type", "X-Amz-Date"}},
		{"http://localhost:3000", "PUT", "", []string{"authorization"}},
		{"http://localhost:3001", "PUT", "", nil},
		{"https://a.b.example.test", "GET", "sub", nil},
		{"https://example.test", "GET", "any", nil},
		{"http://anything", "HEAD", "any", nil},
		{"http://anything", "GET", "", []string{"x-custom"}},
		{"http://anything", "DELETE", "", nil},
	} {
		rule, _, ok := c.Match(m.origin, m.method, m.headers)
		got := ""
		if ok {
			got = rule.ID
		}
		if got != m.want {
			t.Errorf("%s %s %v: matched %q, want %q", m.origin, m.method, m.headers, got, m.want)
		}
	}
}

func TestParseEncryption(t *testing.T) {
	doc := func(inner string) []byte {
		return []byte("<ServerSideEncryptionConfiguration><Rule>" + inner +
			"</Rule></ServerSideEncryptionConfiguration>")
	}
	kms := "<ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm>" +
		"<KMSMasterKeyID>alias/k</KMSMasterKeyID></ApplyServerSideEncryptionByDefault>" +
		"<BucketKeyEnabled>true</BucketKeyEnabled>"
	got, err := ParseEncryption(doc(kms))
	if err != nil {
		t.Fatal(err)
	}
	if sse, key, bk := got.Default(); sse != SSEKMS || key != "alias/k" || !bk {
		t.Errorf("default %s %s %v", sse, key, bk)
	}
	_, err = ParseEncryption(doc("<ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256" +
		"</SSEAlgorithm><KMSMasterKeyID>k</KMSMasterKeyID></ApplyServerSideEncryptionByDefault>"))
	requireS3Error(t, err, http.StatusBadRequest, "InvalidArgument")
	for _, inner := range []string{
		"", "<ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:fsx</SSEAlgorithm>" +
			"</ApplyServerSideEncryptionByDefault>",
	} {
		_, err = ParseEncryption(doc(inner))
		requireS3Error(t, err, http.StatusBadRequest, "MalformedXML")
	}
}

// The default is SSE-S3 without a bucket key, and round-trips.
func TestDefaultEncryptionRoundTrips(t *testing.T) {
	b, err := Encode(NewDefaultEncryption())
	if err != nil {
		t.Fatal(err)
	}
	def, err := ParseEncryption(b)
	if err != nil {
		t.Fatal(err)
	}
	if sse, key, bk := def.Default(); sse != SSEAES256 || key != "" || bk {
		t.Errorf("default %s %s %v", sse, key, bk)
	}
}

func TestEncryptionHeaders(t *testing.T) {
	kmsDefault, err := ParseEncryption([]byte("<ServerSideEncryptionConfiguration><Rule>" +
		"<ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm>" +
		"<KMSMasterKeyID>k1</KMSMasterKeyID></ApplyServerSideEncryptionByDefault>" +
		"<BucketKeyEnabled>true</BucketKeyEnabled></Rule></ServerSideEncryptionConfiguration>"))
	if err != nil {
		t.Fatal(err)
	}
	req := func(kv ...string) *http.Request {
		r := &http.Request{Header: http.Header{}}
		for i := 0; i+1 < len(kv); i += 2 {
			r.Header.Set(kv[i], kv[i+1])
		}
		return r
	}
	for _, c := range []struct {
		def  *ServerSideEncryptionConfiguration
		r    *http.Request
		want string
	}{
		{NewDefaultEncryption(), req(), "AES256||"},
		{kmsDefault, req(), "aws:kms|k1|true"},
		{kmsDefault, req(HeaderSSE, "AES256"), "AES256||"},
		{kmsDefault, req(HeaderSSE, "aws:kms:dsse"), "aws:kms:dsse|k1|true"},
		{kmsDefault, req(HeaderSSE, "aws:kms", HeaderSSEKMSKeyID, "k2",
			HeaderSSEBucketKey, "false"), "aws:kms|k2|"},
		{NewDefaultEncryption(), req(HeaderSSE, "aws:kms"), "aws:kms||"},
	} {
		h, hErr := EncryptionHeaders(c.r, c.def)
		if hErr != nil {
			t.Fatal(hErr)
		}
		got := h[HeaderSSE] + "|" + h[HeaderSSEKMSKeyID] + "|" + h[HeaderSSEBucketKey]
		if got != c.want {
			t.Errorf("%v: got %s, want %s", c.r.Header, got, c.want)
		}
	}
	for _, r := range []*http.Request{
		req(HeaderSSE, "aws:backup"),
		req(HeaderSSEKMSKeyID, "k"),
		req(HeaderSSE, "AES256", HeaderSSEKMSKeyID, "k"),
		req(HeaderSSE, "aws:kms", HeaderSSEBucketKey, "maybe"),
	} {
		_, err = EncryptionHeaders(r, NewDefaultEncryption())
		requireS3Error(t, err, http.StatusBadRequest, "InvalidArgument")
	}
}

func TestPublicAccessBlockRoundTrips(t *testing.T) {
	b, err := Encode(NewPublicAccessBlockAll())
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParsePublicAccessBlock(b)
	if err != nil {
		t.Fatal(err)
	}
	if !got.BlockPublicAcls || !got.IgnorePublicAcls || !got.BlockPublicPolicy ||
		!got.RestrictPublicBuckets {
		t.Errorf("settings %+v", got)
	}
	// An element left out is false.
	got, err = ParsePublicAccessBlock([]byte(`<PublicAccessBlockConfiguration>` +
		`<BlockPublicPolicy>true</BlockPublicPolicy></PublicAccessBlockConfiguration>`))
	if err != nil || got.BlockPublicAcls || !got.BlockPublicPolicy {
		t.Errorf("partial settings %+v, %v", got, err)
	}
	_, err = ParsePublicAccessBlock([]byte(`<Other/>`))
	requireS3Error(t, err, http.StatusBadRequest, "MalformedXML")
}

func TestParseOwnershipControls(t *testing.T) {
	doc := func(rules ...string) []byte {
		var b strings.Builder
		b.WriteString("<OwnershipControls>")
		for _, r := range rules {
			b.WriteString("<Rule><ObjectOwnership>" + r + "</ObjectOwnership></Rule>")
		}
		b.WriteString("</OwnershipControls>")
		return []byte(b.String())
	}
	for _, o := range []string{
		OwnershipBucketOwnerEnforced, OwnershipBucketOwnerPreferred,
		OwnershipObjectWriter,
	} {
		if got, err := ParseOwnershipControls(doc(o)); err != nil || got != o {
			t.Errorf("%s: %s, %v", o, got, err)
		}
	}
	for _, d := range [][]byte{doc(), doc("Nobody"), doc(OwnershipObjectWriter,
		OwnershipObjectWriter)} {
		_, err := ParseOwnershipControls(d)
		requireS3Error(t, err, http.StatusBadRequest, "MalformedXML")
	}
	b, err := xml.Marshal(NewOwnershipControls(OwnershipObjectWriter))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseOwnershipControls(b); err != nil || got != OwnershipObjectWriter {
		t.Errorf("round trip: %s, %v", got, err)
	}
}

// The AllowedOrigin that matched is reported, the first that does, so a
// rule listing an origin and "*" answers that origin as itself.
func TestCORSMatchReportsTheAllowedOrigin(t *testing.T) {
	c := &CORSConfiguration{Rules: []CORSRule{{
		AllowedOrigins: []string{"https://a.example.test", "*"}, AllowedMethods: []string{"GET"},
	}}}
	for origin, want := range map[string]string{
		"https://a.example.test": "https://a.example.test",
		"https://b.example.test": "*",
	} {
		if _, allowed, ok := c.Match(origin, "GET", nil); !ok || allowed != want {
			t.Errorf("%s: allowed by %q, want %q", origin, allowed, want)
		}
	}
}

// A write with a customer-provided key reports it as sent, without
// x-amz-server-side-encryption or the bucket default.
func TestCustomerEncryptionHeaders(t *testing.T) {
	req := func(kv ...string) *http.Request {
		r := &http.Request{Header: http.Header{}}
		for i := 0; i+1 < len(kv); i += 2 {
			r.Header.Set(kv[i], kv[i+1])
		}
		return r
	}
	// A 256-bit key, "k" 32 times, and the base64 of its MD5.
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	digest := md5.Sum([]byte(strings.Repeat("k", 32))) //nolint:gosec // S3's key check
	sum := base64.StdEncoding.EncodeToString(digest[:])
	for _, sent := range []string{sum, ""} {
		h, err := EncryptionHeaders(req(HeaderSSECAlgorithm, "AES256", HeaderSSECKey, key,
			HeaderSSECKeyMD5, sent), NewDefaultEncryption())
		if err != nil {
			t.Fatal(err)
		}
		if len(h) != 2 || h[HeaderSSECAlgorithm] != "AES256" || h[HeaderSSECKeyMD5] != sum {
			t.Errorf("sent MD5 %q: headers %v", sent, h)
		}
	}
	_, err := EncryptionHeaders(req(HeaderSSECAlgorithm, "AES256", HeaderSSECKey, key,
		HeaderSSECKeyMD5, "bWQ1"), NewDefaultEncryption())
	requireS3Error(t, err, http.StatusBadRequest, "InvalidArgument")
	_, err = EncryptionHeaders(req(HeaderSSECAlgorithm, "DES"), NewDefaultEncryption())
	requireS3Error(t, err, http.StatusBadRequest, "InvalidEncryptionAlgorithmError")
	for _, r := range []*http.Request{
		req(HeaderSSECKey, key),
		req(HeaderSSECKeyMD5, sum),
		req(HeaderSSECAlgorithm, "AES256"),
		req(HeaderSSECAlgorithm, "AES256", HeaderSSECKey, "a2V5"),
		req(HeaderSSECAlgorithm, "AES256", HeaderSSECKey, "not base64!"),
		req(HeaderSSECAlgorithm, "AES256", HeaderSSE, "AES256"),
	} {
		_, err = EncryptionHeaders(r, NewDefaultEncryption())
		requireS3Error(t, err, http.StatusBadRequest, "InvalidArgument")
	}
}
