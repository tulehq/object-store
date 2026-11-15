//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go/middleware"

	"tule.dev/object-store/internal/services/notify"
)

// eventRecord is a record of an event message, as the S3 User Guide's
// "Event message structure" gives it. The object's optional fields are
// pointers, so a test tells a field left out from a zero one.
type eventRecord struct {
	EventVersion string `json:"eventVersion"`
	EventSource  string `json:"eventSource"`
	AWSRegion    string `json:"awsRegion"`
	EventTime    string `json:"eventTime"`
	EventName    string `json:"eventName"`
	UserIdentity struct {
		PrincipalID string `json:"principalId"`
	} `json:"userIdentity"`
	RequestParameters struct {
		SourceIPAddress string `json:"sourceIPAddress"`
	} `json:"requestParameters"`
	ResponseElements map[string]string `json:"responseElements"`
	S3               struct {
		SchemaVersion   string `json:"s3SchemaVersion"`
		ConfigurationID string `json:"configurationId"`
		Bucket          struct {
			Name          string `json:"name"`
			OwnerIdentity struct {
				PrincipalID string `json:"principalId"`
			} `json:"ownerIdentity"`
			ARN string `json:"arn"`
		} `json:"bucket"`
		Object struct {
			Size                *int64  `json:"size"`
			ETag                *string `json:"eTag"`
			VersionID           *string `json:"versionId"`
			Sequencer           *string `json:"sequencer"`
			HasObjectAnnotation *bool   `json:"hasObjectAnnotation"`
			Key                 string  `json:"key"`
		} `json:"object"`
	} `json:"s3"`
}

// eventWait bounds how long a test waits for the events it expects.
const eventWait = 30 * time.Second

// isTestEvent reports whether body is an s3:TestEvent, which validation
// sends.
func isTestEvent(body string) bool {
	var ev struct{ Event string }
	return json.Unmarshal([]byte(body), &ev) == nil && ev.Event == "s3:TestEvent"
}

// receiveEvents receives from queueURL for up to wait, deleting what it
// receives, until n event messages arrived, and returns their records in
// the order they arrived. Test events are skipped. Every message must be
// sent by s3.amazonaws.com and hold one record.
func (s *notificationStack) receiveEvents(
	t *testing.T, queueURL string, n int, wait time.Duration,
) []eventRecord {
	t.Helper()
	var out []eventRecord
	deadline := time.Now().Add(wait)
	for len(out) < n && time.Now().Before(deadline) {
		res, err := s.sqs.ReceiveMessage(t.Context(), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
			MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{
				sqstypes.MessageSystemAttributeNameAll,
			},
		})
		requireNoErr(t, err, "receive from "+queueURL)
		for _, m := range res.Messages {
			_, err = s.sqs.DeleteMessage(t.Context(), &sqs.DeleteMessageInput{
				QueueUrl: aws.String(queueURL), ReceiptHandle: m.ReceiptHandle,
			})
			requireNoErr(t, err, "delete a message")
			body := aws.ToString(m.Body)
			if isTestEvent(body) {
				continue
			}
			requireEqual(t, m.Attributes[string(sqstypes.MessageSystemAttributeNameSenderId)],
				"s3.amazonaws.com", "SenderId")
			var msg struct{ Records []eventRecord }
			requireNoErr(t, json.Unmarshal([]byte(body), &msg), "decode "+body)
			requireEqual(t, len(msg.Records), 1, "records in "+body)
			out = append(out, msg.Records[0])
		}
	}
	return out
}

// awaitEvents is receiveEvents that fails unless exactly n arrive within
// eventWait, and no other within a second after.
func (s *notificationStack) awaitEvents(t *testing.T, queueURL string, n int) []eventRecord {
	t.Helper()
	recs := s.receiveEvents(t, queueURL, n, eventWait)
	if len(recs) != n {
		t.Fatalf("%d events arrived, want %d: %+v", len(recs), n, eventsOf(recs))
	}
	if more := s.receiveEvents(t, queueURL, 1, time.Second); len(more) > 0 {
		t.Fatalf("an unexpected event arrived: %s %s", more[0].EventName, more[0].S3.Object.Key)
	}
	return recs
}

// eventsOf returns the event name and key of each of recs.
func eventsOf(recs []eventRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.EventName + " " + r.S3.Object.Key
	}
	return out
}

// eventsQueue is the queue the delivery tests configure.
const eventsQueue = "events"

// queueConf is a queue configuration id of eventsQueue for events, with
// the key filter rules.
func queueConf(
	id string,
	events []types.Event,
	rules ...types.FilterRule,
) types.QueueConfiguration {
	c := types.QueueConfiguration{
		Id: aws.String(id), QueueArn: aws.String(queueARN(eventsQueue)), Events: events,
	}
	if len(rules) > 0 {
		c.Filter = keyFilter(rules...)
	}
	return c
}

// createdAndRemoved are the created and removed events.
var createdAndRemoved = []types.Event{"s3:ObjectCreated:*", "s3:ObjectRemoved:*"}

// queueFor creates eventsQueue, whose policy lets S3 send to it from
// bucket, configures bucket with cfgs, which validation sends the queue a
// test event for, and returns the queue's URL.
func (s *notificationStack) queueFor(
	t *testing.T, bucket string, cfgs ...types.QueueConfiguration,
) string {
	t.Helper()
	u := s.createQueue(t, eventsQueue, s3QueuePolicy(queueARN(eventsQueue), bucket))
	requireNoErr(t, putNotification(t, s.s3, bucket, &types.NotificationConfiguration{
		QueueConfigurations: cfgs,
	}, false), "configure "+bucket)
	return u
}

// sdkRequestIDs returns the x-amz-request-id and x-amz-id-2 of an SDK
// response's metadata.
func sdkRequestIDs(t *testing.T, md middleware.Metadata) (string, string) {
	t.Helper()
	id, ok := awsmiddleware.GetRequestIDMetadata(md)
	host, hok := s3.GetHostIDMetadata(md)
	if !ok || !hok || id == "" || host == "" {
		t.Fatalf("a response without its request IDs: %q %q", id, host)
	}
	return id, host
}

// requireCommon checks the fields every event message of the root key's
// requests to bucket through configuration config has, and that it answers
// the request requestID, hostID: the event source is the root user's
// account and the address the request came from.
func requireCommon(t *testing.T, r *eventRecord, bucket, config, requestID, hostID string) {
	t.Helper()
	requireEqual(t, r.EventVersion, "2.6", "eventVersion")
	requireEqual(t, r.EventSource, "aws:s3", "eventSource")
	requireEqual(t, r.AWSRegion, "us-east-1", "awsRegion")
	if !eventTime.MatchString(r.EventTime) {
		t.Errorf("eventTime %q is not the event time format", r.EventTime)
	}
	requireEqual(t, r.UserIdentity.PrincipalID, account, "principalId")
	requireEqual(t, r.RequestParameters.SourceIPAddress, "127.0.0.1", "sourceIPAddress")
	requireEqual(t, r.ResponseElements["x-amz-request-id"], requestID, "x-amz-request-id")
	requireEqual(t, r.ResponseElements["x-amz-id-2"], hostID, "x-amz-id-2")
	requireEqual(t, r.S3.SchemaVersion, "1.0", "s3SchemaVersion")
	requireEqual(t, r.S3.ConfigurationID, config, "configurationId")
	requireEqual(t, r.S3.Bucket.Name, bucket, "bucket name")
	requireEqual(t, r.S3.Bucket.ARN, "arn:aws:s3:::"+bucket, "bucket arn")
	if r.S3.Bucket.OwnerIdentity.PrincipalID == "" {
		t.Error("no ownerIdentity principalId")
	}
}

// requireCreated checks r is the created event name of key, of an object
// of size bytes and etag, quoted or not, with a sequencer.
func requireCreated(t *testing.T, r *eventRecord, name, key string, size int64, etag string) {
	t.Helper()
	requireEqual(t, r.EventName, name, "eventName of "+key)
	requireEqual(t, r.S3.Object.Key, key, "key")
	if r.S3.Object.Size == nil || *r.S3.Object.Size != size {
		t.Errorf("%s: size %v, want %d", key, r.S3.Object.Size, size)
	}
	if r.S3.Object.ETag == nil || *r.S3.Object.ETag != strings.Trim(etag, `"`) {
		t.Errorf("%s: eTag %v, want %s", key, r.S3.Object.ETag, etag)
	}
	requireSequencer(t, r)
}

// requireRemoved checks r is the removed event name of key: a sequencer,
// and no size or eTag.
func requireRemoved(t *testing.T, r *eventRecord, name, key string) {
	t.Helper()
	requireEqual(t, r.EventName, name, "eventName of "+key)
	requireEqual(t, r.S3.Object.Key, key, "key")
	if r.S3.Object.Size != nil || r.S3.Object.ETag != nil {
		t.Errorf("%s: a removal with size %v, eTag %v", key, r.S3.Object.Size, r.S3.Object.ETag)
	}
	requireSequencer(t, r)
}

// requireSequencer checks r has a sequencer of 16 upper-case hex digits.
func requireSequencer(t *testing.T, r *eventRecord) {
	t.Helper()
	seq := r.S3.Object.Sequencer
	if seq == nil || len(*seq) != 16 || strings.ToUpper(*seq) != *seq {
		t.Errorf("%s: sequencer %v", r.S3.Object.Key, seq)
	}
}

// requireVersion checks r's versionId: want, or left out for "".
func requireVersion(t *testing.T, r *eventRecord, want string) {
	t.Helper()
	got := r.S3.Object.VersionID
	switch {
	case want == "" && got != nil:
		t.Errorf("%s %s: versionId %q in an Unversioned bucket", r.EventName, r.S3.Object.Key, *got)
	case want != "" && (got == nil || *got != want):
		t.Errorf("%s %s: versionId %v, want %s", r.EventName, r.S3.Object.Key, got, want)
	}
}

// Every write and removal of an Unversioned bucket sends its event to the
// queue, with the fields the S3 User Guide gives, the key URL-encoded, and
// the request IDs of the response that caused it; a refused write sends
// none.
func TestQueueDeliveryCreatedAndRemoved(t *testing.T) {
	s := startNotificationStack(t)
	createBucket(t, s.s3, notifyBucket)
	q := s.queueFor(t, notifyBucket, queueConf("all", createdAndRemoved))
	c := writeAndRemove(t, s)

	recs := s.awaitEvents(t, q, 8)
	// The two keys of one DeleteObjects are in no documented order.
	slices.SortFunc(recs[6:], func(a, b eventRecord) int {
		return strings.Compare(a.S3.Object.Key, b.S3.Object.Key)
	})
	reqOf := []int{0, 1, 2, 3, 4, 5, 6, 6}
	for i := range recs {
		ids := c.ids[reqOf[i]]
		requireCommon(t, &recs[i], notifyBucket, "all", ids[0], ids[1])
		requireVersion(t, &recs[i], "")
	}
	requireCreated(t, &recs[0], "ObjectCreated:Put", "put.txt", 5, c.etags[0])
	requireCreated(t, &recs[1], "ObjectCreated:Put", "presigned.txt", 14, c.etags[1])
	requireCreated(t, &recs[2], "ObjectCreated:Copy", "copy.txt", 5, c.etags[2])
	requireCreated(t, &recs[3], "ObjectCreated:CompleteMultipartUpload", "mp.bin", c.size,
		c.etags[3])
	if !strings.HasSuffix(aws.ToString(recs[3].S3.Object.ETag), "-2") {
		t.Errorf("the multipart eTag %v is not of two parts", recs[3].S3.Object.ETag)
	}
	requireCreated(t, &recs[4], "ObjectCreated:Put", "a+b/c%2Bd%3De.txt", 7, c.etags[4])
	requireRemoved(t, &recs[5], "ObjectRemoved:Delete", "put.txt")
	requireRemoved(t, &recs[6], "ObjectRemoved:Delete", "copy.txt")
	requireRemoved(t, &recs[7], "ObjectRemoved:Delete", "presigned.txt")
	for i, r := range recs {
		a := r.S3.Object.HasObjectAnnotation
		if (i == 2) != (a != nil) || (a != nil && *a) {
			t.Errorf("%s: hasObjectAnnotation %v", r.EventName, a)
		}
	}
}

// changes are what writeAndRemove did: the x-amz-request-id and x-amz-id-2
// of each request that causes events, the ETags of the five objects it
// created, and the size of the multipart one.
type changes struct {
	ids   [][2]string
	etags []string
	size  int64
}

// writeAndRemove, in notifyBucket: puts put.txt, puts presigned.txt
// through a presigned URL, copies put.txt to copy.txt, completes mp.bin
// with a copied part, puts "a b/c+d=e.txt", fails a conditional put of
// put.txt, deletes put.txt, and deletes copy.txt and presigned.txt in one
// DeleteObjects.
func writeAndRemove(t *testing.T, s *notificationStack) changes {
	t.Helper()
	ctx := t.Context()
	var c changes
	note := func(md middleware.Metadata, etag *string) {
		r, h := sdkRequestIDs(t, md)
		c.ids = append(c.ids, [2]string{r, h})
		if etag != nil {
			c.etags = append(c.etags, aws.ToString(etag))
		}
	}
	put, err := s.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(notifyBucket), Key: aws.String("put.txt"),
		Body: strings.NewReader("hello"),
	})
	requireNoErr(t, err, "put")
	note(put.ResultMetadata, put.ETag)
	presigned := presignedPut(t, s, "presigned.txt", "presigned body")
	c.ids = append(c.ids, [2]string{presigned.Get("x-amz-request-id"), presigned.Get("x-amz-id-2")})
	c.etags = append(c.etags, presigned.Get("ETag"))
	cp, err := s.s3.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(notifyBucket), Key: aws.String("copy.txt"),
		CopySource: aws.String(notifyBucket + "/put.txt"),
	})
	requireNoErr(t, err, "copy")
	note(cp.ResultMetadata, cp.CopyObjectResult.ETag)
	done, size := completeWithCopiedPart(t, s.s3, "mp.bin")
	note(done.ResultMetadata, done.ETag)
	c.size = size
	enc, err := s.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(notifyBucket), Key: aws.String("a b/c+d=e.txt"),
		Body: strings.NewReader("encoded"),
	})
	requireNoErr(t, err, "put a b/c+d=e.txt")
	note(enc.ResultMetadata, enc.ETag)
	_, err = s.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(notifyBucket), Key: aws.String("put.txt"),
		Body: strings.NewReader("again"), IfNoneMatch: aws.String("*"),
	})
	requireResponse(t, err, http.StatusPreconditionFailed)
	del, err := s.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(notifyBucket), Key: aws.String("put.txt"),
	})
	requireNoErr(t, err, "delete")
	note(del.ResultMetadata, nil)
	dels, err := s.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(notifyBucket), Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("copy.txt")}, {Key: aws.String("presigned.txt")},
		}},
	})
	requireNoErr(t, err, "delete objects")
	note(dels.ResultMetadata, nil)
	return c
}

// presignedPut PUTs body at key of notifyBucket through a presigned URL
// and returns the response's header.
func presignedPut(t *testing.T, s *notificationStack, key, body string) http.Header {
	t.Helper()
	pre, err := s3.NewPresignClient(s.s3).PresignPutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(notifyBucket), Key: aws.String(key),
	})
	requireNoErr(t, err, "presign")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, pre.URL,
		strings.NewReader(body))
	requireNoErr(t, err, "build the presigned PUT")
	for k, v := range pre.SignedHeader {
		req.Header[k] = v
	}
	res, err := (&http.Client{Timeout: time.Minute}).Do(req)
	requireNoErr(t, err, "presigned PUT")
	_ = res.Body.Close()
	requireEqual(t, res.StatusCode, http.StatusOK, "presigned PUT status")
	return res.Header
}

// completeWithCopiedPart uploads key of notifyBucket in two parts, the
// second copied from put.txt with UploadPartCopy, and returns the
// completion and the object's size.
func completeWithCopiedPart(
	t *testing.T, c *s3.Client, key string,
) (*s3.CompleteMultipartUploadOutput, int64) {
	t.Helper()
	ctx := t.Context()
	b, k := aws.String(notifyBucket), aws.String(key)
	up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: b, Key: k})
	requireNoErr(t, err, "create the upload")
	first := bytes.Repeat([]byte("p"), 5<<20)
	p1, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: b, Key: k, UploadId: up.UploadId, PartNumber: aws.Int32(1),
		Body: bytes.NewReader(first),
	})
	requireNoErr(t, err, "upload part 1")
	p2, err := c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
		Bucket: b, Key: k, UploadId: up.UploadId, PartNumber: aws.Int32(2),
		CopySource: aws.String(notifyBucket + "/put.txt"),
	})
	requireNoErr(t, err, "upload part 2 by copy")
	done, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: b, Key: k, UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: p1.ETag},
			{PartNumber: aws.Int32(2), ETag: p2.CopyPartResult.ETag},
		}},
	})
	requireNoErr(t, err, "complete the upload")
	return done, int64(len(first)) + 5
}

// putKeys writes each of keys into bucket with c.
func putKeys(t *testing.T, c *s3.Client, bucket string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		_, err := c.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(k), Body: strings.NewReader("x"),
		})
		requireNoErr(t, err, "put "+k)
	}
}

// Prefix, suffix and both filter events, on the URL-encoded key, a space
// written "+" in the filter; keys no filter matches send nothing; and one
// prefix serves different event types in two configurations ("Configuring
// event notifications using object key name filtering", S3 User Guide).
func TestNotificationFilters(t *testing.T) {
	s := startNotificationStack(t)
	createBucket(t, s.s3, notifyBucket)
	created := []types.Event{"s3:ObjectCreated:*"}
	q := s.queueFor(t, notifyBucket,
		queueConf("images", created,
			filterRule("prefix", "images/"), filterRule("suffix", ".jpg")),
		queueConf("docs", created,
			filterRule("prefix", "my+docs/"), filterRule("suffix", ".md")),
		queueConf("logs", created,
			filterRule("prefix", "logs/"), filterRule("suffix", ".log")),
		queueConf("csv", created, filterRule("suffix", ".csv")),
		queueConf("logs-removed", []types.Event{"s3:ObjectRemoved:*"},
			filterRule("prefix", "logs/")),
	)
	putKeys(t, s.s3, notifyBucket, "images/a.jpg", "images/a.png", "docs/a.jpg",
		"my docs/r.md", "mydocs/r.md", "logs/x.log", "logs/x.txt", "data/t.csv", "t.csv.gz")
	for _, k := range []string{"logs/x.log", "images/a.jpg"} {
		_, err := s.s3.DeleteObject(t.Context(), &s3.DeleteObjectInput{
			Bucket: aws.String(notifyBucket), Key: aws.String(k),
		})
		requireNoErr(t, err, "delete "+k)
	}
	recs := s.awaitEvents(t, q, 5)
	got := make([]string, 0, len(recs))
	for _, r := range recs {
		got = append(got, r.S3.ConfigurationID+" "+r.EventName+" "+r.S3.Object.Key)
	}
	want := []string{
		"images ObjectCreated:Put images/a.jpg",
		"docs ObjectCreated:Put my+docs/r.md",
		"logs ObjectCreated:Put logs/x.log",
		"csv ObjectCreated:Put data/t.csv",
		"logs-removed ObjectRemoved:Delete logs/x.log",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// In an Enabled bucket a write's event carries its version ID, a delete
// without one DeleteMarkerCreated with the marker's, and a delete of a
// version Delete with that version's; Suspended, a write's is "null". The
// sequencers of one key's concurrent writes follow its versions' order.
func TestVersionedEvents(t *testing.T) {
	s := startNotificationStack(t)
	ctx := t.Context()
	const bucket = "versioned"
	createBucket(t, s.s3, bucket)
	setVersioning := func(st types.BucketVersioningStatus) {
		_, err := s.s3.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
			Bucket: aws.String(bucket), VersioningConfiguration: &types.VersioningConfiguration{
				Status: st,
			},
		})
		requireNoErr(t, err, "set versioning "+string(st))
	}
	setVersioning(types.BucketVersioningStatusEnabled)
	q := s.queueFor(t, bucket, queueConf("all", createdAndRemoved))

	put, err := s.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("k"), Body: strings.NewReader("v1"),
	})
	requireNoErr(t, err, "put k")
	marker, err := s.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("k"),
	})
	requireNoErr(t, err, "delete k")
	_, err = s.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("k"), VersionId: put.VersionId,
	})
	requireNoErr(t, err, "delete k's version")
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			_, pErr := s.s3.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String(bucket), Key: aws.String("c"), Body: strings.NewReader("c"),
			})
			if pErr != nil {
				t.Errorf("put c: %v", pErr)
			}
		})
	}
	wg.Wait()
	setVersioning(types.BucketVersioningStatusSuspended)
	putKeys(t, s.s3, bucket, "s")

	recs := s.awaitEvents(t, q, 14)
	requireCreated(t, &recs[0], "ObjectCreated:Put", "k", 2, aws.ToString(put.ETag))
	requireVersion(t, &recs[0], aws.ToString(put.VersionId))
	requireRemoved(t, &recs[1], "ObjectRemoved:DeleteMarkerCreated", "k")
	requireVersion(t, &recs[1], aws.ToString(marker.VersionId))
	requireRemoved(t, &recs[2], "ObjectRemoved:Delete", "k")
	requireVersion(t, &recs[2], aws.ToString(put.VersionId))
	requireEqual(t, recs[13].S3.Object.Key, "s", "the Suspended write's key")
	requireVersion(t, &recs[13], "null")
	requireConcurrentSequencers(t, s.s3, bucket, recs[3:13])
}

// requireConcurrentSequencers checks recs, the events of the concurrent
// writes of "c", arrived with increasing sequencers, in the order of c's
// versions.
func requireConcurrentSequencers(t *testing.T, c *s3.Client, bucket string, recs []eventRecord) {
	t.Helper()
	seqs, ids := make([]string, 0, len(recs)), make([]string, 0, len(recs))
	for i := range recs {
		requireEqual(t, recs[i].S3.Object.Key, "c", "a concurrent write's key")
		requireSequencer(t, &recs[i])
		seqs = append(seqs, aws.ToString(recs[i].S3.Object.Sequencer))
		ids = append(ids, aws.ToString(recs[i].S3.Object.VersionID))
	}
	if !slices.IsSorted(seqs) || len(slices.Compact(slices.Clone(seqs))) != len(seqs) {
		t.Fatalf("sequencers do not increase in arrival order: %v", seqs)
	}
	out, err := c.ListObjectVersions(t.Context(), &s3.ListObjectVersionsInput{
		Bucket: aws.String(bucket), Prefix: aws.String("c"),
	})
	requireNoErr(t, err, "list c's versions")
	var versions []string
	for _, v := range out.Versions {
		versions = append([]string{aws.ToString(v.VersionId)}, versions...)
	}
	if !slices.Equal(ids, versions) {
		t.Fatalf("events in the order of versions %v, want %v (oldest first)", ids, versions)
	}
}

// PutObjectTagging and DeleteObjectTagging send their events, and
// PutObjectAcl sends one only when it changes the object's ACL.
func TestTaggingAndACLEvents(t *testing.T) {
	s := startNotificationStack(t)
	ctx := t.Context()
	b := aws.String(notifyBucket)
	_, err := s.s3.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: b, ObjectOwnership: types.ObjectOwnershipObjectWriter,
	})
	requireNoErr(t, err, "create an ObjectWriter bucket")
	_, err = s.s3.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{Bucket: b})
	requireNoErr(t, err, "delete the public access block")
	putKeys(t, s.s3, notifyBucket, "k")
	q := s.queueFor(t, notifyBucket, queueConf("meta",
		[]types.Event{"s3:ObjectTagging:*", "s3:ObjectAcl:Put"}))

	k := aws.String("k")
	tag, err := s.s3.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: b, Key: k, Tagging: &types.Tagging{TagSet: []types.Tag{
			{Key: aws.String("a"), Value: aws.String("1")},
		}},
	})
	requireNoErr(t, err, "put tagging")
	untag, err := s.s3.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{Bucket: b, Key: k})
	requireNoErr(t, err, "delete tagging")
	// The object's ACL is private already, so this changes nothing.
	_, err = s.s3.PutObjectAcl(ctx, &s3.PutObjectAclInput{
		Bucket: b, Key: k, ACL: types.ObjectCannedACLPrivate,
	})
	requireNoErr(t, err, "put the private ACL")
	acl, err := s.s3.PutObjectAcl(ctx, &s3.PutObjectAclInput{
		Bucket: b, Key: k, ACL: types.ObjectCannedACLPublicRead,
	})
	requireNoErr(t, err, "put public-read")
	_, err = s.s3.PutObjectAcl(ctx, &s3.PutObjectAclInput{
		Bucket: b, Key: k, ACL: types.ObjectCannedACLPublicRead,
	})
	requireNoErr(t, err, "put public-read again")

	recs := s.awaitEvents(t, q, 3)
	for i, want := range []struct {
		md   middleware.Metadata
		name string
	}{
		{tag.ResultMetadata, "ObjectTagging:Put"},
		{untag.ResultMetadata, "ObjectTagging:Delete"},
		{acl.ResultMetadata, "ObjectAcl:Put"},
	} {
		r := &recs[i]
		req, host := sdkRequestIDs(t, want.md)
		requireCommon(t, r, notifyBucket, "meta", req, host)
		requireEqual(t, r.EventName, want.name, "event name")
		o := r.S3.Object
		if o.Key != "k" || o.ETag == nil || *o.ETag == "" || o.Size != nil || o.Sequencer != nil {
			t.Errorf("%s: object %+v, want k's eTag without size or sequencer", want.name, o)
		}
	}
}

// A function destination is invoked asynchronously with the event message
// as its payload, which aws-lambda-go's events.S3Event decodes in the
// recorder; a function of another account than the functions service's
// fails validation.
func TestLambdaDelivery(t *testing.T) {
	s := startNotificationStack(t)
	createBucket(t, s.s3, notifyBucket)
	s.createRecorder(t, "recorder")
	s.allowS3(t, "recorder", notifyBucket)
	res := s.put(t, functionDoc("recorder"), nil)
	requireEqual(t, res.StatusCode, http.StatusOK, "configure the recorder: "+res.Body)

	put, err := s.s3.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(notifyBucket), Key: aws.String("red flower.jpg"),
		Body: strings.NewReader("petals"),
	})
	requireNoErr(t, err, "put")
	reqID, hostID := sdkRequestIDs(t, put.ResultMetadata)

	inv := s.awaitInvocation(t, "recorder")
	requireEqual(t, inv.Status, "succeeded", "the recorder's status: "+inv.ErrorMessage)
	var payload struct{ Records []eventRecord }
	requireNoErr(t, json.Unmarshal([]byte(inv.Payload), &payload), "decode the payload")
	requireEqual(t, len(payload.Records), 1, "records in the payload")
	requireCommon(t, &payload.Records[0], notifyBucket, payload.Records[0].S3.ConfigurationID,
		reqID, hostID)
	var summary struct {
		Records []struct {
			EventName, Bucket, Key, ETag, Sequencer, PrincipalID, RequestID string
			SourceIP                                                        string `json:"sourceIPAddress"`
			Size                                                            int64
		}
	}
	requireNoErr(t, json.Unmarshal([]byte(inv.Result), &summary), "decode the result")
	requireEqual(t, len(summary.Records), 1, "records the recorder saw")
	got := summary.Records[0]
	requireEqual(t, got.EventName, "ObjectCreated:Put", "eventName")
	requireEqual(t, got.Bucket, notifyBucket, "bucket")
	requireEqual(t, got.Key, "red+flower.jpg", "key")
	requireEqual(t, got.Size, 6, "size")
	requireEqual(t, got.ETag, strings.Trim(aws.ToString(put.ETag), `"`), "eTag")
	requireEqual(t, got.RequestID, reqID, "x-amz-request-id")
	requireEqual(t, got.PrincipalID, account, "principalId")
	requireEqual(t, got.SourceIP, "127.0.0.1", "sourceIPAddress")
	requireEqual(t, s.invocations(t, "recorder"), 1, "invocations of the recorder")

	other := "arn:aws:lambda:us-east-1:111111111111:function:recorder"
	res = s.put(t, `<NotificationConfiguration><CloudFunctionConfiguration><CloudFunction>`+
		other+`</CloudFunction><Event>s3:ObjectCreated:*</Event></CloudFunctionConfiguration>`+
		`</NotificationConfiguration>`, nil)
	e := requireBadRequest(t, res, "InvalidArgument", unableToValidate)
	requireEqual(t, e.ArgumentName1, other, "the refused function")
	requireEqual(t, e.ArgumentValue1, "The destination does not exist", "the reason")
}

// awaitInvocation waits for the first invocation of function through the
// service invoke endpoint and returns it, with its payload and result.
func (s *notificationStack) awaitInvocation(t *testing.T, function string) invocation {
	t.Helper()
	deadline := time.Now().Add(serviceTimeout)
	for {
		items := s.listInvocations(t, map[string][]string{
			"function": {function}, "source": {"service"},
		})
		if len(items) > 0 {
			return s.readInvocation(t, items[0].RequestID)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not invoked", function)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// A queue of another account than the bucket owner's is delivered to when
// the queues service serves that account and the queue's policy names the
// bucket owner as aws:SourceAccount.
func TestCrossAccountQueue(t *testing.T) {
	const other = "111111111111"
	s := startNotificationStackWith(t, stackOptions{
		queuesEnv: []string{"TULE_Queues_AccountID=" + other},
	})
	createBucket(t, s.s3, notifyBucket)
	arn := "arn:aws:sqs:us-east-1:" + other + ":events"
	// The policy names the bucket owner, another account, as
	// aws:SourceAccount.
	policy := s3QueuePolicy(arn, notifyBucket)
	q := s.createQueue(t, eventsQueue, policy)
	requireNoErr(t, putNotification(t, s.s3, notifyBucket, &types.NotificationConfiguration{
		QueueConfigurations: []types.QueueConfiguration{{
			Id: aws.String("x"), QueueArn: aws.String(arn), Events: createdAndRemoved,
		}},
	}, false), "configure the other account's queue")
	putKeys(t, s.s3, notifyBucket, "k")
	recs := s.awaitEvents(t, q, 1)
	requireEqual(t, recs[0].S3.Object.Key, "k", "key")
	requireEqual(t, recs[0].UserIdentity.PrincipalID, account, "principalId")
}

// A queues endpoint that accepts connections and never answers holds up
// no write: delivery runs apart from the requests.
func TestWritesNeverWaitForDelivery(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	requireNoErr(t, err, "listen")
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	s := startNotificationStackWith(t, stackOptions{queuesEndpoint: "http://" + ln.Addr().String()})
	createBucket(t, s.s3, notifyBucket)
	res := s.put(t, queuesDoc("hang"), skipValidation)
	requireEqual(t, res.StatusCode, http.StatusOK, "configure with skip: "+res.Body)
	for i := range 20 {
		start := time.Now()
		putKeys(t, s.s3, notifyBucket, "hang/"+strings.Repeat("k", i+1))
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Fatalf("PUT %d took %v", i, d)
		}
	}
	mu.Lock()
	n := len(conns)
	mu.Unlock()
	if n == 0 {
		t.Fatal("the deliverer never called the hanging endpoint")
	}
}

// dropTiming is a deliverer's timing whose retries are a second apart and
// run out after the third attempt, about two seconds after the first:
// long enough to see an event wait for its retries, short enough to see
// them run out.
var dropTiming = notify.Settings{
	Backoff: time.Second, MaxBackoff: time.Second, MaxAttempts: 3,
	Poll: 200 * time.Millisecond,
}

// An event whose destination is down is retried, and survives a restart
// of object-store: once queues is back, it arrives, once, or again with
// the same sequencer, which delivery at least once allows.
func TestDeliveryRetriesAndSurvivesRestart(t *testing.T) {
	s := startNotificationStackWith(t, stackOptions{live: liveOptions{notifyTiming: notify.Settings{
		Backoff: 100 * time.Millisecond, MaxBackoff: 500 * time.Millisecond, MaxAttempts: 1000,
	}}})
	createBucket(t, s.s3, notifyBucket)
	q := s.queueFor(t, notifyBucket, queueConf("all", createdAndRemoved))
	s.queues.stop(t)
	putKeys(t, s.s3, notifyBucket, "survivor")
	s.restartObjectStore(t)
	s.restartQueues(t)
	recs := s.receiveEvents(t, q, 1, eventWait)
	if len(recs) != 1 {
		t.Fatalf("%d events after the restarts, want 1", len(recs))
	}
	recs = append(recs, s.receiveEvents(t, q, 1, 2*time.Second)...)
	for _, r := range recs {
		requireEqual(t, r.S3.Object.Key, "survivor", "key")
		requireEqual(t, aws.ToString(r.S3.Object.Sequencer),
			aws.ToString(recs[0].S3.Object.Sequencer), "a duplicate's sequencer")
	}
	s.requireOutboxEmpty(t, notifyBucket)
}

// requireOutboxEmpty waits until bucket's outbox in object-store's storage
// root holds no pending event, failing after eventWait.
func (s *notificationStack) requireOutboxEmpty(t *testing.T, bucket string) {
	t.Helper()
	s.requireOutboxEmptyWithin(t, bucket, eventWait)
}

// requireOutboxEmptyWithin is requireOutboxEmpty failing after wait.
func (s *notificationStack) requireOutboxEmptyWithin(
	t *testing.T, bucket string, wait time.Duration,
) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		n := s.pendingIn(t, bucket)
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d events of %s still pending after %v", n, bucket, wait)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pendingIn returns how many events bucket's outbox holds pending.
func (s *notificationStack) pendingIn(t *testing.T, bucket string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.srv.Root, ".tule", "events", bucket))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	pending := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			pending++
		}
	}
	return pending
}

// An event the queue's policy keeps refusing waits for its retries, a
// second apart, and is dropped once its third attempt fails; one the
// queue refuses outright, larger than its MaximumMessageSize, is dropped
// at once, before any retry could run. Neither arrives once the queue
// would take it, while a later event does.
func TestUndeliverableEventsAreDropped(t *testing.T) {
	s := startNotificationStackWith(t, stackOptions{live: liveOptions{notifyTiming: dropTiming}})
	createBucket(t, s.s3, notifyBucket)
	q := s.queueFor(t, notifyBucket, queueConf("all", createdAndRemoved))
	setAttr := func(name sqstypes.QueueAttributeName, v string) {
		_, err := s.sqs.SetQueueAttributes(t.Context(), &sqs.SetQueueAttributesInput{
			QueueUrl: aws.String(q), Attributes: map[string]string{string(name): v},
		})
		requireNoErr(t, err, "set "+string(name))
	}
	// A policy for another bucket refuses this one's events.
	setAttr(sqstypes.QueueAttributeNamePolicy, s3QueuePolicy(queueARN(eventsQueue), "other-bucket"))
	start := time.Now()
	putKeys(t, s.s3, notifyBucket, "refused")
	// The first attempt fails at once; the retries are a second apart.
	time.Sleep(500 * time.Millisecond)
	requireEqual(t, s.pendingIn(t, notifyBucket), 1, "the refused event, waiting for its retry")
	s.requireOutboxEmpty(t, notifyBucket)
	if d := time.Since(start); d < 2*dropTiming.Backoff-200*time.Millisecond {
		t.Fatalf("the refused event was dropped after %v, before its retries could run", d)
	}

	setAttr(sqstypes.QueueAttributeNamePolicy, s3QueuePolicy(queueARN(eventsQueue), notifyBucket))
	setAttr(sqstypes.QueueAttributeNameMaximumMessageSize, "1024")
	putKeys(t, s.s3, notifyBucket, "big/"+strings.Repeat("k", 900))
	s.requireOutboxEmptyWithin(t, notifyBucket, dropTiming.Backoff-200*time.Millisecond)

	putKeys(t, s.s3, notifyBucket, "accepted")
	recs := s.awaitEvents(t, q, 1)
	requireEqual(t, recs[0].S3.Object.Key, "accepted", "the only event")
}
