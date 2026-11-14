package authz

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"tule.dev/object-store/internal/s3api"
)

// reference is testdata/s3-reference.json: the actions and operations of
// AWS's Service Authorization Reference for S3, trimmed to what these tests
// read. Its Source and Retrieved fields say where and when it was taken.
type reference struct {
	Actions map[string]struct {
		Resources     []string
		ConditionKeys []string
	}
	Operations map[string][]string
}

func loadReference(t *testing.T) *reference {
	t.Helper()
	raw, err := os.ReadFile("testdata/s3-reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref reference
	if err = json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	return &ref
}

var kindNames = map[resourceKind]string{bucketResource: "bucket", objectResource: "object"}

// beyondReference are the actions an operation is evaluated as that the
// reference's entry for it leaves out but its API reference names:
// GetObjectAttributes' s3:GetObjectAttributes ("you need the s3:GetObject
// and s3:GetObjectAttributes permissions").
var beyondReference = map[Operation][]string{GetObjectAttributes: {"GetObjectAttributes"}}

// Every entry is an operation of the reference, evaluated as actions the
// reference authorizes it with, or beyondReference names, on resource
// types the reference lists for those actions, or on "*" for an action
// that lists none.
func TestEveryEntryMatchesTheReference(t *testing.T) {
	ref := loadReference(t)
	for op, checks := range operations {
		authorized, ok := ref.Operations[string(op)]
		if !ok {
			t.Errorf("%s is not an operation of the reference", op)
			continue
		}
		authorized = append(slices.Clone(authorized), beyondReference[op]...)
		if len(checks) == 0 {
			t.Errorf("%s has no checks", op)
		}
		for _, c := range checks {
			checkMatchesReference(t, ref, op, c, authorized)
		}
	}
}

// checkMatchesReference checks one of op's checks against the reference,
// in which op is authorized with the actions authorized.
func checkMatchesReference(
	t *testing.T,
	ref *reference,
	op Operation,
	c check,
	authorized []string,
) {
	t.Helper()
	name, ok := strings.CutPrefix(c.action, "s3:")
	action, exists := ref.Actions[name]
	if !ok || !exists {
		t.Errorf("%s: %s is not an action of the reference", op, c.action)
		return
	}
	if !slices.Contains(authorized, name) {
		t.Errorf("%s: the reference does not authorize it with %s (it lists %v)",
			op, c.action, authorized)
	}
	if c.kind == anyResource {
		if len(action.Resources) != 0 {
			t.Errorf("%s: %s is evaluated on * but has resource types %v",
				op, c.action, action.Resources)
		}
		return
	}
	if !slices.Contains(action.Resources, kindNames[c.kind]) {
		t.Errorf("%s: %s has no %s resource type (it lists %v)",
			op, c.action, kindNames[c.kind], action.Resources)
	}
}

// Every operation the reference authorizes with one action the server
// does not check, in its entry or for its headers, is one whose extra
// actions belong to features the server refuses (object lock, versions,
// Object Lambda), the listings' s3:GetObjectAcl, which the reference lists
// for the object owners a listing can return and the server never
// returns, or GetObject's s3:GetObjectTagging, which decides only whether
// its tag count is answered (Caller.Allows).
func TestNoAuthorizedActionIsLeftOutUnexplained(t *testing.T) {
	ref := loadReference(t)
	unimplemented := []string{
		"LegalHold", "Retention", "Version", "ObjectLockConfiguration",
		"BypassGovernanceRetention",
	}
	for op := range operations {
		for _, a := range ref.Operations[string(op)] {
			if !checked(op, a) && !leftOut(op, a) && !slices.ContainsFunc(unimplemented,
				func(s string) bool { return strings.Contains(a, s) }) {
				t.Errorf("%s: the reference also authorizes it with s3:%s", op, a)
			}
		}
	}
}

// checked reports whether op is evaluated as the action a, in its entry
// or for its headers.
func checked(op Operation, a string) bool {
	return slices.Contains(Actions(op), "s3:"+a) || slices.Contains(HeaderActions(op), "s3:"+a)
}

// leftOut reports whether the reference's action a for op is one left out
// for a reason TestNoAuthorizedActionIsLeftOutUnexplained gives.
func leftOut(op Operation, a string) bool {
	return a == "GetObjectAcl" && (op == ListObjects || op == ListObjectsV2) ||
		a == "GetObjectTagging" && op == GetObject
}

// Every service key is one the reference lists for each action it is
// supplied for, and every action it is supplied for is one an entry checks.
func TestEveryServiceKeyMatchesTheReference(t *testing.T) {
	ref := loadReference(t)
	var used []string
	for _, checks := range operations {
		for _, c := range checks {
			used = append(used, c.action)
		}
	}
	for _, k := range serviceKeys {
		actions := k.actions
		if actions == nil {
			actions = used
		}
		for _, a := range actions {
			if !slices.Contains(used, a) {
				t.Errorf("%s is supplied for %s, which no entry checks", k.name, a)
			}
			if !slices.Contains(ref.Actions[strings.TrimPrefix(a, "s3:")].ConditionKeys, k.name) {
				t.Errorf("the reference does not list %s for %s", k.name, a)
			}
		}
	}
}

// The operation-derived keys are ones the reference lists for their action.
func TestOperationKeysMatchTheReference(t *testing.T) {
	ref := loadReference(t)
	for op := range operations {
		targets := make([]s3api.Target, len(operations[op]))
		for i := range targets {
			targets[i] = s3api.Target{Bucket: "b", Key: "k"}
		}
		for action, ks := range operationKeys(op, targets) {
			if !slices.Contains(Actions(op), action) {
				t.Errorf("%s does not check %s", op, action)
			}
			for k := range ks {
				if !slices.Contains(
					ref.Actions[strings.TrimPrefix(action, "s3:")].ConditionKeys,
					k,
				) {
					t.Errorf("the reference does not list %s for %s", k, action)
				}
			}
		}
	}
	keys := operationKeys(CopyObject, []s3api.Target{{}, {Bucket: "b", Key: "k"}})
	if keys["s3:PutObject"][copySourceKey] != "b/k" {
		t.Errorf("copy source = %q", keys["s3:PutObject"][copySourceKey])
	}
}

// Every action a write's ACL or Object Ownership headers add is one the
// reference authorizes the operation with, on a resource type it lists for
// that action.
func TestEveryHeaderCheckMatchesTheReference(t *testing.T) {
	ref := loadReference(t)
	r := &http.Request{Header: http.Header{}}
	r.Header.Set("X-Amz-Acl", "public-read")
	r.Header.Set("X-Amz-Object-Ownership", "ObjectWriter")
	r.Header.Set(s3api.HeaderTagging, "k=v")
	r.Header.Set(s3api.HeaderTaggingDirective, "REPLACE")
	added := 0
	for op := range operations {
		for _, c := range headerChecks(op, r) {
			added++
			checkMatchesReference(t, ref, op, c, ref.Operations[string(op)])
		}
	}
	if added != 8 {
		t.Errorf("%d header checks, want CreateBucket's two and two for each object write",
			added)
	}

	private := &http.Request{Header: http.Header{"X-Amz-Acl": {"private"}}}
	if checks := headerChecks(CreateBucket, private); len(checks) != 0 {
		t.Errorf("CreateBucket with the private ACL adds %v", checks)
	}
	full := &http.Request{Header: http.Header{"X-Amz-Acl": {"bucket-owner-full-control"}}}
	for _, r := range []*http.Request{private, full} {
		for _, op := range []Operation{PutObject, CopyObject, CreateMultipartUpload} {
			if checks := headerChecks(op, r); len(checks) != 0 {
				t.Errorf("%s with %v adds %v", op, r.Header, checks)
			}
		}
	}
	grant := &http.Request{Header: http.Header{"X-Amz-Grant-Read": {`id="x"`}}}
	if checks := headerChecks(PutObject, grant); len(checks) != 1 {
		t.Errorf("PutObject with a grant header adds %v", checks)
	}
}

// Every check an object write's headers add is made on the object it
// replaces, so takes no s3:ExistingObjectTag keys.
func TestAWritesHeaderChecksAreOnWrite(t *testing.T) {
	r := &http.Request{Header: http.Header{}}
	r.Header.Set("X-Amz-Acl", "public-read")
	r.Header.Set(s3api.HeaderTagging, "k=v")
	r.Header.Set(s3api.HeaderTaggingDirective, "REPLACE")
	for _, op := range []Operation{PutObject, CopyObject, CreateMultipartUpload} {
		checks := headerChecks(op, r)
		if len(checks) != 2 {
			t.Errorf("%s adds %v", op, checks)
		}
		for _, c := range checks {
			if !c.onWrite {
				t.Errorf("%s: %s is not marked onWrite", op, c.action)
			}
		}
	}
}

// x-amz-tagging adds s3:PutObjectTagging to a write, and to a copy only
// under REPLACE, where its header is read.
func TestTheTaggingHeaderAddsPutObjectTagging(t *testing.T) {
	tagged := &http.Request{Header: http.Header{"X-Amz-Tagging": {"k=v"}}}
	if checks := headerChecks(CopyObject, tagged); len(checks) != 0 {
		t.Errorf("CopyObject with x-amz-tagging under COPY adds %v", checks)
	}
	for _, op := range []Operation{PutObject, CreateMultipartUpload} {
		if checks := headerChecks(op, tagged); len(checks) != 1 || !checks[0].onWrite {
			t.Errorf("%s with x-amz-tagging adds %v", op, checks)
		}
	}
	tagged.Header.Set("X-Amz-Tagging-Directive", "REPLACE")
	if checks := headerChecks(CopyObject, tagged); len(checks) != 1 {
		t.Errorf("CopyObject with x-amz-tagging under REPLACE adds %v", checks)
	}
}

// The object tag keys are supplied for actions the reference lists them
// for, and every such action an entry checks is supplied them.
func TestTagKeysMatchTheReference(t *testing.T) {
	ref := loadReference(t)
	checked := map[string]bool{}
	for op := range operations {
		for _, a := range Actions(op) {
			checked[a] = true
		}
		for _, a := range HeaderActions(op) {
			checked[a] = true
		}
	}
	for keys, actions := range map[[2]string][]string{
		{"s3:ExistingObjectTag/<key>", ""}:                       existingTagActions,
		{"s3:RequestObjectTag/<key>", "s3:RequestObjectTagKeys"}: requestTagActions,
	} {
		for a := range checked {
			listed := slices.Contains(
				ref.Actions[strings.TrimPrefix(a, "s3:")].ConditionKeys, keys[0])
			if listed != slices.Contains(actions, a) {
				t.Errorf("%s: the reference lists %s %t, supplied %t", a, keys[0], listed, !listed)
			}
			if keys[1] != "" && listed != slices.Contains(
				ref.Actions[strings.TrimPrefix(a, "s3:")].ConditionKeys, keys[1]) {
				t.Errorf("%s: the reference does not list %s with %s", a, keys[1], keys[0])
			}
		}
	}
}
