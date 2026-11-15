package notification

import (
	"slices"
	"testing"

	"tule.dev/object-store/internal/contracts"
)

func TestExpand(t *testing.T) {
	for typ, want := range map[string][]contracts.EventName{
		"s3:ObjectCreated:*": {
			"ObjectCreated:Put", "ObjectCreated:Post", "ObjectCreated:Copy",
			"ObjectCreated:CompleteMultipartUpload",
		},
		"s3:ObjectRemoved:*": {"ObjectRemoved:Delete", "ObjectRemoved:DeleteMarkerCreated"},
		"s3:ObjectRestore:*": {
			"ObjectRestore:Post", "ObjectRestore:Completed", "ObjectRestore:Delete",
		},
		"s3:Replication:*": {
			"Replication:OperationFailedReplication", "Replication:OperationNotTracked",
			"Replication:OperationMissedThreshold",
			"Replication:OperationReplicatedAfterThreshold",
		},
		"s3:LifecycleExpiration:*": {
			"LifecycleExpiration:Delete", "LifecycleExpiration:DeleteMarkerCreated",
		},
		"s3:ObjectTagging:*":             {"ObjectTagging:Put", "ObjectTagging:Delete"},
		"s3:ObjectAnnotation:*":          {"ObjectAnnotation:Put", "ObjectAnnotation:Delete"},
		"s3:ObjectCreated:Put":           {"ObjectCreated:Put"},
		"s3:LifecycleTransition":         {"LifecycleTransition"},
		"s3:ObjectAcl:Put":               {"ObjectAcl:Put"},
		"s3:ObjectRetention:Put":         {"ObjectRetention:Put"},
		"s3:ReducedRedundancyLostObject": {"ReducedRedundancyLostObject"},
		"s3:IntelligentTiering":          {"IntelligentTiering"},
		"s3:TestEvent":                   nil,
		"s3:ObjectCreated":               nil,
		"ObjectCreated:Put":              nil,
	} {
		if got := Expand(typ); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", typ, got, want)
		}
	}
}

// TestEveryRecordedEventHasAType checks that each event the storage
// records is matched by its own type and its family's wildcard.
func TestEveryRecordedEventHasAType(t *testing.T) {
	for _, name := range []contracts.EventName{
		contracts.EventObjectCreatedPut, contracts.EventObjectCreatedCopy,
		contracts.EventObjectCreatedCompleteMultipartUpload, contracts.EventObjectRemovedDelete,
		contracts.EventObjectRemovedDeleteMarkerCreated, contracts.EventLifecycleExpirationDelete,
		contracts.EventLifecycleExpirationDeleteMarkerCreated, contracts.EventObjectTaggingPut,
		contracts.EventObjectTaggingDelete,
	} {
		own := eventPrefix + string(name)
		family := own[:len(own)-len(own[lastColon(own):])] + ":*"
		if !slices.Contains(Expand(own), name) || !slices.Contains(Expand(family), name) {
			t.Errorf("%s: %v %v", name, Expand(own), Expand(family))
		}
	}
	if !slices.Contains(Expand("s3:ObjectAcl:Put"), contracts.EventObjectACLPut) {
		t.Error("ObjectAcl:Put")
	}
}

func lastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

func TestExpandReturnsACopy(t *testing.T) {
	Expand("s3:ObjectCreated:*")[0] = "changed"
	if Expand("s3:ObjectCreated:*")[0] != "ObjectCreated:Put" {
		t.Error("Expand shares its table")
	}
}
