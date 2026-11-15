package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/config"
	"github.com/gasmod/gas/config/configtest"
	gaszerolog "github.com/gasmod/gas/log/zerolog"
	"github.com/rs/zerolog"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/notification"
	"tule.dev/object-store/internal/services/storage/dirimpl"
)

const testAccount = "000000000000"

// fakeClock is a Clock that moves only when the test advances it, from the
// system's time, which the storage stamps an event's Created with.
type fakeClock struct {
	now     time.Time
	waiters []waiter
	mu      sync.Mutex
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Now()}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, waiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock on by d and fires every wait that has ended.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = kept
}

// logBuffer is a log output tests read back.
type logBuffer struct {
	b  bytes.Buffer
	mu sync.Mutex
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p) //nolint:wrapcheck // a bytes.Buffer write never fails
}

// lines returns the log lines at level that hold msg.
func (l *logBuffer) lines(level, msg string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for line := range strings.SplitSeq(l.b.String(), "\n") {
		if strings.Contains(line, `"level":"`+level+`"`) &&
			strings.Contains(line, `"message":"`+msg+`"`) {
			out = append(out, line)
		}
	}
	return out
}

// delivery is a call of fakeDests' Deliver.
type delivery struct {
	src  contracts.SourceRef
	dest string
	key  string
}

// fakeDests records deliveries, which fn, when set, answers.
type fakeDests struct {
	fn       func(ctx context.Context, bucket, key string) error
	inFlight map[string]int
	calls    []delivery
	// maxInFlight is the most deliveries of one bucket at once.
	maxInFlight int
	mu          sync.Mutex
}

func (d *fakeDests) Validate(
	context.Context, contracts.Destination, []byte, contracts.SourceRef,
) error {
	return nil
}

func (d *fakeDests) Deliver(
	ctx context.Context, dest contracts.Destination, body []byte, src contracts.SourceRef,
) error {
	key, _ := describe(body)
	bucket := strings.TrimPrefix(src.ARN, "arn:aws:s3:::")
	d.mu.Lock()
	d.inFlight[bucket]++
	d.maxInFlight = max(d.maxInFlight, d.inFlight[bucket])
	fn := d.fn
	d.mu.Unlock()
	var err error
	if fn != nil {
		err = fn(ctx, bucket, key)
	}
	d.mu.Lock()
	d.inFlight[bucket]--
	if err == nil {
		d.calls = append(d.calls, delivery{src: src, dest: dest.ARN, key: key})
	}
	d.mu.Unlock()
	return err
}

func (d *fakeDests) setFn(fn func(ctx context.Context, bucket, key string) error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fn = fn
}

// delivered returns the keys delivered, of bucket when it is set, in the
// order they were.
func (d *fakeDests) delivered(bucket string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var keys []string
	for _, c := range d.calls {
		if bucket == "" || c.src.ARN == "arn:aws:s3:::"+bucket {
			keys = append(keys, c.key)
		}
	}
	return keys
}

type fixture struct {
	t      *testing.T
	ctx    context.Context //nolint:containedctx // the test's
	dir    *dirimpl.Service
	dests  *fakeDests
	svc    *Service
	clock  *fakeClock
	logs   *logBuffer
	ready  chan struct{}
	signal chan struct{}
	root   string
	// logger and settings are what svc was built with (see useStore).
	logger   gas.Logger
	settings Settings
}

// useStore builds the service again over store, not started.
func (f *fixture) useStore(store contracts.StorageProvider) {
	f.svc = NewService(f.settings)(f.logger, store, f.dests)
}

// newFixture returns a deliverer, not started, on settings, over a fresh
// storage whose events go to queues, and which signals the deliverer
// unless quiet is set.
func newFixture(t *testing.T, settings Settings, quiet bool) *fixture {
	t.Helper()
	cfg := config.New(config.WithProvider(configtest.NewStaticProvider(nil)))
	if err := cfg.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}
	f := &fixture{
		t: t, ctx: context.Background(), clock: newFakeClock(), logs: &logBuffer{},
		ready: make(chan struct{}), signal: make(chan struct{}, 1),
		dests: &fakeDests{inFlight: map[string]int{}}, root: t.TempDir(),
	}
	events := dirimpl.Events{Router: notification.NewRouter(testAccount,
		map[contracts.DestinationType]bool{contracts.DestinationQueue: true})}
	if !quiet {
		events.Signal = func() {
			select {
			case f.signal <- struct{}{}:
			default:
			}
		}
	}
	f.dir = dirimpl.NewService(&dirimpl.Settings{RootDir: f.root}, contracts.SystemClock,
		events)(cfg, &gas.NopLogger{})
	if err := f.dir.Init(f.ctx); err != nil {
		t.Fatalf("init storage: %v", err)
	}
	t.Cleanup(func() {
		if err := f.dir.Close(f.ctx); err != nil {
			t.Errorf("close storage: %v", err)
		}
	})
	zl := zerolog.New(f.logs)
	logger, err := gaszerolog.New(gaszerolog.WithLogger(&zl))()
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	settings.Clock, settings.Ready, settings.Signal = f.clock, f.ready, f.signal
	settings.Account = testAccount
	f.logger, f.settings = logger, settings
	f.svc = NewService(settings)(logger, f.dir, f.dests)
	return f
}

// queueOf is the queue bucket's events go to.
func queueOf(bucket string) string {
	return "arn:aws:sqs:us-east-1:" + testAccount + ":q-" + bucket
}

// bucket creates bucket, configured to send every created event to its
// queue.
func (f *fixture) bucket(name string) {
	f.t.Helper()
	if _, err := f.dir.CreateBucket(f.ctx, name, nil); err != nil {
		f.t.Fatalf("create bucket: %v", err)
	}
	f.configure(name, `<NotificationConfiguration><QueueConfiguration>`+
		`<Id>all</Id><Queue>`+queueOf(name)+`</Queue><Event>s3:ObjectCreated:*</Event>`+
		`</QueueConfiguration></NotificationConfiguration>`)
}

// configure gives bucket the notification configuration doc.
func (f *fixture) configure(name, doc string) {
	f.t.Helper()
	cfg, err := notification.Parse([]byte(doc))
	if err != nil {
		f.t.Fatalf("parse: %v", err)
	}
	ref := f.ref(name)
	stored, err := notification.Encode(ref.Generation, cfg)
	if err != nil {
		f.t.Fatalf("encode: %v", err)
	}
	ctx, release, err := f.dir.Pin(f.ctx, ref)
	if err != nil {
		f.t.Fatalf("pin: %v", err)
	}
	defer release()
	if err = f.dir.PutBucketConfig(ctx, name, contracts.ConfigNotification, stored); err != nil {
		f.t.Fatalf("configure: %v", err)
	}
}

func (f *fixture) ref(bucket string) contracts.BucketRef {
	f.t.Helper()
	gen, _, err := f.dir.GetBucketPolicy(f.ctx, bucket)
	if err != nil && !errors.Is(err, contracts.ErrNoSuchBucketPolicy) {
		f.t.Fatalf("read bucket %s: %v", bucket, err)
	}
	return contracts.BucketRef{Name: bucket, Generation: gen}
}

// put writes each of keys into bucket, which records its event.
func (f *fixture) put(bucket string, keys ...string) {
	f.t.Helper()
	ctx, release, err := f.dir.Pin(f.ctx, f.ref(bucket))
	if err != nil {
		f.t.Fatalf("pin: %v", err)
	}
	defer release()
	for _, k := range keys {
		if _, err = f.dir.PutObject(ctx, bucket, k, strings.NewReader("x"),
			contracts.PutObjectInput{}, contracts.Precondition{}); err != nil {
			f.t.Fatalf("put %s: %v", k, err)
		}
	}
}

// pending returns how many events are pending.
func (f *fixture) pending() int {
	f.t.Helper()
	evs, err := f.dir.PendingEvents(f.ctx, 1000, contracts.PendingQuery{})
	if err != nil {
		f.t.Fatal(err)
	}
	return len(evs)
}

// pass runs one pass, waits for the delivery runs it started, and
// returns what the pass did.
func (f *fixture) pass() time.Time {
	f.t.Helper()
	f.svc.run.begin()
	next := f.svc.pass(f.svc.run.ctx)
	eventually(f.t, "the runs of the pass", func() bool {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		return f.svc.running == 0
	})
	if err := f.svc.run.stop(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	return next
}

// nextRetry is when the next retry is due, once no run is under way.
func (f *fixture) nextRetry() time.Time {
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	return f.svc.nextRetry()
}

// start starts the service, and closes it at the end of the test.
func (f *fixture) start() {
	f.t.Helper()
	if err := f.svc.Init(f.ctx); err != nil {
		f.t.Fatalf("init: %v", err)
	}
	f.t.Cleanup(func() { f.close() })
}

func (f *fixture) close() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.svc.Close(ctx); err != nil {
		f.t.Errorf("close: %v", err)
	}
}

// eventually fails the test unless cond holds within 5 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 5s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// never fails the test if cond holds within d.
func never(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("%s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Nothing is delivered before Ready; then the storage's signal wakes the
// deliverer at once, the clock never moving.
func TestReadyGatesAndSignalWakes(t *testing.T) {
	f := newFixture(t, Settings{}, false)
	f.bucket("photos")
	f.put("photos", "a")
	f.start()
	never(t, "delivered before Ready", 50*time.Millisecond,
		func() bool { return len(f.dests.delivered("")) > 0 })
	close(f.ready)
	eventually(t, "the first delivery", func() bool { return len(f.dests.delivered("")) == 1 })
	f.put("photos", "b")
	eventually(t, "the signaled delivery", func() bool { return len(f.dests.delivered("")) == 2 })
	eventually(t, "the outbox emptied", func() bool { return f.pending() == 0 })
	got := f.dests.calls[0]
	if got.dest != queueOf("photos") ||
		got.src != (contracts.SourceRef{ARN: "arn:aws:s3:::photos", Account: testAccount}) {
		t.Fatalf("delivery %+v", got)
	}
}

// Without a signal, the poll finds the event.
func TestPollFindsEventsWithoutASignal(t *testing.T) {
	f := newFixture(t, Settings{}, true)
	f.bucket("photos")
	close(f.ready)
	f.start()
	eventually(t, "the first wait", func() bool { return f.waiting() })
	f.put("photos", "a")
	never(t, "delivered before the poll", 50*time.Millisecond,
		func() bool { return len(f.dests.delivered("")) > 0 })
	f.clock.Advance(defaultPoll)
	eventually(t, "the polled delivery", func() bool { return len(f.dests.delivered("")) == 1 })
}

// waiting reports whether the loop waits on the clock.
func (f *fixture) waiting() bool {
	f.clock.mu.Lock()
	defer f.clock.mu.Unlock()
	return len(f.clock.waiters) > 0
}

// Each bucket has one delivery in flight at a time, in the order its
// events were recorded, while buckets are delivered at once.
func TestOneInFlightPerBucketInOrder(t *testing.T) {
	f := newFixture(t, Settings{}, true)
	keys := []string{"k1", "k2", "k3", "k4", "k5"}
	for _, b := range []string{"aaa", "bbb"} {
		f.bucket(b)
		f.put(b, keys...)
	}
	// The first delivery of each bucket waits until both are in flight,
	// which only parallel buckets allow.
	var both sync.WaitGroup
	both.Add(2)
	var once [2]sync.Once
	f.dests.setFn(func(_ context.Context, bucket, key string) error {
		if key == "k1" {
			once[map[string]int{"aaa": 0, "bbb": 1}[bucket]].Do(both.Done)
			both.Wait()
		}
		return nil
	})
	f.pass()
	for _, b := range []string{"aaa", "bbb"} {
		if got := f.dests.delivered(b); !slices.Equal(got, keys) {
			t.Fatalf("%s delivered %v, want %v", b, got, keys)
		}
	}
	if f.dests.maxInFlight != 1 {
		t.Fatalf("%d deliveries of a bucket at once", f.dests.maxInFlight)
	}
	if n := f.pending(); n != 0 {
		t.Fatalf("%d pending", n)
	}
}

// At most Workers buckets are delivered at once; the others wait for the
// next pass, which starts after the last bucket started, so every bucket
// is served.
func TestWorkersBoundTheBuckets(t *testing.T) {
	f := newFixture(t, Settings{Workers: 2}, true)
	for _, b := range []string{"aaa", "bbb", "ccc"} {
		f.bucket(b)
		f.put(b, "k")
	}
	f.pass()
	if got := f.dests.delivered(""); len(got) != 2 {
		t.Fatalf("first pass delivered %d, want 2", len(got))
	}
	f.pass()
	if got := f.dests.delivered("ccc"); len(got) != 1 {
		t.Fatalf("ccc delivered %d in the second pass, want 1", len(got))
	}
}

// A failing event does not hold back the bucket's later ones, and is
// retried after 1s, 2s, 4s ... up to 60s, until its 10th failure drops it.
func TestBackoffAndMaxAttempts(t *testing.T) {
	f := newFixture(t, Settings{}, true)
	f.bucket("photos")
	f.put("photos", "bad", "good1", "good2")
	failErr := errors.New("connection refused")
	f.dests.setFn(func(_ context.Context, _, key string) error {
		if key == "bad" {
			return failErr
		}
		return nil
	})
	f.pass()
	if got := f.dests.delivered(""); !slices.Equal(got, []string{"good1", "good2"}) {
		t.Fatalf("delivered %v behind a failing event", got)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		if d := f.nextRetry().Sub(f.clock.Now()); d != w*time.Second {
			t.Fatalf("wait after failure %d: %v, want %v", i+1, d, w*time.Second)
		}
		// Not due yet: the pass delivers nothing.
		f.clock.Advance(w*time.Second - time.Millisecond)
		f.pass()
		if a := f.svc.attempts[f.only(t)]; a.n != i+1 {
			t.Fatalf("attempt %d ran early: %+v", i+1, a)
		}
		f.clock.Advance(time.Millisecond)
		f.pass()
	}
	if n := f.pending(); n != 0 {
		t.Fatalf("%d pending after 10 failures", n)
	}
	f.requireDropLine(`"reason":"`+reasonAttempts+`"`, `"count":1`, "connection refused",
		`"first_key":"bad"`)
	if len(f.svc.attempts) != 0 {
		t.Fatalf("attempts kept: %v", f.svc.attempts)
	}
}

// requireDropLine fails unless one drop line is logged at Warn, holding
// each of want.
func (f *fixture) requireDropLine(want ...string) {
	f.t.Helper()
	lines := f.logs.lines("warn", "dropped bucket events")
	if len(lines) != 1 {
		f.t.Fatalf("drop logs %v, want one", lines)
	}
	for _, w := range want {
		if !strings.Contains(lines[0], w) {
			f.t.Fatalf("drop log %s, without %s", lines[0], w)
		}
	}
}

// attemptCounts returns the failed deliveries counted of each event.
func (f *fixture) attemptCounts() map[eventKey]int {
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	out := map[eventKey]int{}
	for k, a := range f.svc.attempts {
		out[k] = a.n
	}
	return out
}

// only returns the key of the one attempt.
func (f *fixture) only(t *testing.T) eventKey {
	t.Helper()
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	if len(f.svc.attempts) != 1 {
		t.Fatalf("%d attempts", len(f.svc.attempts))
	}
	for k := range f.svc.attempts {
		return k
	}
	return eventKey{}
}

// A message the destination refuses, and a destination whose service is
// not configured, are dropped at once.
func TestPermanentFailuresDropAtOnce(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{fmt.Errorf("%w: too large", contracts.ErrDestinationRejected), reasonRejected},
		{fmt.Errorf("%w: q", contracts.ErrDestinationUnconfigured), reasonUnconfigured},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			f := newFixture(t, Settings{}, true)
			f.bucket("photos")
			f.put("photos", "a", "b")
			f.dests.setFn(func(context.Context, string, string) error { return tc.err })
			f.pass()
			if n := f.pending(); n != 0 {
				t.Fatalf("%d pending", n)
			}
			lines := f.logs.lines("warn", "dropped bucket events")
			if len(lines) != 1 || !strings.Contains(lines[0], `"count":2`) ||
				!strings.Contains(lines[0], `"reason":"`+tc.reason+`"`) {
				t.Fatalf("drop logs %v", lines)
			}
			if n := len(f.logs.lines("debug", "dropped a bucket event")); n != 2 {
				t.Fatalf("%d debug drop lines, want 2", n)
			}
		})
	}
}

// Events recorded more than MaxAge ago are dropped when read, without a
// delivery, and a backlog of them is logged in one Warn line a Poll, not
// one an event.
func TestOldEventsAreDroppedAndSummarized(t *testing.T) {
	f := newFixture(t, Settings{Batch: 10}, true)
	f.bucket("photos")
	keys := make([]string, 35)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%02d", i)
	}
	f.put("photos", keys...)
	f.clock.Advance(defaultMaxAge + time.Second)
	for range 4 {
		f.pass()
	}
	if n := f.pending(); n != 0 {
		t.Fatalf("%d pending", n)
	}
	if got := f.dests.delivered(""); len(got) != 0 {
		t.Fatalf("delivered %v", got)
	}
	// The first drops are logged at once, and the rest of the backlog,
	// dropped within the same Poll, in one line once it is over.
	lines := f.logs.lines("warn", "dropped bucket events")
	if len(lines) != 1 || !strings.Contains(lines[0], `"count":10`) {
		t.Fatalf("drop logs within the Poll %v", lines)
	}
	f.clock.Advance(defaultPoll)
	f.pass()
	lines = f.logs.lines("warn", "dropped bucket events")
	if len(lines) != 2 || !strings.Contains(lines[1], `"count":25`) ||
		!strings.Contains(lines[1], `"reason":"`+reasonAge+`"`) {
		t.Fatalf("drop logs %v", lines)
	}
}

// Drops a trickle of passes makes within a Poll are logged in one line,
// and Close logs those not yet logged.
func TestDropsAcrossPassesAreLoggedOnceAPoll(t *testing.T) {
	f := newFixture(t, Settings{}, true)
	f.bucket("photos")
	f.dests.setFn(func(context.Context, string, string) error {
		return fmt.Errorf("%w: too large", contracts.ErrDestinationRejected)
	})
	for i := range 5 {
		f.put("photos", fmt.Sprintf("k%d", i))
		f.pass()
		f.clock.Advance(defaultPoll / 10)
	}
	if n := len(f.logs.lines("warn", "dropped bucket events")); n != 1 {
		t.Fatalf("%d drop lines within a Poll, want the first", n)
	}
	f.clock.Advance(defaultPoll)
	f.pass()
	lines := f.logs.lines("warn", "dropped bucket events")
	if len(lines) != 2 || !strings.Contains(lines[1], `"count":4`) {
		t.Fatalf("drop logs a Poll later %v", lines)
	}
	f.put("photos", "last")
	f.pass()
	f.put("photos", "after")
	f.pass()
	f.close()
	lines = f.logs.lines("warn", "dropped bucket events")
	if len(lines) != 3 || !strings.Contains(lines[2], `"count":2`) {
		t.Fatalf("drop logs after Close %v", lines)
	}
}

// A full batch that dropped events asks for the next pass at once, one
// that was not full for none.
func TestABacklogDrainsWithoutWaiting(t *testing.T) {
	f := newFixture(t, Settings{Batch: 2}, true)
	f.bucket("photos")
	f.put("photos", "a", "b", "c")
	f.clock.Advance(defaultMaxAge + time.Second)
	if next := f.pass(); !next.Equal(f.clock.Now()) {
		t.Fatalf("next pass at %v, want now", next)
	}
	if next := f.pass(); !next.IsZero() {
		t.Fatalf("next pass at %v after the backlog, want none", next)
	}
}

// Close cancels the delivery in flight, which stays in the outbox and is
// not counted as a failure; Init starts again, a fresh run, with the
// attempts kept, and delivers it.
func TestCloseLeavesTheEventAndInitKeepsAttempts(t *testing.T) {
	f := newFixture(t, Settings{}, false)
	f.bucket("photos")
	f.put("photos", "flaky", "hang")
	close(f.ready)
	f.dests.setFn(func(ctx context.Context, _, key string) error {
		if key == "flaky" {
			return errors.New("refused")
		}
		<-ctx.Done()
		return ctx.Err()
	})
	f.start()
	eventually(t, "the flaky failure", func() bool {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		return len(f.svc.attempts) == 1
	})
	eventually(t, "the hanging delivery", func() bool {
		f.dests.mu.Lock()
		defer f.dests.mu.Unlock()
		return f.dests.inFlight["photos"] == 1
	})
	f.close()
	if n := f.pending(); n != 2 {
		t.Fatalf("%d pending after Close, want 2", n)
	}
	kept := f.attemptCounts()
	if len(kept) != 1 || kept[f.only(t)] != 1 {
		t.Fatalf("attempts after Close: %v, want the flaky one's first", kept)
	}
	f.dests.setFn(nil)
	if err := f.svc.Init(f.ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the hanging event delivered", func() bool { return f.pending() == 1 })
	if got := f.dests.delivered(""); !slices.Equal(got, []string{"hang"}) {
		t.Fatalf("delivered %v, want hang once", got)
	}
	if got := f.attemptCounts(); !maps.Equal(got, kept) {
		t.Fatalf("attempts after Init: %v, want %v kept", got, kept)
	}
	f.clock.Advance(defaultBackoff)
	eventually(t, "the flaky event retried", func() bool { return f.pending() == 0 })
	f.close()
}

// A pass removes the empty outbox of a deleted bucket, at most once every
// Poll, and keeps a live bucket's.
func TestPassesRemoveEmptyOutboxes(t *testing.T) {
	f := newFixture(t, Settings{}, true)
	f.bucket("photos")
	f.bucket("gone")
	f.put("photos", "a")
	f.put("gone", "a")
	f.pass()
	f.deleteBucket("gone", "a")
	f.pass()
	if !f.hasOutbox("gone") {
		t.Fatal("the outbox was removed within the Poll")
	}
	f.clock.Advance(defaultPoll)
	f.pass()
	if f.hasOutbox("gone") {
		t.Fatal("the deleted bucket's outbox was left a Poll later")
	}
	if !f.hasOutbox("photos") {
		t.Fatal("the live bucket's empty outbox was removed")
	}
}

// deleteBucket deletes each of keys of bucket, then bucket.
func (f *fixture) deleteBucket(bucket string, keys ...string) {
	f.t.Helper()
	ref := f.ref(bucket)
	ctx, release, err := f.dir.Pin(f.ctx, ref)
	if err != nil {
		f.t.Fatalf("pin: %v", err)
	}
	for _, k := range keys {
		if _, err = f.dir.DeleteObject(
			ctx,
			contracts.ObjectRef{Bucket: bucket, Key: k},
		); err != nil {
			f.t.Fatalf("delete %s: %v", k, err)
		}
	}
	release()
	if err = f.dir.DeleteBucket(f.ctx, ref); err != nil {
		f.t.Fatalf("delete bucket %s: %v", bucket, err)
	}
}

// failingAcks is a storage whose AckEvent always fails, as an outbox that
// cannot be written does, and counts the calls.
type failingAcks struct {
	contracts.StorageProvider
	acks atomic.Int32
}

func (s *failingAcks) AckEvent(context.Context, string, string) error {
	s.acks.Add(1)
	return errors.New("permission denied")
}

// An acknowledgement that keeps failing is retried with the delivery
// backoff, not on every pass, and the event is not delivered again.
func TestFailedAcksBackOff(t *testing.T) {
	f := newFixture(t, Settings{}, true)
	store := &failingAcks{StorageProvider: f.dir}
	f.useStore(store)
	f.bucket("photos")
	f.put("photos", "a")
	// A minute of passes every 100ms: acks at 0s, 1s, 3s, 7s, 15s, 31s.
	for range 600 {
		f.pass()
		f.clock.Advance(100 * time.Millisecond)
	}
	if n := store.acks.Load(); n < 5 || n > 7 {
		t.Fatalf("%d acknowledgements in a minute, want about 6", n)
	}
	if got := f.dests.delivered(""); len(got) != 1 {
		t.Fatalf("delivered %v, want once", got)
	}
	if n := len(
		f.logs.lines("error", "failed to remove a settled bucket event from the outbox"),
	); n != int(
		store.acks.Load(),
	) {
		t.Fatalf("%d error lines for %d failed acknowledgements", n, store.acks.Load())
	}
}

// A delivered event whose acknowledgement keeps failing is never dropped
// for its age: past MaxAge, it is still only acknowledged again, with the
// backoff, and not delivered again.
func TestASettledEventIsNotDroppedForAge(t *testing.T) {
	f := newFixture(t, Settings{}, true)
	store := &failingAcks{StorageProvider: f.dir}
	f.useStore(store)
	f.bucket("photos")
	f.put("photos", "a")
	f.pass()
	f.clock.Advance(defaultMaxAge + time.Second)
	before := store.acks.Load()
	// A minute of passes every 100ms: the backoff goes on from the first
	// failed acknowledgement, 2s, 4s, 8s, 16s apart, so about five.
	for range 600 {
		f.pass()
		f.clock.Advance(100 * time.Millisecond)
	}
	if n := store.acks.Load() - before; n < 4 || n > 6 {
		t.Fatalf("%d acknowledgements in a minute past MaxAge, want about five", n)
	}
	if lines := f.logs.lines("warn", "dropped bucket events"); len(lines) != 0 {
		t.Fatalf("a settled event was dropped: %v", lines)
	}
	if lines := f.logs.lines("debug", "dropped a bucket event"); len(lines) != 0 {
		t.Fatalf("a settled event was dropped: %v", lines)
	}
	if got := f.dests.delivered(""); len(got) != 1 {
		t.Fatalf("delivered %v, want once", got)
	}
}

// bucketTo creates bucket, configured to send the created events of keys
// under each prefix to that prefix's queue.
func (f *fixture) bucketTo(bucket string, prefixes ...string) {
	f.t.Helper()
	if _, err := f.dir.CreateBucket(f.ctx, bucket, nil); err != nil {
		f.t.Fatalf("create bucket: %v", err)
	}
	doc := "<NotificationConfiguration>"
	for _, p := range prefixes {
		doc += `<QueueConfiguration><Id>` + p + `</Id><Queue>` + queueOf(p) + `</Queue>` +
			`<Event>s3:ObjectCreated:*</Event><Filter><S3Key><FilterRule><Name>prefix</Name>` +
			`<Value>` + p + `/</Value></FilterRule></S3Key></Filter></QueueConfiguration>`
	}
	f.configure(bucket, doc+"</NotificationConfiguration>")
}

// Events waiting for a retry do not take up the batch: past a full batch
// of them, the bucket's event for another destination is read and
// delivered at once.
func TestRetriesDoNotHoldBackTheBatch(t *testing.T) {
	f := newFixture(t, Settings{}, true)
	f.bucketTo("photos", "down", "up")
	keys := make([]string, 150)
	for i := range keys {
		keys[i] = fmt.Sprintf("down/k%03d", i)
	}
	f.put("photos", keys...)
	f.put("photos", "up/k")
	f.dests.setFn(func(_ context.Context, _, key string) error {
		if strings.HasPrefix(key, "down/") {
			return errors.New("connection refused")
		}
		return nil
	})
	f.pass()
	f.pass()
	if got := f.dests.delivered(""); !slices.Equal(got, []string{"up/k"}) {
		t.Fatalf("delivered %v behind 150 retrying events, want up/k", got)
	}
	if n := len(f.attemptCounts()); n != 150 {
		t.Fatalf("%d events retrying, want 150", n)
	}
}

// With more buckets holding events than a batch reads, each pass reads
// from the bucket after the last one the previous read reached, so the
// later buckets are served without waiting for the earlier ones.
func TestBucketsBeyondTheBatchAreServed(t *testing.T) {
	f := newFixture(t, Settings{Batch: 10, Workers: 1}, true)
	buckets := make([]string, 25)
	for i := range buckets {
		buckets[i] = fmt.Sprintf("b%02d", i)
		f.bucket(buckets[i])
		f.put(buckets[i], "k")
	}
	for range 3 {
		f.pass()
	}
	served := map[string]bool{}
	for _, c := range f.dests.calls {
		served[strings.TrimPrefix(c.src.ARN, "arn:aws:s3:::")] = true
	}
	if !served["b00"] || !served["b10"] || !served["b20"] {
		t.Fatalf("served %v in three passes, want b00, b10 and b20", served)
	}
	for range 25 {
		f.pass()
	}
	if n := f.pending(); n != 0 {
		t.Fatalf("%d pending", n)
	}
}

// hasOutbox reports whether bucket's outbox directory exists.
func (f *fixture) hasOutbox(bucket string) bool {
	f.t.Helper()
	_, err := os.Stat(filepath.Join(f.root, ".tule", "events", bucket))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		f.t.Fatal(err)
	}
	return err == nil
}

// An event a run holds is never dropped for its age meanwhile: the
// buckets with a run under way are not read, so the age is checked on a
// pass after the run.
func TestAHeldEventIsNotDroppedForAge(t *testing.T) {
	f := newFixture(t, Settings{}, true)
	f.bucket("photos")
	f.put("photos", "a")
	release := make(chan struct{})
	f.dests.setFn(func(context.Context, string, string) error {
		<-release
		return nil
	})
	f.svc.run.begin()
	f.svc.pass(f.svc.run.ctx)
	eventually(t, "the delivery in flight", func() bool {
		f.dests.mu.Lock()
		defer f.dests.mu.Unlock()
		return f.dests.inFlight["photos"] == 1
	})
	f.clock.Advance(defaultMaxAge + time.Second)
	f.svc.pass(f.svc.run.ctx)
	close(release)
	eventually(t, "the run's end", func() bool {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		return f.svc.running == 0
	})
	if err := f.svc.run.stop(f.ctx); err != nil {
		t.Fatal(err)
	}
	if lines := f.logs.lines("debug", "dropped a bucket event"); len(lines) != 0 {
		t.Fatalf("a held event was dropped: %v", lines)
	}
	if got := f.dests.delivered(""); !slices.Equal(got, []string{"a"}) || f.pending() != 0 {
		t.Fatalf("delivered %v, %d pending; want a once", got, f.pending())
	}
}
