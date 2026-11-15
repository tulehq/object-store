//go:build e2e

package e2e

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// lifecycleRequestID is the form of a lifecycle event's x-amz-request-id,
// that of a response's.
var lifecycleRequestID = regexp.MustCompile(`^[0-9A-F]{16}$`)

// requireLifecycleEvent fails unless r is the removal event name of key in
// bucket, of versionID, "" for none, from lifecycle: principalId and
// sourceIPAddress s3.amazonaws.com, and an ID pair of its own.
func requireLifecycleEvent(t *testing.T, r *eventRecord, name, bucket, key, versionID string) {
	t.Helper()
	requireRemoved(t, r, name, key)
	requireVersion(t, r, versionID)
	requireEqual(t, r.S3.Bucket.Name, bucket, "bucket name")
	requireEqual(t, r.UserIdentity.PrincipalID, "s3.amazonaws.com", "principalId")
	requireEqual(t, r.RequestParameters.SourceIPAddress, "s3.amazonaws.com", "sourceIPAddress")
	if !lifecycleRequestID.MatchString(r.ResponseElements["x-amz-request-id"]) ||
		r.ResponseElements["x-amz-id-2"] == "" {
		t.Errorf("%s %s: responseElements %v", r.EventName, key, r.ResponseElements)
	}
}

// lifecycleQueue creates a queue for bucket and configures bucket to send
// it the events of eventTypes, returning the queue's URL.
func (s *notificationStack) lifecycleQueue(t *testing.T, bucket string,
	eventTypes ...types.Event,
) string {
	t.Helper()
	queue := "lc-" + bucket
	u := s.createQueue(t, queue, s3QueuePolicy(queueARN(queue), bucket))
	requireNoErr(t, putNotification(t, s.s3, bucket, &types.NotificationConfiguration{
		QueueConfigurations: []types.QueueConfiguration{{
			QueueArn: aws.String(queueARN(queue)), Events: eventTypes,
		}},
	}, false), "configure "+bucket)
	return u
}

// bySequencer sorts recs, events of one key, in the order they happened.
func bySequencer(recs []eventRecord) []eventRecord {
	slices.SortFunc(recs, func(a, b eventRecord) int {
		return strings.Compare(aws.ToString(a.S3.Object.Sequencer),
			aws.ToString(b.S3.Object.Sequencer))
	})
	return recs
}

// Lifecycle expirations send S3's LifecycleExpiration events, from
// s3.amazonaws.com: a Delete in an unversioned bucket; in an Enabled one,
// a DeleteMarkerCreated when the current version expires, then a Delete
// of the noncurrent version, and of the marker left alone, once they
// expire. A configuration of s3:ObjectRemoved:* gets none of them (S3 User
// Guide, "Event notification types and destinations").
//
// Lifecycle runs on the fake clock, in 2014, while the deliverer keeps the
// system clock, as the app wires it: an event's Created is the wall time
// it was recorded at, so the 24 hour age bound counts from then and never
// drops these events.
func TestLifecycleEvents(t *testing.T) {
	clk := newFakeClock(lifecycleEpoch)
	s := startNotificationStackWith(t, stackOptions{live: liveOptions{clock: clk}})
	c := s.s3

	createBucket(t, c, "unv")
	put(t, c, "unv", "logs/a b", []byte("x"))
	unv := s.lifecycleQueue(t, "unv", "s3:LifecycleExpiration:*")
	putLifecycle(t, c, "unv", enabled("exp", prefixFilter(""), days(1)))

	versionedBucket(t, c, "ver")
	v1 := putVersion(t, c, "ver", "k", "one")
	ver := s.lifecycleQueue(t, "ver", "s3:LifecycleExpiration:*")
	putLifecycle(t, c, "ver", types.LifecycleRule{
		ID: aws.String("exp"), Status: types.ExpirationStatusEnabled,
		Filter: prefixFilter(""), Expiration: days(1),
		NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{
			NoncurrentDays: aws.Int32(1),
		},
	})

	createBucket(t, c, "rem")
	put(t, c, "rem", "x/1", []byte("x"))
	put(t, c, "rem", "y/1", []byte("y"))
	rem := s.lifecycleQueue(t, "rem", "s3:ObjectRemoved:*")
	putLifecycle(t, c, "rem", enabled("exp", prefixFilter("x/"), days(1)))

	// Written 1/15 10:30 with a day's rules, they expire on 1/17 at 00:00.
	advance(t, clk, day(17))
	got := s.awaitEvents(t, unv, 1)
	requireLifecycleEvent(t, &got[0], "LifecycleExpiration:Delete", "unv", "logs/a+b", "")

	got = s.awaitEvents(t, ver, 1)
	versions := versionsOf(t, c, "ver", "k")
	i := slices.IndexFunc(versions, func(e entry) bool { return e.Marker && e.Latest })
	if len(versions) != 2 || i < 0 || versions[1-i].VersionID != v1 {
		t.Fatalf("versions of ver/k %+v, want a marker over %s", versions, v1)
	}
	marker := versions[i].VersionID
	requireLifecycleEvent(t, &got[0], "LifecycleExpiration:DeleteMarkerCreated", "ver", "k",
		marker)

	// The expiry in "rem" sends nothing: the delete after it is the only
	// event its queue receives.
	eventuallyKeys(t, c, "rem", "y/1")
	_, err := c.DeleteObject(t.Context(), &s3.DeleteObjectInput{
		Bucket: aws.String("rem"), Key: aws.String("y/1"),
	})
	requireNoErr(t, err, "delete rem/y/1")
	got = s.awaitEvents(t, rem, 1)
	requireRemoved(t, &got[0], "ObjectRemoved:Delete", "y/1")

	// v1 became noncurrent on 1/17, so it goes on 1/19, and the marker,
	// left alone, with it, each with its event, in that order.
	advance(t, clk, day(19))
	got = bySequencer(s.awaitEvents(t, ver, 2))
	requireLifecycleEvent(t, &got[0], "LifecycleExpiration:Delete", "ver", "k", v1)
	requireLifecycleEvent(t, &got[1], "LifecycleExpiration:Delete", "ver", "k", marker)
	if got[0].ResponseElements["x-amz-request-id"] == got[1].ResponseElements["x-amz-request-id"] {
		t.Fatalf("events %v share a request ID", eventsOf(got))
	}
}
