package dirimpl

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/config"
	"github.com/gasmod/gas/config/configtest"
	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/notification"
)

// testQueue is where stubRouter sends every event.
var testQueue = contracts.Destination{
	Type: contracts.DestinationQueue, ARN: "arn:aws:sqs:us-east-1:000000000000:q",
	ConfigurationID: "c",
}

// stubRouter routes every event of a configuration stored for the bucket's
// generation, {"generation":…}, to testQueue, and renders an event as its
// JSON, so a test reads back what the storage recorded.
type stubRouter struct {
	// renderErr, when set, fails Render.
	renderErr atomic.Pointer[error]
}

func (r *stubRouter) Route(
	doc []byte, generation string, _ *contracts.BucketEvent,
) (contracts.Destination, bool, error) {
	var stored struct{ Generation string }
	if err := json.Unmarshal(doc, &stored); err != nil {
		return contracts.Destination{}, false, fmt.Errorf("stub router: %w", err)
	}
	return testQueue, stored.Generation == generation, nil
}

func (r *stubRouter) Render(_ contracts.Destination, ev *contracts.BucketEvent) ([]byte, error) {
	if err := r.renderErr.Load(); err != nil {
		return nil, *err
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("stub router: %w", err)
	}
	return b, nil
}

// eventFixture is a fixture whose service records events with a
// stubRouter, on a root it can be opened on again.
type eventFixture struct {
	*fixture
	router  *stubRouter
	signals *atomic.Int32
	root    string
}

// openEventService returns an initialized Service on root that records
// events with events.
func openEventService(t *testing.T, root string, events Events) *Service {
	t.Helper()
	cfg := config.New(config.WithProvider(configtest.NewStaticProvider(nil)))
	if err := cfg.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}
	s := NewService(&Settings{RootDir: root}, contracts.SystemClock, events)(cfg, &gas.NopLogger{})
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func newEventFixture(t *testing.T) *eventFixture {
	t.Helper()
	f := &eventFixture{router: &stubRouter{}, signals: &atomic.Int32{}, root: t.TempDir()}
	s := openEventService(t, f.root, f.events())
	if _, err := s.CreateBucket(context.Background(), "bucket", nil); err != nil {
		t.Fatal(err)
	}
	f.fixture = pinnedFixture(t, s)
	return f
}

func (f *eventFixture) events() Events {
	return Events{
		Router: f.router, Signal: func() { f.signals.Add(1) }, ImplicitACL: []byte("<implicit/>"),
	}
}

// restart closes the service and opens the root again, as a crash and a
// start would, without RecoverEvents.
func (f *eventFixture) restart() {
	f.t.Helper()
	f.release()
	if err := f.s.Close(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	f.fixture = pinnedFixture(f.t, openEventService(f.t, f.root, f.events()))
}

// recreate deletes "bucket", which is empty, and creates it again.
func (f *eventFixture) recreate() {
	f.t.Helper()
	if err := f.deleteBucket(); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.s.CreateBucket(context.Background(), "bucket", nil); err != nil {
		f.t.Fatal(err)
	}
	f.fixture = pinnedFixture(f.t, f.s)
}

// configure gives "bucket" a notification configuration of its
// generation, which stubRouter routes every event of.
func (f *eventFixture) configure() {
	f.t.Helper()
	gen := ref(f.t, f.s, "bucket").Generation
	if err := f.s.PutBucketConfig(f.ctx, "bucket", contracts.ConfigNotification,
		[]byte(`{"generation":"`+gen+`"}`)); err != nil {
		f.t.Fatal(err)
	}
}

// outbox returns the names of the files of "bucket"'s outbox.
func (f *eventFixture) outbox() []string {
	f.t.Helper()
	entries, err := fs.ReadDir(f.s.root.FS(), filepath.Join(eventsDir, "bucket"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// intents returns the names of the intents of "bucket"'s outbox.
func (f *eventFixture) intents() []string {
	f.t.Helper()
	var out []string
	for _, n := range f.outbox() {
		if strings.HasSuffix(n, intentExt) {
			out = append(out, n)
		}
	}
	return out
}

// pending returns the pending events, decoded as stubRouter rendered them,
// and acknowledges them.
func (f *eventFixture) pending() []contracts.BucketEvent {
	f.t.Helper()
	evs, err := f.s.PendingEvents(f.ctx, 1000, contracts.PendingQuery{})
	if err != nil {
		f.t.Fatal(err)
	}
	out := make([]contracts.BucketEvent, len(evs))
	for i, pe := range evs {
		if pe.Destination != testQueue {
			f.t.Fatalf("destination %+v", pe.Destination)
		}
		if err = json.Unmarshal(pe.Body, &out[i]); err != nil {
			f.t.Fatal(err)
		}
		if err = f.s.AckEvent(f.ctx, pe.Bucket, pe.ID); err != nil {
			f.t.Fatal(err)
		}
	}
	return out
}

// stat returns the info of the current version of "k".
func (f *eventFixture) stat() *contracts.ObjectInfo {
	f.t.Helper()
	info, err := f.s.StatObject(f.ctx, oref("bucket", "k"))
	if err != nil {
		f.t.Fatal(err)
	}
	return info
}

// eventOp is an operation of the recording matrix: run does it and returns
// the version ID it reports, and want returns the event it causes, given
// that ID, nil for none.
type eventOp struct {
	run  func(f *eventFixture) (string, error)
	want func(f *eventFixture, st contracts.Versioning, vid string) *contracts.BucketEvent
}

// created is the event of a write of "new" at "k".
func created(name contracts.EventName) func(*eventFixture, contracts.Versioning,
	string) *contracts.BucketEvent {
	return func(f *eventFixture, _ contracts.Versioning, vid string) *contracts.BucketEvent {
		return &contracts.BucketEvent{
			Name: name, Bucket: "bucket", Key: "k", VersionID: vid, ETag: f.stat().ETag,
			Size: 3, HasObject: true,
		}
	}
}

// onK is an event of name on "k", the version vid, with k's ETag when
// etag is set.
func onK(name contracts.EventName, etag bool) func(*eventFixture, contracts.Versioning,
	string) *contracts.BucketEvent {
	return func(f *eventFixture, _ contracts.Versioning, vid string) *contracts.BucketEvent {
		ev := &contracts.BucketEvent{Name: name, Bucket: "bucket", Key: "k", VersionID: vid}
		if etag {
			ev.ETag = f.stat().ETag
		}
		return ev
	}
}

// writeOp runs write as an eventOp.
func writeOp(write func(f *fixture) (*contracts.ObjectInfo, error)) func(*eventFixture) (
	string, error) {
	return func(f *eventFixture) (string, error) {
		info, err := write(f.fixture)
		if err != nil {
			return "", err
		}
		return info.VersionID, nil
	}
}

func deleteOp(r contracts.ObjectRef) func(*eventFixture) (string, error) {
	return func(f *eventFixture) (string, error) {
		out, err := f.s.DeleteObject(f.ctx, r)
		if err != nil {
			return "", err
		}
		return out.VersionID, nil
	}
}

func none(*eventFixture, contracts.Versioning, string) *contracts.BucketEvent { return nil }

// eventOps are the operations that record events, and some that do not,
// on the keys eventSetup leaves.
var eventOps = map[string]eventOp{
	"put":  {run: writeOp(writeOps["put"]), want: created(contracts.EventObjectCreatedPut)},
	"copy": {run: writeOp(writeOps["copy"]), want: created(contracts.EventObjectCreatedCopy)},
	"complete": {
		run:  writeOp(writeOps["complete"]),
		want: created(contracts.EventObjectCreatedCompleteMultipartUpload),
	},
	"delete": {
		run: deleteOp(oref("bucket", "k")),
		want: func(f *eventFixture, st contracts.Versioning, vid string) *contracts.BucketEvent {
			if st == contracts.Unversioned {
				return onK(contracts.EventObjectRemovedDelete, false)(f, st, vid)
			}
			return onK(contracts.EventObjectRemovedDeleteMarkerCreated, false)(f, st, vid)
		},
	},
	"delete version": {
		run: func(f *eventFixture) (string, error) {
			return deleteOp(vref("k", f.ids("k")[0]))(f)
		},
		want: onK(contracts.EventObjectRemovedDelete, false),
	},
	"delete only version": {
		run: func(f *eventFixture) (string, error) {
			return deleteOp(vref("solo", f.ids("solo")[0]))(f)
		},
		want: func(_ *eventFixture, _ contracts.Versioning, vid string) *contracts.BucketEvent {
			return &contracts.BucketEvent{
				Name: contracts.EventObjectRemovedDelete, Bucket: "bucket", Key: "solo",
				VersionID: vid,
			}
		},
	},
	"delete missing key": {
		run: deleteOp(oref("bucket", "missing")),
		want: func(_ *eventFixture, st contracts.Versioning, vid string) *contracts.BucketEvent {
			if st == contracts.Unversioned {
				return nil
			}
			return &contracts.BucketEvent{
				Name: contracts.EventObjectRemovedDeleteMarkerCreated, Bucket: "bucket",
				Key: "missing", VersionID: vid,
			}
		},
	},
	"delete missing version": {
		run:  deleteOp(vref("k", newVersionID(1))),
		want: none,
	},
	"tagging put": {
		run: func(f *eventFixture) (string, error) {
			return f.s.PutObjectTagging(f.ctx, oref("bucket", "k"),
				[]contracts.Tag{{Key: "a", Value: "b"}}, nil)
		},
		want: onK(contracts.EventObjectTaggingPut, true),
	},
	"tagging delete": {
		run: func(f *eventFixture) (string, error) {
			return f.s.DeleteObjectTagging(f.ctx, oref("bucket", "k"), nil)
		},
		want: onK(contracts.EventObjectTaggingDelete, true),
	},
	"acl changed": {
		run: func(f *eventFixture) (string, error) {
			return f.s.PutObjectACL(f.ctx, oref("bucket", "k"), []byte("<changed/>"), nil)
		},
		want: onK(contracts.EventObjectACLPut, true),
	},
	"acl implicit": {
		run: func(f *eventFixture) (string, error) {
			return f.s.PutObjectACL(f.ctx, oref("bucket", "solo"), []byte("<implicit/>"), nil)
		},
		want: none,
	},
	"acl unchanged": {
		run: func(f *eventFixture) (string, error) {
			return f.s.PutObjectACL(f.ctx, oref("bucket", "k"), []byte("<set/>"), nil)
		},
		want: none,
	},
}

// eventSetup leaves, in bucket state st, without a notification
// configuration: "src", holding "new"; "k", a key written before st was
// set and once after, with ACL <set/>; and "solo", written once after.
func eventSetup(f *eventFixture, st contracts.Versioning) {
	f.put("src", "new")
	f.put("k", "v0")
	if st != contracts.Unversioned {
		f.state(st)
	}
	f.putIn("k", "v1", contracts.PutObjectInput{ACL: []byte("<set/>")})
	f.put("solo", "x")
}

// forEachEventCase runs run for each versioning state and eventOp, on an
// eventFixture set up by eventSetup.
func forEachEventCase(
	t *testing.T,
	run func(t *testing.T, f *eventFixture, st contracts.Versioning, op eventOp),
) {
	for _, st := range bucketStates {
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for n := range eventOps {
				if !yield(n) {
					return
				}
			}
		}) {
			op := eventOps[name]
			t.Run(cmpName(st)+"/"+name, func(t *testing.T) {
				f := newEventFixture(t)
				eventSetup(f, st)
				run(t, f, st, op)
			})
		}
	}
}

// requireEvent fails unless got is want, but for its time, source and
// sequence, which has to be set for a sequenced event, and not otherwise.
func requireEvent(t *testing.T, got, want *contracts.BucketEvent) {
	t.Helper()
	sequenced := want.Name != contracts.EventObjectTaggingPut &&
		want.Name != contracts.EventObjectTaggingDelete && want.Name != contracts.EventObjectACLPut
	if (got.Sequence != 0) != sequenced {
		t.Fatalf("sequence %d for %s", got.Sequence, want.Name)
	}
	if got.Time.IsZero() {
		t.Fatal("no event time")
	}
	g := *got
	g.Sequence, g.Time, g.Source = 0, time.Time{}, contracts.EventSource{}
	if g != *want {
		t.Fatalf("event\n got %+v\nwant %+v", g, *want)
	}
}

// requireEvents fails unless evs is want alone, in state st, or nothing
// for a nil want.
func requireEvents(
	t *testing.T,
	st contracts.Versioning,
	evs []contracts.BucketEvent,
	want *contracts.BucketEvent,
) {
	t.Helper()
	if want == nil {
		if len(evs) != 0 {
			t.Fatalf("events %+v, want none", evs)
		}
		return
	}
	if len(evs) != 1 {
		t.Fatalf("events %+v, want one", evs)
	}
	if (evs[0].VersionID == "") != (st == contracts.Unversioned) {
		t.Fatalf("version ID %q in state %q", evs[0].VersionID, st)
	}
	requireEvent(t, &evs[0], want)
}

// Each operation records the event S3 sends for it, once committed, in
// every versioning state, and nothing else; the deliverer is signaled.
func TestEventRecordingMatrix(t *testing.T) {
	forEachEventCase(t, func(t *testing.T, f *eventFixture, st contracts.Versioning, op eventOp) {
		f.configure()
		src := contracts.EventSource{
			PrincipalID: "AIDA1", SourceIP: "127.0.0.1",
			RequestID: "r", HostID: "h",
		}
		f.ctx = contracts.WithEventSource(f.ctx, src)
		vid, err := op.run(f)
		if err != nil {
			t.Fatal(err)
		}
		got := f.pending()
		requireEvents(t, st, got, op.want(f, st, vid))
		if len(got) == 1 && got[0].Source != src {
			t.Fatalf("source %+v", got[0].Source)
		}
		if int(f.signals.Load()) != len(got) {
			t.Fatalf("signaled %d times", f.signals.Load())
		}
		if len(f.outbox()) != 0 {
			t.Fatalf("outbox left %v", f.outbox())
		}
	})
}

// A later change to a key always has a greater sequencer, also a permanent
// delete of a version older than the ones written since.
func TestEventSequencersFollowTheCommits(t *testing.T) {
	for _, st := range bucketStates[1:] {
		t.Run(cmpName(st), func(t *testing.T) {
			f := newEventFixture(t)
			f.state(contracts.VersioningEnabled)
			v1 := f.put("k", "v1")
			f.configure()
			f.put("k", "v2")
			f.state(st)
			f.put("k", "v3")
			f.del(oref("bucket", "k"))
			f.del(vref("k", v1.VersionID))
			evs := f.pending()
			if len(evs) != 4 {
				t.Fatalf("events %+v", evs)
			}
			for i := 1; i < len(evs); i++ {
				if evs[i].Sequence <= evs[i-1].Sequence {
					t.Fatalf("sequence %d after %d", evs[i].Sequence, evs[i-1].Sequence)
				}
			}
		})
	}
}

// Without a notification configuration nothing is recorded: no outbox for
// the bucket, and trailers byte for byte as v0.12.0 wrote them, without a
// writeId; with one, a write in place has one.
func TestNoConfigurationRecordsNothing(t *testing.T) {
	forEachEventCase(t, func(t *testing.T, f *eventFixture, _ contracts.Versioning, op eventOp) {
		if _, err := op.run(f); err != nil {
			t.Fatal(err)
		}
		if got := f.outbox(); got != nil || f.exists(seqLeaseFile) {
			t.Fatalf("outbox %v, lease %v", got, f.exists(seqLeaseFile))
		}
		if f.signals.Load() != 0 {
			t.Fatal("signaled")
		}
	})

	f := newEventFixture(t)
	f.put("k", "v1")
	raw := f.rawTrailer()
	if bytes.Contains(raw, []byte("writeId")) {
		t.Fatalf("trailer %s", raw)
	}
	// The trailer as v0.12.0 encodes it.
	var old struct {
		LastModified time.Time         `json:"lastModified"`
		Meta         map[string]string `json:"meta"`
		ContentType  string            `json:"contentType"`
		Tags         []contracts.Tag   `json:"tags,omitempty"`
	}
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	if again, err := json.Marshal(old); err != nil || !bytes.Equal(again, raw) {
		t.Fatalf("trailer %s, v0.12.0 encodes %s (%v)", raw, again, err)
	}

	f.configure()
	f.put("k", "v2")
	if !bytes.Contains(f.rawTrailer(), []byte(`"writeId":"`)) {
		t.Fatalf("trailer of a write with an event: %s", f.rawTrailer())
	}
}

// rawTrailer returns the trailer JSON of "k"'s object in place.
func (f *eventFixture) rawTrailer() []byte {
	f.t.Helper()
	obj, err := f.s.dir.GetObject(f.ctx, &storage.GetObjectInput{
		HeadObjectInput: storage.HeadObjectInput{BaseObjectInput: dataInput("bucket", "k", nil)},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	b, err := io.ReadAll(obj.Body)
	if cErr := obj.Body.Close(); err != nil || cErr != nil {
		f.t.Fatal(err, cErr)
	}
	end := len(b) - trailerFooterSize
	n := int(binary.BigEndian.Uint64(b[end : end+8])) //nolint:gosec // a test's small trailer
	return b[end-n : end]
}

// routerCase is a case of TestEventsWithTheNotificationRouter: the
// generation the configuration is stored for, "" for the bucket's, and
// the events a put and a delete record.
type routerCase struct {
	generation string
	events     int
	available  bool
}

// The notification package's Router, with its generation guard: a
// configuration of another generation, or whose destination type is not
// available, records nothing.
func TestEventsWithTheNotificationRouter(t *testing.T) {
	cfg, err := notification.Parse([]byte(`<NotificationConfiguration><QueueConfiguration>` +
		`<Id>c</Id><Queue>arn:aws:sqs:us-east-1:000000000000:q</Queue>` +
		`<Event>s3:ObjectCreated:*</Event></QueueConfiguration></NotificationConfiguration>`))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]routerCase{
		"routed":           {available: true, events: 1},
		"other generation": {available: true, generation: "old", events: 0},
		"unavailable":      {available: false, events: 0},
	} {
		t.Run(name, func(t *testing.T) { runRouterCase(t, cfg, tc) })
	}
}

func runRouterCase(t *testing.T, cfg *notification.Configuration, tc routerCase) {
	router := notification.NewRouter("000000000000",
		map[contracts.DestinationType]bool{contracts.DestinationQueue: tc.available})
	s := openEventService(t, t.TempDir(), Events{Router: router})
	if _, err := s.CreateBucket(context.Background(), "bucket", nil); err != nil {
		t.Fatal(err)
	}
	f := pinnedFixture(t, s)
	gen := tc.generation
	if gen == "" {
		gen = ref(t, s, "bucket").Generation
	}
	doc, err := notification.Encode(gen, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PutBucketConfig(f.ctx, "bucket", contracts.ConfigNotification, doc); err != nil {
		t.Fatal(err)
	}
	f.put("k", "v1")
	f.del(oref("bucket", "k")) // not configured
	evs, err := s.PendingEvents(f.ctx, 10, contracts.PendingQuery{})
	if err != nil || len(evs) != tc.events {
		t.Fatalf("events %d (%v), want %d", len(evs), err, tc.events)
	}
	put := []byte(`"eventName":"ObjectCreated:Put"`)
	if tc.events == 1 && !bytes.Contains(evs[0].Body, put) {
		t.Fatalf("body %s", evs[0].Body)
	}
}

// A failure to record an event never fails the write: with an outbox that
// cannot be written, the write commits and nothing is recorded.
func TestAnUnwritableOutboxNeverFailsTheWrite(t *testing.T) {
	forEachEventCase(t, func(t *testing.T, f *eventFixture, _ contracts.Versioning, op eventOp) {
		f.configure()
		if err := f.s.root.WriteFile(filepath.Join(eventsDir, "bucket"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := op.run(f); err != nil {
			t.Fatal(err)
		}
		f.put("k", "last")
		if got := f.body(oref("bucket", "k")); got != "last" {
			t.Fatalf("body %q", got)
		}
		if f.signals.Load() != 0 {
			t.Fatal("signaled")
		}
	})
}

// failRender runs change with a Render that fails, so its confirm does.
func (f *eventFixture) failRender(change func()) {
	f.t.Helper()
	renderErr := errors.New("render")
	f.router.renderErr.Store(&renderErr)
	change()
	f.router.renderErr.Store(nil)
}

// confirmCommitted runs ConfirmCommitted, which must confirm n events.
func (f *eventFixture) confirmCommitted(n int) {
	f.t.Helper()
	got, err := f.s.ConfirmCommitted(f.ctx)
	if err != nil || got != n {
		f.t.Fatalf("confirmed %d (%v), want %d", got, err, n)
	}
}

// With a message that cannot be rendered, the write commits and its event
// is marked committed, which ConfirmCommitted confirms once it can,
// trying again after a failure.
func TestARenderFailureMarksTheEventCommitted(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	f.failRender(func() { f.put("k", "v1") })
	if got := f.body(oref("bucket", "k")); got != "v1" {
		t.Fatalf("body %q", got)
	}
	if out := f.outbox(); len(out) != 1 || !strings.HasSuffix(out[0], committedExt) {
		t.Fatalf("outbox %v", out)
	}
	f.failRender(func() {
		if _, err := f.s.ConfirmCommitted(f.ctx); err == nil {
			t.Fatal("confirmed with a failing render")
		}
	})
	if len(f.outbox()) != 1 {
		t.Fatal("a failed confirm dropped the mark")
	}
	f.confirmCommitted(1)
	requireEvents(t, contracts.Unversioned, f.pending(), &contracts.BucketEvent{
		Name: contracts.EventObjectCreatedPut, Bucket: "bucket", Key: "k",
		ETag: f.stat().ETag, Size: 2, HasObject: true,
	})
	f.confirmCommitted(0)
}

// laterChange is a case of TestACommittedEventSurvivesLaterChanges: change,
// whose confirm fails, then second, which falsifies change's witness, on
// a key setup leaves, and the events they cause.
type laterChange struct {
	setup          func(f *eventFixture)
	change, second func(f *eventFixture)
	want           [2]contracts.EventName
}

// tagK gives "k" the tag t=v.
func tagK(f *eventFixture, v string) {
	if _, err := f.s.PutObjectTagging(f.ctx, oref("bucket", "k"),
		[]contracts.Tag{{Key: "t", Value: v}}, nil); err != nil {
		f.t.Fatal(err)
	}
}

// aclK gives "k" the ACL v.
func aclK(f *eventFixture, v string) {
	if _, err := f.s.PutObjectACL(f.ctx, oref("bucket", "k"), []byte(v), nil); err != nil {
		f.t.Fatal(err)
	}
}

// laterChanges are the cases of TestACommittedEventSurvivesLaterChanges.
func laterChanges() map[string]laterChange {
	return map[string]laterChange{
		"unversioned overwrite": {
			change: func(f *eventFixture) { f.put("k", "v1") },
			second: func(f *eventFixture) { f.put("k", "v2") },
			want: [2]contracts.EventName{
				contracts.EventObjectCreatedPut,
				contracts.EventObjectCreatedPut,
			},
		},
		"put after a delete in place": {
			setup:  func(f *eventFixture) { f.put("k", "v1") },
			change: func(f *eventFixture) { f.del(oref("bucket", "k")) },
			second: func(f *eventFixture) { f.put("k", "v2") },
			want: [2]contracts.EventName{
				contracts.EventObjectRemovedDelete,
				contracts.EventObjectCreatedPut,
			},
		},
		"suspended overwrite of the null version": {
			setup: func(f *eventFixture) {
				f.state(contracts.VersioningEnabled)
				f.put("k", "v1")
				f.state(contracts.VersioningSuspended)
			},
			change: func(f *eventFixture) { f.put("k", "null1") },
			second: func(f *eventFixture) { f.put("k", "null2") },
			want: [2]contracts.EventName{
				contracts.EventObjectCreatedPut,
				contracts.EventObjectCreatedPut,
			},
		},
		"tag rewrite": {
			setup:  func(f *eventFixture) { f.put("k", "v1") },
			change: func(f *eventFixture) { tagK(f, "a") },
			second: func(f *eventFixture) { tagK(f, "b") },
			want: [2]contracts.EventName{
				contracts.EventObjectTaggingPut,
				contracts.EventObjectTaggingPut,
			},
		},
		"ACL rewrite": {
			setup:  func(f *eventFixture) { f.put("k", "v1") },
			change: func(f *eventFixture) { aclK(f, "<a/>") },
			second: func(f *eventFixture) { aclK(f, "<b/>") },
			want: [2]contracts.EventName{
				contracts.EventObjectACLPut,
				contracts.EventObjectACLPut,
			},
		},
	}
}

// An event whose confirm failed is confirmed, by recovery after a start,
// also when the key changed since in a way that falsifies its witness, and
// sorts before the event of that change.
func TestACommittedEventSurvivesLaterChanges(t *testing.T) {
	for name, tc := range laterChanges() {
		t.Run(name, func(t *testing.T) {
			f := newEventFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			f.configure()
			f.failRender(func() { tc.change(f) })
			tc.second(f)
			f.restart()
			if out, err := f.s.RecoverEvents(f.ctx); err != nil ||
				out != (contracts.RecoveredEvents{}) {
				t.Fatalf("recovered %+v, %v", out, err)
			}
			f.confirmCommitted(1)
			evs := f.pending()
			if len(evs) != 2 || evs[0].Name != tc.want[0] || evs[1].Name != tc.want[1] {
				t.Fatalf("events %+v, want %v", evs, tc.want)
			}
		})
	}
}

// An event whose confirm failed outlives its bucket: it is confirmed after
// the bucket is deleted, and also once it is created again.
func TestACommittedEventOutlivesItsBucket(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		f := newEventFixture(t)
		f.configure()
		f.put("k", "v1")
		f.failRender(func() { f.del(oref("bucket", "k")) })
		f.pending()
		if err := f.deleteBucket(); err != nil {
			t.Fatal(err)
		}
		if recreate {
			if _, err := f.s.CreateBucket(context.Background(), "bucket", nil); err != nil {
				t.Fatal(err)
			}
		}
		f.release()
		if err := f.s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.s = openEventService(t, f.root, f.events())
		f.ctx = context.Background()
		if out, err := f.s.RecoverEvents(
			f.ctx,
		); err != nil ||
			out != (contracts.RecoveredEvents{}) {
			t.Fatalf("recovered %+v, %v", out, err)
		}
		f.confirmCommitted(1)
		evs := f.pending()
		if len(evs) != 1 || evs[0].Name != contracts.EventObjectRemovedDelete {
			t.Fatalf("events %+v", evs)
		}
	}
}

// A committed mark that is the intent renamed, as when the mark could not
// be written, has no details of a write in place: they are read back from
// the object, as long as it is the one written.
func TestARenamedMarkReadsItsDetailsBack(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	crashAt(f.fixture, stageCommitted)
	_, err := writeOps["put"](f.fixture)
	requireIs(t, "the stopped put", err, errCrash)
	intent := f.intents()
	if len(intent) != 1 {
		t.Fatalf("outbox %v", f.outbox())
	}
	dir := filepath.Join(eventsDir, "bucket")
	id := strings.TrimSuffix(intent[0], intentExt)
	if err = f.s.root.Rename(
		filepath.Join(dir, intent[0]),
		filepath.Join(dir, id+committedExt),
	); err != nil {
		t.Fatal(err)
	}
	f.confirmCommitted(1)
	requireEvents(t, contracts.Unversioned, f.pending(), &contracts.BucketEvent{
		Name: contracts.EventObjectCreatedPut, Bucket: "bucket", Key: "k",
		ETag: f.stat().ETag, Size: 3, HasObject: true,
	})
}

// crashCase is a stage a change stops at, and what RecoverEvents does
// after: its counts, and whether the event is pending then.
type crashCase struct {
	stage string
	want  contracts.RecoveredEvents
	event bool
}

// A stop at each stage of recording, for each operation that records an
// event, leaves what RecoverEvents settles as the stop requires: before
// the commit, the change did not happen and its intent is dropped; after
// it, the event is confirmed, its details read back from the store; after
// the event is written, only the intent is removed.
func TestRecoverEventsAfterACrash(t *testing.T) {
	for _, tc := range []crashCase{
		{stageEventIntent, contracts.RecoveredEvents{Dropped: 1}, false},
		{stageCommitted, contracts.RecoveredEvents{Confirmed: 1}, true},
		{stageEventConfirmed, contracts.RecoveredEvents{Confirmed: 1}, true},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			forEachEventCase(t, func(t *testing.T, f *eventFixture, st contracts.Versioning,
				op eventOp,
			) {
				crashAndRecover(t, f, st, op, tc)
			})
		})
	}
}

// crashAndRecover stops op at tc's stage, opens the store again, and
// checks what RecoverEvents does; an op that records no event in st is
// skipped.
func crashAndRecover(
	t *testing.T,
	f *eventFixture,
	st contracts.Versioning,
	op eventOp,
	tc crashCase,
) {
	if op.want(f, st, "") == nil {
		return
	}
	f.configure()
	crashAt(f.fixture, tc.stage)
	_, err := op.run(f)
	requireIs(t, "the stopped change", err, errCrash)
	if len(f.intents()) != 1 {
		t.Fatalf("outbox %v", f.outbox())
	}
	f.restart()
	out, err := f.s.RecoverEvents(f.ctx)
	if err != nil || out != tc.want {
		t.Fatalf("recovered %+v, %v; want %+v", out, err, tc.want)
	}
	evs := f.pending()
	if len(f.outbox()) != 0 {
		t.Fatalf("outbox left %v", f.outbox())
	}
	if !tc.event {
		requireEvents(t, st, evs, nil)
		return
	}
	vid := ""
	if len(evs) > 0 {
		vid = evs[0].VersionID
	}
	requireEvents(t, st, evs, op.want(f, st, vid))
}

// RecoverEvents removes temp files and intents marked aborted, and keeps
// pending events.
func TestRecoverEventsLeftovers(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	f.put("k", "v1")
	dir := filepath.Join(eventsDir, "bucket")
	tmp := filepath.Join(dir, "x.json.tmp-ABC")
	aborted := filepath.Join(dir, "00000000000000000001-"+strings.Repeat("A", 26)+abortedExt)
	for _, name := range []string{tmp, aborted} {
		if err := f.s.root.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := f.s.RecoverEvents(f.ctx)
	if err != nil || out != (contracts.RecoveredEvents{Dropped: 1}) {
		t.Fatalf("recovered %+v, %v", out, err)
	}
	if f.exists(tmp) || f.exists(aborted) {
		t.Fatalf("outbox left %v", f.outbox())
	}
	if evs := f.pending(); len(evs) != 1 || evs[0].Key != "k" {
		t.Fatalf("events %+v", evs)
	}
}

// A pending event that cannot be read is set aside, so it is not read,
// nor logged, again.
func TestAnUnreadableEventIsSetAside(t *testing.T) {
	f := newEventFixture(t)
	id := "00000000000000000001-" + strings.Repeat("A", 26)
	if err := f.s.root.MkdirAll(filepath.Join(eventsDir, "bucket"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := f.s.root.WriteFile(
		eventFile("bucket", id, eventExt),
		[]byte("{"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if evs, err := f.s.PendingEvents(
			f.ctx,
			10,
			contracts.PendingQuery{},
		); err != nil ||
			len(evs) != 0 {
			t.Fatalf("pending %v, %v", evs, err)
		}
	}
	if !f.exists(eventFile("bucket", id, badExt)) || f.exists(eventFile("bucket", id, eventExt)) {
		t.Fatalf("outbox %v", f.outbox())
	}
}

// A write in place sorts after the key's earlier events also after a
// restart on a clock behind the one they were recorded on: its sequence
// starts above the one its trailer holds, and so do the sequences of the
// key's later changes, also once it has versions.
func TestALegacyWriteSortsAfterTheKeysEvents(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	f.s.seq.Store(time.Now().Add(time.Hour).UnixNano())
	var last int64
	// next runs change, after a restart, which must record one event
	// sequenced after the last.
	next := func(change func()) {
		t.Helper()
		f.restart()
		change()
		evs := f.pending()
		if len(evs) != 1 || evs[0].Sequence <= last {
			t.Fatalf("events %+v after sequence %d", evs, last)
		}
		last = evs[0].Sequence
	}
	f.put("k", "v1")
	last = f.pending()[0].Sequence
	next(func() { f.put("k", "v2") })
	next(func() { f.del(oref("bucket", "k")) })
	next(func() { f.put("k", "v3") })
	next(func() {
		f.state(contracts.VersioningEnabled)
		f.put("k", "v4")
	})
}

// Concurrent writers to one key in place, in an Unversioned bucket and in
// a Suspended one without a manifest, get strictly increasing sequencers,
// the last the one the key's object holds.
func TestConcurrentLegacyWritersGetIncreasingSequencers(t *testing.T) {
	for _, st := range []contracts.Versioning{contracts.Unversioned, contracts.VersioningSuspended} {
		t.Run(cmpName(st), func(t *testing.T) {
			f := newEventFixture(t)
			if st != contracts.Unversioned {
				f.state(st)
			}
			f.configure()
			const writers = 16
			putConcurrently(t, f, writers)
			evs := f.pending()
			if len(evs) != writers || f.hasManifest() {
				t.Fatalf("%d events, manifest %v", len(evs), f.hasManifest())
			}
			for i := 1; i < len(evs); i++ {
				if evs[i].Sequence <= evs[i-1].Sequence {
					t.Fatalf("sequence %d after %d", evs[i].Sequence, evs[i-1].Sequence)
				}
			}
			var tr trailer
			if err := json.Unmarshal(f.rawTrailer(), &tr); err != nil {
				t.Fatal(err)
			}
			if tr.Seq != evs[len(evs)-1].Sequence {
				t.Fatalf("the object holds sequence %d, the last event %d", tr.Seq,
					evs[len(evs)-1].Sequence)
			}
		})
	}
}

// putConcurrently runs n puts of "k" at once.
func putConcurrently(t *testing.T, f *eventFixture, n int) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			_, err := f.s.PutObject(f.ctx, "bucket", "k", strings.NewReader("x"),
				contracts.PutObjectInput{}, contracts.Precondition{})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// DeleteBucket and CreateBucket leave the outbox alone: events recorded
// before a delete are still pending after it, and after the bucket is
// created again.
func TestDeleteBucketKeepsTheOutbox(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	f.put("k", "v1")
	f.del(oref("bucket", "k"))
	f.recreate()
	if len(f.outbox()) != 2 {
		t.Fatalf("outbox %v", f.outbox())
	}
	evs := f.pending()
	if len(evs) != 2 || evs[0].Name != contracts.EventObjectCreatedPut ||
		evs[1].Name != contracts.EventObjectRemovedDelete {
		t.Fatalf("events %+v", evs)
	}
	// The new bucket has no configuration: DeleteBucket removed it.
	f.put("k", "v2")
	if len(f.outbox()) != 0 {
		t.Fatalf("outbox %v", f.outbox())
	}
}

// twoBuckets returns an eventFixture pinned to "bucket" and "other", both
// configured, with events for puts of "a", "b" and "c" in "bucket", and
// one of "z" in "other", in this order.
func twoBuckets(t *testing.T) *eventFixture {
	t.Helper()
	f := newEventFixture(t)
	if _, err := f.s.CreateBucket(context.Background(), "other", nil); err != nil {
		t.Fatal(err)
	}
	ctx, release, err := f.s.Pin(f.ctx, ref(t, f.s, "other"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	f.ctx = ctx
	f.configure()
	if err = f.s.PutBucketConfig(f.ctx, "other", contracts.ConfigNotification,
		[]byte(`{"generation":"`+ref(t, f.s, "other").Generation+`"}`)); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		f.put(k, "x")
	}
	if _, err = f.s.PutObject(f.ctx, "other", "z", strings.NewReader("x"),
		contracts.PutObjectInput{}, contracts.Precondition{}); err != nil {
		t.Fatal(err)
	}
	return f
}

// PendingEvents takes each bucket's events in order, the buckets in turn.
func TestPendingEventsTakeTheBucketsInTurn(t *testing.T) {
	f := twoBuckets(t)
	evs, err := f.s.PendingEvents(f.ctx, 3, contracts.PendingQuery{})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(evs))
	for _, ev := range evs {
		got = append(got, ev.Bucket+"/"+keyOf(t, ev))
	}
	if want := []string{"bucket/a", "other/z", "bucket/b"}; !slices.Equal(got, want) {
		t.Fatalf("pending %v, want %v", got, want)
	}
	if evs, err = f.s.PendingEvents(f.ctx, 0, contracts.PendingQuery{}); err != nil || evs != nil {
		t.Fatalf("pending %v, %v for n = 0", evs, err)
	}
}

// PendingEvents leaves out the events Skip reports before it reads them,
// so they do not count toward n.
func TestPendingEventsSkipAndStartAfter(t *testing.T) {
	f := twoBuckets(t)
	all, err := f.s.PendingEvents(f.ctx, 10, contracts.PendingQuery{})
	if err != nil || len(all) != 4 {
		t.Fatalf("%d pending (%v)", len(all), err)
	}
	skipped := map[string]bool{all[0].ID: true, all[2].ID: true} // bucket/a, bucket/b
	evs, err := f.s.PendingEvents(f.ctx, 2, contracts.PendingQuery{
		Skip: func(bucket, id string) bool { return bucket == "bucket" && skipped[id] },
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(evs))
	for _, ev := range evs {
		got = append(got, ev.Bucket+"/"+keyOf(t, ev))
	}
	if want := []string{"bucket/c", "other/z"}; !slices.Equal(got, want) {
		t.Fatalf("pending with a, b skipped %v, want %v", got, want)
	}
}

// PendingEvents starts the turns with the bucket after After, wrapping
// around.
func TestPendingEventsStartAfter(t *testing.T) {
	f := twoBuckets(t)
	for after, want := range map[string]string{
		"bucket": "other", "other": "bucket", "zzz": "bucket", "a": "bucket",
	} {
		evs, err := f.s.PendingEvents(f.ctx, 1, contracts.PendingQuery{After: after})
		if err != nil || len(evs) != 1 || evs[0].Bucket != want {
			t.Fatalf("after %q: %+v (%v), want the first of %s", after, evs, err, want)
		}
	}
}

// AckEvent removes an event, idempotently, and refuses a name that cannot
// be an event's.
func TestAckEvent(t *testing.T) {
	f := twoBuckets(t)
	evs, err := f.s.PendingEvents(f.ctx, 10, contracts.PendingQuery{})
	if err != nil || len(evs) != 4 {
		t.Fatalf("%d pending (%v)", len(evs), err)
	}
	for range 2 {
		if err = f.s.AckEvent(f.ctx, evs[0].Bucket, evs[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range [][2]string{{"bucket", "../x"}, {"..", evs[1].ID}, {"bucket", ""}} {
		if err = f.s.AckEvent(f.ctx, bad[0], bad[1]); err == nil {
			t.Fatalf("acknowledged %q of %q", bad[1], bad[0])
		}
	}
	if evs, err = f.s.PendingEvents(
		f.ctx,
		10,
		contracts.PendingQuery{},
	); err != nil ||
		len(evs) != 3 {
		t.Fatalf("%d pending after an ack (%v)", len(evs), err)
	}
}

func keyOf(t *testing.T, pe contracts.PendingEvent) string {
	t.Helper()
	var ev contracts.BucketEvent
	if err := json.Unmarshal(pe.Body, &ev); err != nil {
		t.Fatal(err)
	}
	return ev.Key
}

// A data directory v0.12.0 left, which has no outbox, opens: Init makes
// the outbox, RecoverEvents finds nothing, and its keys, written in place
// and as versions, read and record events once configured.
func TestDataDirWithoutAnOutbox(t *testing.T) {
	f := newEventFixture(t)
	f.put("legacy", "l")
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	f.release()
	if err := f.s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.root, eventsDir)); err != nil {
		t.Fatal(err)
	}
	f.fixture = pinnedFixture(t, openEventService(t, f.root, f.events()))
	requireOpened(f, v1.VersionID)
	f.configure()
	f.del(vref("legacy", contracts.VersionNull))
	requireEvents(t, contracts.VersioningEnabled, f.pending(), &contracts.BucketEvent{
		Name: contracts.EventObjectRemovedDelete, Bucket: "bucket", Key: "legacy",
		VersionID: contracts.VersionNull,
	})
}

// requireOpened fails unless f's store, opened on a root without an
// outbox, made one, finds no events to recover, and reads "legacy" and
// version v1 of "k".
func requireOpened(f *eventFixture, v1 string) {
	f.t.Helper()
	if !f.exists(eventsDir) {
		f.t.Fatal("Init did not make the outbox")
	}
	out, err := f.s.RecoverEvents(f.ctx)
	if err != nil || out != (contracts.RecoveredEvents{}) {
		f.t.Fatalf("recovered %+v, %v", out, err)
	}
	if f.body(oref("bucket", "legacy")) != "l" || f.body(vref("k", v1)) != "v1" {
		f.t.Fatal("the data did not read back")
	}
}

// A directory sync that fails after writeDurable's rename does not undo
// the write readers already see: a fresh key's version stays readable and
// indexed, its manifest lists it, its event is confirmed, a delete of it
// and a change of the versioning state happen too.
func TestAFailedSyncAfterTheRenameIsCommitted(t *testing.T) {
	f := newEventFixture(t)
	f.state(contracts.VersioningEnabled)
	f.configure()
	errSync := errors.New("sync")
	f.s.hook = func(stage string) error {
		if stage == stageDirSync {
			return errSync
		}
		return nil
	}
	v1 := f.put("k", "v1")
	if f.body(vref("k", v1.VersionID)) != "v1" || !f.exists(indexEntry("bucket", "k")) {
		t.Fatal("the committed version did not read back")
	}
	f.requireIDs(v1.VersionID)
	if len(f.intents()) != 0 {
		t.Fatalf("outbox %v", f.outbox())
	}
	requireEvents(t, contracts.VersioningEnabled, f.pending(), &contracts.BucketEvent{
		Name: contracts.EventObjectCreatedPut, Bucket: "bucket", Key: "k",
		VersionID: v1.VersionID, ETag: v1.ETag, Size: 2, HasObject: true,
	})

	f.del(vref("k", v1.VersionID))
	if f.hasManifest() || len(f.ids("k")) != 0 {
		t.Fatal("the deleted version is left")
	}
	requireEvents(t, contracts.VersioningEnabled, f.pending(), &contracts.BucketEvent{
		Name: contracts.EventObjectRemovedDelete, Bucket: "bucket", Key: "k",
		VersionID: v1.VersionID,
	})

	f.state(contracts.VersioningSuspended)
	st, err := f.s.GetBucketVersioning(f.ctx, "bucket")
	if err != nil || st.Status != contracts.VersioningSuspended {
		t.Fatalf("versioning %+v, %v", st, err)
	}
}

// onlyIntent returns the id of the one intent of "bucket"'s outbox.
func (f *eventFixture) onlyIntent() string {
	f.t.Helper()
	intents := f.intents()
	if len(intents) != 1 {
		f.t.Fatalf("outbox %v", f.outbox())
	}
	return strings.TrimSuffix(intents[0], intentExt)
}

// An intent marked aborted is removed at recovery without its witness
// checked, also when a later change made the witness hold.
func TestAnAbortedIntentIsRemoved(t *testing.T) {
	f := newEventFixture(t)
	f.put("k", "v1")
	f.configure()
	tags := []contracts.Tag{{Key: "t", Value: "a"}}
	crashAt(f.fixture, stageEventIntent)
	_, err := f.s.PutObjectTagging(f.ctx, oref("bucket", "k"), tags, nil)
	requireIs(t, "the stopped tagging", err, errCrash)
	id := f.onlyIntent()
	if err = f.s.root.Rename(eventFile("bucket", id, intentExt),
		eventFile("bucket", id, abortedExt)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PutObjectTagging(f.ctx, oref("bucket", "k"), tags, nil); err != nil {
		t.Fatal(err)
	}
	f.restart()
	out, err := f.s.RecoverEvents(f.ctx)
	if err != nil || out != (contracts.RecoveredEvents{Dropped: 1}) {
		t.Fatalf("recovered %+v, %v", out, err)
	}
	if evs := f.pending(); len(evs) != 1 || len(f.outbox()) != 0 {
		t.Fatalf("events %+v, outbox %v", evs, f.outbox())
	}
}

// A stop between writing an event's committed mark and removing its
// intent leaves both: recovery removes the intent, and ConfirmCommitted
// confirms the mark, once.
func TestACrashAfterTheCommittedMark(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	crashAt(f.fixture, stageCommitted)
	_, err := writeOps["put"](f.fixture)
	requireIs(t, "the stopped put", err, errCrash)
	id := f.onlyIntent()
	rec, err := f.s.readIntent(eventFile("bucket", id, intentExt), id)
	if err != nil {
		t.Fatal(err)
	}
	rec.Witness = witness{Kind: witnessCommitted}
	rec.Event.ETag, rec.Event.Size = f.stat().ETag, 3
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.root.WriteFile(eventFile("bucket", id, committedExt), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	f.restart()
	out, err := f.s.RecoverEvents(f.ctx)
	if err != nil || out != (contracts.RecoveredEvents{}) || len(f.intents()) != 0 {
		t.Fatalf("recovered %+v, %v; outbox %v", out, err, f.outbox())
	}
	f.confirmCommitted(1)
	f.confirmCommitted(0)
	requireEvents(t, contracts.Unversioned, f.pending(), &contracts.BucketEvent{
		Name: contracts.EventObjectCreatedPut, Bucket: "bucket", Key: "k",
		ETag: f.stat().ETag, Size: 3, HasObject: true,
	})
}

// A renamed mark of a write in place whose object was written over since
// cannot read its details back: its event leaves the object's size and
// ETag out rather than report wrong ones.
func TestARenamedMarkWithoutItsObject(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	crashAt(f.fixture, stageCommitted)
	_, err := writeOps["put"](f.fixture)
	requireIs(t, "the stopped put", err, errCrash)
	id := f.onlyIntent()
	if err = f.s.root.Rename(eventFile("bucket", id, intentExt),
		eventFile("bucket", id, committedExt)); err != nil {
		t.Fatal(err)
	}
	f.put("k", "later")
	f.confirmCommitted(1)
	evs := f.pending()
	if len(evs) != 2 {
		t.Fatalf("events %+v", evs)
	}
	requireEvent(t, &evs[0], &contracts.BucketEvent{
		Name: contracts.EventObjectCreatedPut, Bucket: "bucket", Key: "k",
	})
}

// The sequence lease resumes across a restart: on a clock behind the one
// events were recorded on, a key never written before still sorts after
// them.
func TestTheLeaseResumesAcrossARestart(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	f.s.seq.Store(time.Now().Add(time.Hour).UnixNano())
	f.put("a", "x")
	first := f.pending()[0].Sequence
	f.restart()
	if got := f.s.seq.Load(); got < first+seqLeaseAhead {
		t.Fatalf("resumed at %d, the lease is %d", got, first+seqLeaseAhead)
	}
	f.put("b", "x")
	if evs := f.pending(); len(evs) != 1 || evs[0].Sequence <= first {
		t.Fatalf("events %+v after sequence %d", evs, first)
	}
}

// A lease that cannot be read or decoded does not keep the store from
// starting: it resumes from the clock, and the next event stores a new
// lease.
func TestADamagedLeaseDoesNotBlockTheStart(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	f.put("a", "x")
	f.pending()
	if err := f.s.root.WriteFile(seqLeaseFile, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.restart()
	if f.s.leased.Load() != 0 {
		t.Fatal("resumed from a damaged lease")
	}
	f.put("b", "x")
	if len(f.pending()) != 1 {
		t.Fatal("no event after a damaged lease")
	}
	raw, err := f.s.root.ReadFile(seqLeaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = strconv.ParseInt(string(raw), 10, 64); err != nil {
		t.Fatalf("lease %q: %v", raw, err)
	}

	if err = f.s.root.Remove(seqLeaseFile); err != nil {
		t.Fatal(err)
	}
	if err = f.s.root.Mkdir(seqLeaseFile, 0o750); err != nil {
		t.Fatal(err)
	}
	f.restart() // an unreadable lease
	f.put("c", "x")
	if len(f.pending()) != 1 {
		t.Fatal("no event after an unreadable lease")
	}
}
