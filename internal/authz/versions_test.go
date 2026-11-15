package authz

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// stat is what versionedSource's StatObjectTagging answers for a ref.
type stat struct {
	info *contracts.ObjectInfo
	err  error
}

// versionedSource is buckets with objects by version: StatObjectTagging
// answers each ref objects holds, and storage.ErrObjectNotFound for any
// other.
type versionedSource struct {
	buckets
	objects map[contracts.ObjectRef]stat
}

func (v versionedSource) StatObjectTagging(
	_ context.Context,
	ref contracts.ObjectRef,
) (*contracts.ObjectInfo, error) {
	if _, ok := v.buckets[ref.Bucket]; !ok {
		return nil, storage.ErrBucketNotFound
	}
	s, ok := v.objects[ref]
	if !ok {
		return nil, storage.ErrObjectNotFound
	}
	if s.err != nil {
		return nil, s.err
	}
	info := *s.info
	info.Tags = slices.Clone(s.info.Tags)
	return &info, nil
}

func newVersionedAuthorizer(objects map[contracts.ObjectRef]stat) *Authorizer {
	return New("000000000000", versionedSource{
		buckets: buckets{"b": "", "src": "", "dst": ""}, objects: objects,
	})
}

// versionInfo is an object version of key "k", every one with the same
// ETag and modification time.
func versionInfo(id string, tags ...contracts.Tag) *contracts.ObjectInfo {
	return &contracts.ObjectInfo{
		Key: "k", ETag: "etag-k", LastModified: testModified, VersionID: id, Tags: tags,
	}
}

// wantVersionActions are the actions each operation that has version
// actions is evaluated as on targets that name a version, check by check
// in its entry's order ("Required permissions for Amazon S3 API
// operations", S3 User Guide, and the reference).
var wantVersionActions = map[Operation][]string{
	GetObject:           {"s3:GetObjectVersion"},
	HeadObject:          {"s3:GetObjectVersion"},
	CopyObject:          {"s3:PutObject", "s3:GetObjectVersion"},
	UploadPartCopy:      {"s3:PutObject", "s3:GetObjectVersion"},
	DeleteObject:        {"s3:DeleteObjectVersion"},
	DeleteObjects:       {"s3:DeleteObjectVersion"},
	GetObjectTagging:    {"s3:GetObjectVersionTagging"},
	PutObjectTagging:    {"s3:PutObjectVersionTagging"},
	DeleteObjectTagging: {"s3:DeleteObjectVersionTagging"},
	GetObjectACL:        {"s3:GetObjectVersionAcl"},
	PutObjectACL:        {"s3:PutObjectVersionAcl"},
	GetObjectAttributes: {"s3:GetObjectVersion", "s3:GetObjectVersionAttributes"},
}

// Exactly the operations wantVersionActions names have version actions,
// and those it gives.
func TestVersionActionsAreTheTables(t *testing.T) {
	for op, checks := range operations {
		has := slices.ContainsFunc(checks, func(c check) bool { return c.versionAction != "" })
		want, ok := wantVersionActions[op]
		if has != ok {
			t.Errorf("%s: has version actions %t, want %t", op, has, ok)
			continue
		}
		if ok && !slices.Equal(versionActions(op), want) {
			t.Errorf("%s: version actions %v, want %v", op, versionActions(op), want)
		}
	}
}

// versionTargets are op's targets, each on b/k, naming version id where
// its check has a version action and none elsewhere.
func versionTargets(op Operation, id string) []s3api.Target {
	targets := make([]s3api.Target, len(operations[op]))
	for i, c := range operations[op] {
		targets[i] = s3api.Target{Bucket: "b", Key: "k"}
		if c.versionAction != "" {
			targets[i].VersionID = id
		}
	}
	return targets
}

// evaluationOrder is op's checks in the order they are evaluated,
// CopyObject's source first (see evaluateCopy).
func evaluationOrder(op Operation) []check {
	checks := slices.Clone(operations[op])
	if op == CopyObject {
		slices.Reverse(checks)
	}
	return checks
}

// allowAll is an identity policy allowing actions on every resource.
func allowAll(actions []string) string {
	list := ""
	for i, a := range slices.Compact(slices.Sorted(slices.Values(actions))) {
		if i > 0 {
			list += ","
		}
		list += `"` + a + `"`
	}
	return `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":[` + list +
		`],"Resource":"*"}}`
}

// A request that names a version is evaluated as the version actions
// only: allowed by them alone, and denied, naming the first version action
// evaluated, by the actions of a request that names none; a request that
// names no version is evaluated as those actions, never the version ones.
func TestEveryVersionActionIsSelectedForAVersion(t *testing.T) {
	a := newAuthorizer(nil)
	for op := range wantVersionActions {
		versioned := versionTargets(op, "v1")
		current := versionTargets(op, "")
		var firstVersion, firstCurrent string
		for _, c := range evaluationOrder(op) {
			if c.versionAction != "" && firstVersion == "" {
				firstVersion, firstCurrent = c.versionAction, c.action
			}
		}

		onlyVersions := user(allowAll(versionActions(op)))
		if err := a.check(request(onlyVersions, "/b/k"), op, versioned...); err != nil {
			t.Errorf("%s of a version with %v allowed: %v", op, versionActions(op), err)
		}
		err := a.check(request(onlyVersions, "/b/k"), op, current...)
		requireDeniedFor(t, op, err, notAllowedMsg(firstCurrent, "arn:aws:s3:::b/k"))

		onlyCurrent := user(allowAll(Actions(op)))
		if err = a.check(request(onlyCurrent, "/b/k"), op, current...); err != nil {
			t.Errorf("%s with %v allowed: %v", op, Actions(op), err)
		}
		err = a.check(request(onlyCurrent, "/b/k"), op, versioned...)
		requireDeniedFor(t, op, err, notAllowedMsg(firstVersion, "arn:aws:s3:::b/k"))
	}
}

func requireDeniedFor(t *testing.T, op Operation, err error, message string) {
	t.Helper()
	var s3Err *s3api.Error
	if !errors.As(err, &s3Err) || s3Err.Code != "AccessDenied" || s3Err.Message != message {
		t.Errorf("%s: got %v, want AccessDenied %q", op, err, message)
	}
}

// Allows evaluates a check on a version as its version action, as
// GetObject's tag count of a version needs s3:GetObjectVersionTagging.
func TestAllowsSelectsTheVersionAction(t *testing.T) {
	a := newAuthorizer(nil)
	c, err := a.Caller(request(user(allow("s3:GetObjectVersionTagging", "*")), "/b/k"))
	if err != nil {
		t.Fatal(err)
	}
	for target, want := range map[s3api.Target]bool{
		{Bucket: "b", Key: "k", VersionID: "v1"}: true,
		{Bucket: "b", Key: "k"}:                  false,
	} {
		got, aErr := c.Allows(GetObjectTagging, target)
		if aErr != nil || got != want {
			t.Errorf("Allows(GetObjectTagging, %+v) = %t, %v, want %t", target, got, aErr, want)
		}
	}
}

// A copy under COPY of a tagged source version needs
// s3:GetObjectVersionTagging on it, as a GetObjectTagging of it would, and
// s3:PutObjectTagging, never a version action, on the destination.
func TestACopyOfAVersionsTagsNeedsTheVersionTagging(t *testing.T) {
	a := newVersionedAuthorizer(map[contracts.ObjectRef]stat{
		{Bucket: "src", Key: "k", Version: "v1"}: {
			info: versionInfo("v1", contracts.Tag{Key: "env", Value: "prod"}),
		},
	})
	dst := s3api.Target{Bucket: "dst", Key: "c"}
	src := s3api.Target{Bucket: "src", Key: "k", VersionID: "v1"}
	base := []string{"s3:GetObjectVersion", "s3:PutObject", "s3:PutObjectTagging"}
	err := a.check(request(user(allowAll(append(slices.Clone(base), "s3:GetObjectTagging"))),
		"/dst/c"), CopyObject, dst, src)
	requireDenied(t, err, notAllowedMsg("s3:GetObjectVersionTagging", "arn:aws:s3:::src/k"))
	err = a.check(request(user(allowAll(append(slices.Clone(base), "s3:GetObjectVersionTagging"))),
		"/dst/c"), CopyObject, dst, src)
	if err != nil {
		t.Errorf("copy of a tagged version with s3:GetObjectVersionTagging: %v", err)
	}
}

// denyWithVersionID is an identity policy denying action wherever the
// request has an s3:versionid.
func denyWithVersionID(action string) string {
	return `{"Version":"2012-10-17","Statement":{"Effect":"Deny","Action":"` + action +
		`","Resource":"*","Condition":{"Null":{"s3:versionid":"false"}}}}`
}

// s3:versionid is the version a request names, as given, "null" for the
// null version, a copy's source's on its source check; it is supplied for
// the version actions only, so a copy's s3:PutObject never has it.
func TestVersionIDKey(t *testing.T) {
	a := newAuthorizer(nil)
	onV1 := user(conditioned("s3:GetObjectVersion", `{"StringEquals":{"s3:versionid":"v1"}}`))
	for id, want := range map[string]bool{"v1": true, "v2": false, "null": false} {
		err := a.check(request(onV1, "/b/k"), GetObject,
			s3api.Target{Bucket: "b", Key: "k", VersionID: id})
		if (err == nil) != want {
			t.Errorf("GetObject of version %s: %v, want allowed %t", id, err, want)
		}
	}
	onNull := user(conditioned("s3:DeleteObjectVersion",
		`{"StringEquals":{"s3:versionid":"null"}}`))
	if err := a.check(request(onNull, "/b/k"), DeleteObject,
		s3api.Target{Bucket: "b", Key: "k", VersionID: contracts.VersionNull}); err != nil {
		t.Errorf("DeleteObject of the null version: %v", err)
	}

	dst := s3api.Target{Bucket: "b", Key: "c"}
	cp := user(allow("s3:PutObject", "*"), denyWithVersionID("s3:PutObject"),
		conditioned("s3:GetObjectVersion", `{"StringEquals":{"s3:versionid":"v1"}}`))
	for _, op := range []Operation{CopyObject, UploadPartCopy} {
		if err := a.check(request(cp, "/b/c"), op, dst,
			s3api.Target{Bucket: "b", Key: "k", VersionID: "v1"}); err != nil {
			t.Errorf("%s of source version v1: %v", op, err)
		}
		if err := a.check(request(cp, "/b/c"), op, dst,
			s3api.Target{Bucket: "b", Key: "k", VersionID: "v2"}); err == nil {
			t.Errorf("%s of source version v2 was allowed", op)
		}
	}

	// GetObjectAttributes' two checks both take it.
	attrs := user(allow("s3:GetObjectVersion", "*"), allow("s3:GetObjectVersionAttributes", "*"),
		denyWithVersionID("s3:GetObjectVersionAttributes"))
	err := a.check(request(attrs, "/b/k?attributes"), GetObjectAttributes,
		versionTargets(GetObjectAttributes, "v1")...)
	requireDenied(t, err, `User: `+userARN+` is not authorized to perform: `+
		`s3:GetObjectVersionAttributes on resource: "arn:aws:s3:::b/k" with an explicit deny `+
		`in an identity-based policy`)
}

// actionKeys adds s3:versionid only for an action the reference lists it
// for, and leaves the operation's keys it is given unchanged.
func TestActionKeys(t *testing.T) {
	extra := map[string]string{"k": "v"}
	v1 := s3api.Target{Bucket: "b", Key: "k", VersionID: "v1"}
	got := actionKeys(check{action: "s3:GetObjectVersion"}, v1, extra)
	if !maps.Equal(got, map[string]string{"k": "v", versionIDKey: "v1"}) {
		t.Errorf("GetObjectVersion keys %v", got)
	}
	if !maps.Equal(extra, map[string]string{"k": "v"}) {
		t.Errorf("the operation's keys were changed: %v", extra)
	}
	if got = actionKeys(check{action: "s3:PutObject"}, v1, extra); !maps.Equal(got, extra) {
		t.Errorf("PutObject keys %v", got)
	}
	if got = actionKeys(check{action: "s3:GetObjectVersion"}, s3api.Target{Bucket: "b"},
		nil); got != nil {
		t.Errorf("keys without a version %v", got)
	}
}

// s3:ExistingObjectTag is read from the version a request names, and is
// not supplied for s3:DeleteObjectVersion, which the reference does not
// list it for.
func TestExistingObjectTagIsTheVersions(t *testing.T) {
	a := newVersionedAuthorizer(map[contracts.ObjectRef]stat{
		{Bucket: "b", Key: "k", Version: "v1"}: {
			info: versionInfo("v1", contracts.Tag{Key: "env", Value: "prod"}),
		},
		{Bucket: "b", Key: "k"}: {info: versionInfo("v2", contracts.Tag{Key: "env", Value: "dev"})},
	})
	prod := `{"StringEquals":{"s3:ExistingObjectTag/env":"prod"}}`
	v1 := s3api.Target{Bucket: "b", Key: "k", VersionID: "v1"}
	if err := a.check(request(user(conditioned("s3:GetObjectVersion", prod)), "/b/k"),
		GetObject, v1); err != nil {
		t.Errorf("GetObject of the prod version: %v", err)
	}
	if err := a.check(request(user(conditioned("s3:GetObject", prod)), "/b/k"),
		GetObject, s3api.Target{Bucket: "b", Key: "k"}); err == nil {
		t.Error("GetObject of the dev current version was allowed")
	}
	for _, op := range []Operation{
		GetObjectTagging, PutObjectTagging, DeleteObjectTagging,
		GetObjectACL, PutObjectACL,
	} {
		action := versionActions(op)[0]
		if err := a.check(request(user(conditioned(action, prod)), "/b/k"), op, v1); err != nil {
			t.Errorf("%s of the prod version: %v", op, err)
		}
	}
	if err := a.check(request(user(conditioned("s3:DeleteObjectVersion", prod)), "/b/k"),
		DeleteObject, v1); err == nil {
		t.Error("s3:DeleteObjectVersion was evaluated with s3:ExistingObjectTag")
	}
}

// A decision made on one version applies to that version only: a
// different version served, even with the same ETag, modification time
// and tags, fails with ErrOperationAborted. "" and "null" are the same
// version, so a bucket whose versioning was enabled meanwhile still binds.
func TestCheckObjectBindsTheDecisionToTheVersionEvaluated(t *testing.T) {
	tags := []contracts.Tag{{Key: "env", Value: "prod"}}
	for name, tc := range map[string]struct {
		evaluated, served string
		aborted           bool
	}{
		"the same version":                 {evaluated: "v1", served: "v1"},
		"another version":                  {evaluated: "v1", served: "v2", aborted: true},
		"unversioned, then the null":       {evaluated: "", served: contracts.VersionNull},
		"the null, then unversioned":       {evaluated: contracts.VersionNull, served: ""},
		"unversioned, then a version":      {evaluated: "", served: "v1", aborted: true},
		"the null, then another version":   {evaluated: contracts.VersionNull, served: "v1", aborted: true},
		"unversioned, then unversioned":    {evaluated: "", served: ""},
		"a version, then the null version": {evaluated: "v1", served: contracts.VersionNull, aborted: true},
	} {
		a := newVersionedAuthorizer(map[contracts.ObjectRef]stat{
			{Bucket: "b", Key: "k"}: {info: versionInfo(tc.evaluated, tags...)},
		})
		target := s3api.Target{Bucket: "b", Key: "k"}
		c := evaluatedFor(t, a, target)
		served := versionInfo(tc.served, tags...)
		err := c.CheckObject(target, served)
		if aborted := errors.Is(err, contracts.ErrOperationAborted); aborted != tc.aborted ||
			!tc.aborted && err != nil {
			t.Errorf("%s: %v, want aborted %t", name, err, tc.aborted)
		}
	}

	// A version named by the request is bound to its own reads too.
	a := newVersionedAuthorizer(map[contracts.ObjectRef]stat{
		{Bucket: "b", Key: "k", Version: "v1"}: {info: versionInfo("v1", tags...)},
	})
	v1 := s3api.Target{Bucket: "b", Key: "k", VersionID: "v1"}
	c, err := a.Authorize(request(user(allow("s3:GetObjectVersion", "*")), "/b/k"), GetObject, v1)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CheckObject(v1, versionInfo("v1", tags...)); err != nil {
		t.Errorf("the version evaluated: %v", err)
	}
	if err = c.CheckObject(
		v1,
		versionInfo("v1", contracts.Tag{Key: "env", Value: "dev"}),
	); !errors.Is(
		err,
		contracts.ErrOperationAborted,
	) {
		t.Errorf("the version retagged: %v", err)
	}
	// The current version was never read for this request, so is not bound.
	if err = c.CheckObject(s3api.Target{Bucket: "b", Key: "k"}, versionInfo("v2")); err != nil {
		t.Errorf("a target not evaluated: %v", err)
	}
}

// A delete marker, a version the key does not have and a version ID this
// store does not issue are no object: they are evaluated without tags,
// the decision is bound to there being none, and the handler answers the
// error. Any other read failure fails the request.
func TestAVersionThatIsNoObject(t *testing.T) {
	a := newVersionedAuthorizer(map[contracts.ObjectRef]stat{
		{Bucket: "b", Key: "k"}: {
			err: &contracts.DeleteMarkerError{VersionID: "m", Current: true},
		},
		{Bucket: "b", Key: "k", Version: "m"}: {err: &contracts.DeleteMarkerError{VersionID: "m"}},
		{Bucket: "b", Key: "k", Version: "gone"}: {
			err: errors.Join(storage.ErrVersionNotFound, storage.ErrObjectNotFound),
		},
		{Bucket: "b", Key: "k", Version: "bad"}:  {err: contracts.ErrInvalidVersionID},
		{Bucket: "b", Key: "k", Version: "fail"}: {err: storage.ErrBackend},
	})
	p := user(allow("s3:GetObject", "*"), allow("s3:GetObjectVersion", "*"),
		conditioned("s3:GetObjectVersion", `{"Null":{"s3:ExistingObjectTag/env":"false"}}`))
	for _, id := range []string{"", "m", "gone", "bad"} {
		target := s3api.Target{Bucket: "b", Key: "k", VersionID: id}
		c, err := a.Authorize(request(p, "/b/k"), GetObject, target)
		if err != nil {
			t.Errorf("version %q: %v", id, err)
			continue
		}
		if o, ok := c.evaluated[target]; !ok || o.found || o.tags != nil {
			t.Errorf("version %q evaluated as %+v, %t", id, o, ok)
		}
		if err = c.CheckObject(target, versionInfo(id)); !errors.Is(
			err, contracts.ErrOperationAborted) {
			t.Errorf("version %q, then an object: %v", id, err)
		}
	}
	_, err := a.Authorize(request(p, "/b/k"), GetObject,
		s3api.Target{Bucket: "b", Key: "k", VersionID: "fail"})
	if !errors.Is(err, storage.ErrBackend) {
		t.Errorf("a failed read: %v, want storage.ErrBackend", err)
	}
}

// A version that is not found answers as a missing key does: NoSuchKey's
// error only to a caller with s3:ListBucket, whichever version was asked.
func TestNotFoundOfAVersionNeedsListBucket(t *testing.T) {
	a := newAuthorizer(nil)
	missing := errors.Join(storage.ErrVersionNotFound, storage.ErrObjectNotFound)
	marker := &contracts.DeleteMarkerError{VersionID: "m", Current: true}
	for _, notFound := range []error{missing, marker} {
		err := a.notFound(request(user(allow("s3:GetObjectVersion", "*")), "/b/k"), notFound)
		requireDenied(t, err, notAllowedMsg("s3:ListBucket", "arn:aws:s3:::b"))
		err = a.notFound(request(user(allow("s3:ListBucket", "*")), "/b/k"), notFound)
		if !errors.Is(err, storage.ErrObjectNotFound) {
			t.Errorf("with s3:ListBucket: %v", err)
		}
	}
}
