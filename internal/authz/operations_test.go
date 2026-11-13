package authz

import (
	"encoding/json"
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

// Every entry is an operation of the reference, evaluated as actions the
// reference authorizes it with, on resource types the reference lists for
// those actions, or on "*" for an action that lists none.
func TestEveryEntryMatchesTheReference(t *testing.T) {
	ref := loadReference(t)
	for op, checks := range operations {
		authorized, ok := ref.Operations[string(op)]
		if !ok {
			t.Errorf("%s is not an operation of the reference", op)
			continue
		}
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
// does not check is one whose extra actions belong to features the server
// refuses (ACLs, tagging, object lock, versions, Object Lambda).
func TestNoAuthorizedActionIsLeftOutUnexplained(t *testing.T) {
	ref := loadReference(t)
	unimplemented := []string{
		"Acl", "Tagging", "LegalHold", "Retention", "Version", "ObjectLockConfiguration",
		"OwnershipControls", "BypassGovernanceRetention",
	}
	for op := range operations {
		for _, a := range ref.Operations[string(op)] {
			if slices.Contains(Actions(op), "s3:"+a) {
				continue
			}
			if !slices.ContainsFunc(unimplemented, func(s string) bool {
				return strings.Contains(a, s)
			}) {
				t.Errorf("%s: the reference also authorizes it with s3:%s", op, a)
			}
		}
	}
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
