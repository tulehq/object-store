package authz

import (
	"net/http"
	"testing"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// conditioned is an identity policy allowing action on every object of b
// under the condition cond, a JSON object.
func conditioned(action, cond string) string {
	return `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"` + action +
		`","Resource":"arn:aws:s3:::b/*","Condition":` + cond + `}}`
}

// s3:ExistingObjectTag/<key> is the stored tag of the object acted on, for
// the actions that list it, and absent for an object without it.
func TestExistingObjectTag(t *testing.T) {
	a := newAuthorizer(nil)
	p := user(conditioned("s3:GetObject",
		`{"StringEquals":{"s3:ExistingObjectTag/env":"prod"}}`))
	if err := a.check(request(p, "/b/tagged"), GetObject, s3api.Target{
		Bucket: "b",
		Key:    "tagged",
	}); err != nil {
		t.Fatalf("tagged object: %v", err)
	}
	for _, key := range []string{"k", "missing"} {
		if err := a.check(request(p, "/b/"+key), GetObject,
			s3api.Target{Bucket: "b", Key: key}); err == nil {
			t.Fatalf("%s allowed without the tag", key)
		}
	}

	// A copy's source is read as GetObject is.
	src := s3api.Target{Bucket: "b", Key: "tagged"}
	cp := user(conditioned("s3:GetObject", `{"StringEquals":{"s3:ExistingObjectTag/env":"prod"}}`),
		allow("s3:PutObject", "arn:aws:s3:::dst/*"))
	if err := a.check(request(cp, "/dst/c"), UploadPartCopy,
		s3api.Target{Bucket: "dst", Key: "c"}, src); err != nil {
		t.Fatalf("part copy of a tagged source: %v", err)
	}
}

// A write is never evaluated with the existing object's tags, not even for
// the s3:PutObjectTagging its x-amz-tagging header adds.
func TestAWriteTakesNoExistingObjectTag(t *testing.T) {
	a := newAuthorizer(nil)
	p := user(allow("s3:PutObject", "arn:aws:s3:::b/*"), conditioned("s3:PutObjectTagging",
		`{"StringEquals":{"s3:ExistingObjectTag/env":"prod"}}`))
	r := request(p, "/b/tagged")
	r.Header.Set(s3api.HeaderTagging, "env=prod")
	if err := a.check(r, PutObject, s3api.Target{Bucket: "b", Key: "tagged"}); err == nil {
		t.Fatal("a write was evaluated with the existing object's tags")
	}
	// PutObjectTagging itself is.
	put := request(p, "/b/tagged?tagging")
	if err := a.check(put, PutObjectTagging, s3api.Target{Bucket: "b", Key: "tagged"}); err != nil {
		t.Fatalf("PutObjectTagging: %v", err)
	}
}

// s3:RequestObjectTag/<key> and s3:RequestObjectTagKeys come from the
// x-amz-tagging header of a write, a copy's only under REPLACE, and from
// SetRequestTags.
func TestRequestObjectTags(t *testing.T) {
	a := newAuthorizer(nil)
	p := user(conditioned("s3:PutObject", `{"StringEquals":{"s3:RequestObjectTag/team":"a"},`+
		`"ForAllValues:StringEquals":{"s3:RequestObjectTagKeys":["team","env"]}}`),
		allow("s3:PutObjectTagging", "*"), allow("s3:GetObject", "*"))
	dst := s3api.Target{Bucket: "b", Key: "x"}
	put := func(tagging string) *http.Request {
		r := request(p, "/b/x")
		r.Header.Set(s3api.HeaderTagging, tagging)
		return r
	}
	if err := a.check(put("team=a&env=dev"), PutObject, dst); err != nil {
		t.Fatalf("allowed tags: %v", err)
	}
	if err := a.check(put("team=a&owner=me"), CreateMultipartUpload, dst); err == nil {
		t.Fatal("a key outside RequestObjectTagKeys was allowed")
	}
	if err := a.check(put("team=b"), PutObject, dst); err == nil {
		t.Fatal("another RequestObjectTag value was allowed")
	}
	if err := a.check(put("team=%zz"), PutObject, dst); err == nil {
		t.Fatal("a header that does not decode was allowed")
	}

	src := s3api.Target{Bucket: "b", Key: "tagged"}
	copyReq := put("team=b")
	if err := a.check(copyReq, CopyObject, dst, src); err == nil {
		t.Fatal("a copy's header was not read")
	}
	copyReq.Header.Set(s3api.HeaderTaggingDirective, "REPLACE")
	if err := a.check(copyReq, CopyObject, dst, src); err == nil {
		t.Fatal("a copy's REPLACE header was not read")
	}
	copyReq.Header.Set(s3api.HeaderTagging, "team=a")
	if err := a.check(copyReq, CopyObject, dst, src); err != nil {
		t.Fatalf("a copy with allowed tags: %v", err)
	}
}

// PutObjectTagging's request tags are those SetRequestTags gives.
func TestRequestObjectTagsOfADocument(t *testing.T) {
	a := newAuthorizer(nil)
	dst := s3api.Target{Bucket: "b", Key: "x"}
	tagging := user(conditioned("s3:PutObjectTagging",
		`{"StringEquals":{"s3:RequestObjectTag/team":"a"}}`))
	c, err := a.Caller(request(tagging, "/b/x?tagging"))
	if err != nil {
		t.Fatal(err)
	}
	c.SetRequestTags([]contracts.Tag{{Key: "team", Value: "a"}})
	if err = c.Authorize(PutObjectTagging, dst); err != nil {
		t.Fatalf("PutObjectTagging with allowed tags: %v", err)
	}
	c, err = a.Caller(request(tagging, "/b/x?tagging"))
	if err != nil {
		t.Fatal(err)
	}
	c.SetRequestTags([]contracts.Tag{{Key: "team", Value: "b"}})
	if err = c.Authorize(PutObjectTagging, dst); err == nil {
		t.Fatal("PutObjectTagging with other tags was allowed")
	}
}

// Allows tells an allowed check from a denied one without failing.
func TestAllows(t *testing.T) {
	a := newAuthorizer(nil)
	target := s3api.Target{Bucket: "b", Key: "k"}
	for _, tt := range []struct {
		docs []string
		want bool
	}{
		{docs: []string{allow("s3:GetObjectTagging", "*")}, want: true},
		{docs: []string{allow("s3:GetObject", "*")}, want: false},
	} {
		c, err := a.Caller(request(user(tt.docs...), "/b/k"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.Allows(GetObjectTagging, target)
		if err != nil || got != tt.want {
			t.Errorf("%v: Allows = %t, %v; want %t", tt.docs, got, err, tt.want)
		}
	}
	c, err := a.Caller(request(user(), "/b/k"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Allows(CopyObject, target); err == nil {
		t.Error("Allows took an operation of two checks")
	}
}

// A bucket policy denying s3:PutObjectAcl on an object's existing tag does
// not deny a write with an ACL over that object: "you cannot create a
// policy to grant or deny a user permissions to delete or overwrite an
// object based on its existing tags" ("Tagging and access control
// policies", S3 User Guide). The write is allowed over the tagged object
// as to a new key.
func TestAWritesACLCheckTakesNoExistingObjectTag(t *testing.T) {
	a := newAuthorizer(buckets{"b": `{"Version":"2012-10-17","Statement":{"Effect":"Deny",` +
		`"Principal":"*","Action":"s3:PutObjectAcl","Resource":"arn:aws:s3:::b/*",` +
		`"Condition":{"StringEquals":{"s3:ExistingObjectTag/locked":"true"}}}}`})
	p := user(allow("s3:PutObject", "*"), allow("s3:PutObjectAcl", "*"))
	for _, key := range []string{"locked", "new"} {
		r := request(p, "/b/"+key)
		r.Header.Set("X-Amz-Acl", "public-read")
		if err := a.check(r, PutObject, s3api.Target{Bucket: "b", Key: key}); err != nil {
			t.Errorf("a write with an ACL to %s: %v", key, err)
		}
	}
	// PutObjectAcl itself is evaluated with the tags.
	r := request(p, "/b/locked?acl")
	if err := a.check(r, PutObjectACL, s3api.Target{Bucket: "b", Key: "locked"}); err == nil {
		t.Error("PutObjectAcl of a locked object was allowed")
	}
}
