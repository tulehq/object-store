package authz

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

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
	// Under COPY the copy's tags are its source's, team=storage.
	if err := a.check(copyReq, CopyObject, dst, src); err == nil {
		t.Fatal("a copy's source tags were not its request tags")
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

// evaluatedFor returns a caller allowed s3:GetObject that has been
// authorized for GetObject of target.
func evaluatedFor(t *testing.T, a *Authorizer, target s3api.Target) *Caller {
	t.Helper()
	c, err := a.Caller(request(user(allow("s3:GetObject", "*")),
		"/"+target.Bucket+"/"+target.Key))
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Authorize(GetObject, target); err != nil {
		t.Fatal(err)
	}
	return c
}

// taggedInfo is the object "tagged" as the test PolicySource reports it.
func taggedInfo() *contracts.ObjectInfo {
	return &contracts.ObjectInfo{
		Key: "tagged", ETag: "etag-tagged", LastModified: testModified,
		Tags: slices.Clone(testTags["tagged"]),
	}
}

// A decision made on an object's tags applies only to that object: one
// whose ETag, modification time or tags differ from those evaluated, or
// one where there was none, fails with ErrOperationAborted.
func TestCheckObjectBindsTheDecisionToTheObjectEvaluated(t *testing.T) {
	a := newAuthorizer(nil)
	tagged := s3api.Target{Bucket: "b", Key: "tagged"}
	if err := evaluatedFor(t, a, tagged).CheckObject(tagged, taggedInfo()); err != nil {
		t.Fatalf("the object evaluated: %v", err)
	}
	if err := evaluatedFor(t, a, tagged).ObjectCheck(tagged)(taggedInfo()); err != nil {
		t.Fatalf("ObjectCheck of the object evaluated: %v", err)
	}
	for name, change := range map[string]func(*contracts.ObjectInfo){
		"another ETag": func(i *contracts.ObjectInfo) { i.ETag = "other" },
		"written again": func(i *contracts.ObjectInfo) {
			i.LastModified = i.LastModified.Add(time.Nanosecond)
		},
		"retagged": func(i *contracts.ObjectInfo) {
			i.Tags = []contracts.Tag{{Key: "env", Value: "dev"}}
		},
		"untagged": func(i *contracts.ObjectInfo) { i.Tags = nil },
		"a tag added": func(i *contracts.ObjectInfo) {
			i.Tags = append(i.Tags, contracts.Tag{Key: "x"})
		},
	} {
		info := taggedInfo()
		change(info)
		err := evaluatedFor(t, a, tagged).CheckObject(tagged, info)
		if !errors.Is(err, contracts.ErrOperationAborted) {
			t.Errorf("%s: %v, want ErrOperationAborted", name, err)
		}
	}
	// An object written where there was none when evaluated.
	missing := s3api.Target{Bucket: "b", Key: "missing"}
	err := evaluatedFor(t, a, missing).CheckObject(missing, &contracts.ObjectInfo{Key: "missing"})
	if !errors.Is(err, contracts.ErrOperationAborted) {
		t.Errorf("an object where there was none: %v", err)
	}
}

// A retagging that only reorders the object's tag set is no change; one
// that changes a value, or swaps a key, is.
func TestCheckObjectComparesTagsAsASet(t *testing.T) {
	a := newAuthorizer(nil)
	tagged := s3api.Target{Bucket: "b", Key: "tagged"}
	reordered := taggedInfo()
	slices.Reverse(reordered.Tags)
	if slices.Equal(reordered.Tags, taggedInfo().Tags) {
		t.Fatal("the test set is not reordered")
	}
	if err := evaluatedFor(t, a, tagged).CheckObject(tagged, reordered); err != nil {
		t.Errorf("the same tags in another order: %v", err)
	}
	for name, tags := range map[string][]contracts.Tag{
		"another value": {{Key: "team", Value: "storage"}, {Key: "env", Value: "dev"}},
		"a key swapped": {{Key: "team", Value: "storage"}, {Key: "owner", Value: "prod"}},
		"a key twice":   {{Key: "env", Value: "prod"}, {Key: "env", Value: "prod"}},
	} {
		info := taggedInfo()
		info.Tags = tags
		err := evaluatedFor(t, a, tagged).CheckObject(tagged, info)
		if !errors.Is(err, contracts.ErrOperationAborted) {
			t.Errorf("%s: %v, want ErrOperationAborted", name, err)
		}
	}
}

// A caller denied s3:GetObject on a copy's source is denied that, the same
// for every source, whatever its tags: they are supplied to the
// destination's checks only once its read is allowed, so a Deny on the
// destination's request tags that a source's tags would meet tells the
// caller nothing about them.
func TestACopysSourceTagsAreNotEvaluatedBeforeItsRead(t *testing.T) {
	a := newAuthorizer(nil)
	p := user(allow("s3:PutObject", "*"), allow("s3:PutObjectTagging", "*"),
		allow("s3:GetObjectTagging", "*"),
		`{"Version":"2012-10-17","Statement":{"Effect":"Deny","Action":"s3:PutObject",`+
			`"Resource":"*","Condition":{"StringEquals":{"s3:RequestObjectTag/team":"storage"}}}}`)
	dst := s3api.Target{Bucket: "dst", Key: "c"}
	for _, key := range []string{"tagged", "locked", "k"} {
		err := a.check(request(p, "/dst/c"), CopyObject, dst, s3api.Target{Bucket: "src", Key: key})
		requireDenied(t, err, notAllowedMsg("s3:GetObject", "arn:aws:s3:::src/"+key))
	}
	// Allowed the source's read, the copy of the source whose tags meet
	// the Deny is denied by it.
	reader := user(allow("s3:GetObject", "*"), allow("s3:PutObject", "*"),
		allow("s3:PutObjectTagging", "*"), allow("s3:GetObjectTagging", "*"),
		`{"Version":"2012-10-17","Statement":{"Effect":"Deny","Action":"s3:PutObject",`+
			`"Resource":"*","Condition":{"StringEquals":{"s3:RequestObjectTag/team":"storage"}}}}`)
	if err := a.check(request(reader, "/dst/c"), CopyObject, dst,
		s3api.Target{Bucket: "src", Key: "tagged"}); err == nil {
		t.Error("the Deny on the copied tags did not apply")
	}
}

// A target no tags were read for, and the root principal, which is not
// evaluated, are not checked.
func TestCheckObjectChecksOnlyWhatWasEvaluated(t *testing.T) {
	a := newAuthorizer(nil)
	tagged := s3api.Target{Bucket: "b", Key: "tagged"}
	other := &contracts.ObjectInfo{ETag: "other"}
	err := evaluatedFor(t, a, tagged).CheckObject(s3api.Target{Bucket: "b", Key: "k"}, other)
	if err != nil {
		t.Errorf("a target not evaluated: %v", err)
	}
	c, err := a.Authorize(request(rootOf("000000000000"), "/b/tagged"), GetObject, tagged)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CheckObject(tagged, other); err != nil {
		t.Errorf("root: %v", err)
	}
}

// A copy under COPY of a source with tags needs s3:GetObjectTagging on the
// source and s3:PutObjectTagging on the destination, and the source's tags
// are the destination checks' s3:RequestObjectTag keys; a source without
// tags needs neither.
func TestACopyOfTagsNeedsTagPermissions(t *testing.T) {
	a := newAuthorizer(nil)
	dst := s3api.Target{Bucket: "dst", Key: "c"}
	tagged := s3api.Target{Bucket: "src", Key: "tagged"}
	untagged := s3api.Target{Bucket: "src", Key: "k"}
	base := []string{
		allow("s3:GetObject", "arn:aws:s3:::src/*"),
		allow("s3:PutObject", "arn:aws:s3:::dst/*"),
	}
	getTags := allow("s3:GetObjectTagging", "arn:aws:s3:::src/*")
	putTags := allow("s3:PutObjectTagging", "arn:aws:s3:::dst/*")
	copyAs := func(docs ...string) error {
		return a.check(request(user(append(slices.Clone(base), docs...)...), "/dst/c"),
			CopyObject, dst, tagged)
	}

	requireDenied(t, copyAs(putTags), "User: "+userARN+" is not authorized to perform: "+
		`s3:GetObjectTagging on resource: "arn:aws:s3:::src/tagged" `+
		"because no identity-based policy allows the s3:GetObjectTagging action")
	requireDenied(t, copyAs(getTags), "User: "+userARN+" is not authorized to perform: "+
		`s3:PutObjectTagging on resource: "arn:aws:s3:::dst/c" `+
		"because no identity-based policy allows the s3:PutObjectTagging action")
	if err := copyAs(getTags, putTags); err != nil {
		t.Fatalf("with both: %v", err)
	}
	if err := a.check(request(user(base...), "/dst/c"), CopyObject, dst, untagged); err != nil {
		t.Fatalf("an untagged source: %v", err)
	}

	// The copied tags are the destination's request tags, for s3:PutObject
	// and s3:PutObjectTagging alike, and a Deny on them applies.
	deny := func(action string) string {
		return `{"Version":"2012-10-17","Statement":{"Effect":"Deny","Action":"` + action +
			`","Resource":"*","Condition":{"StringEquals":{"s3:RequestObjectTag/env":"prod"}}}}`
	}
	for _, action := range []string{"s3:PutObject", "s3:PutObjectTagging"} {
		if err := copyAs(getTags, putTags, deny(action)); err == nil {
			t.Errorf("a Deny of %s on the copied tags did not apply", action)
		}
	}
	keys := `{"Version":"2012-10-17","Statement":{"Effect":"Deny","Action":"s3:PutObject",` +
		`"Resource":"*","Condition":{"ForAnyValue:StringEquals":` +
		`{"s3:RequestObjectTagKeys":"team"}}}}`
	if err := copyAs(getTags, putTags, keys); err == nil {
		t.Error("a Deny on the copied tag keys did not apply")
	}
	// The s3:GetObjectTagging check is on the source, with its tags.
	srcDeny := `{"Version":"2012-10-17","Statement":{"Effect":"Deny",` +
		`"Action":"s3:GetObjectTagging","Resource":"*","Condition":` +
		`{"StringEquals":{"s3:ExistingObjectTag/team":"storage"}}}}`
	if err := copyAs(getTags, putTags, srcDeny); err == nil {
		t.Error("the source's s3:GetObjectTagging was not evaluated with its tags")
	}

	// Under REPLACE the request's own tags apply, as before.
	r := request(user(base...), "/dst/c")
	r.Header.Set(s3api.HeaderTaggingDirective, "REPLACE")
	if err := a.check(r, CopyObject, dst, tagged); err != nil {
		t.Fatalf("REPLACE without tags: %v", err)
	}
}
