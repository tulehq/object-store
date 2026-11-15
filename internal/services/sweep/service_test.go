package sweep

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/config"
	"github.com/gasmod/gas/config/configtest"
	gaszerolog "github.com/gasmod/gas/log/zerolog"
	"github.com/gasmod/gas/storage"
	"github.com/rs/zerolog"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/lifecycle"
	"tule.dev/object-store/internal/services/storage/dirimpl"
)

// fakeClock is a Clock that moves only when the test advances it.
type fakeClock struct {
	now     time.Time
	waiters []waiter
	mu      sync.Mutex
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

// newFakeClock returns a fake clock at 10:00 UTC on a fixed day, far from
// midnight, which the storage stamps what it stores with too.
func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)}
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

// waiting returns how many waits are pending.
func (c *fakeClock) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// countingStore is a storage that counts the lifecycle calls made of it,
// and can slow, block or fail ApplyLifecycle.
type countingStore struct {
	contracts.StorageProvider
	// beforeApply, when set, is called before each ApplyLifecycle, and an
	// error it returns fails that call.
	beforeApply func(key string) error
	// beforeAbort, when set, is called before each AbortLifecycleUpload,
	// and an error it returns fails that call.
	beforeAbort func(uploadID string) error
	// locked, when set, reports the keys whose lock a write holds, which
	// ApplyLifecycle and AbortLifecycleUpload then report Busy.
	locked  func(key string) bool
	applied map[string]int
	// cursors are the cursors ScanKeys was called with, in order.
	cursors []string
	targets atomic.Int64
	scans   atomic.Int64
	mu      sync.Mutex
}

func (c *countingStore) LifecycleTargets(ctx context.Context) ([]contracts.LifecycleTarget, error) {
	c.targets.Add(1)
	return c.StorageProvider.LifecycleTargets(ctx)
}

func (c *countingStore) ScanKeys(
	ctx context.Context,
	bucket, cursor string,
	n int,
) ([]string, string, error) {
	c.scans.Add(1)
	c.mu.Lock()
	c.cursors = append(c.cursors, cursor)
	c.mu.Unlock()
	return c.StorageProvider.ScanKeys(ctx, bucket, cursor, n)
}

func (c *countingStore) ApplyLifecycle(
	ctx context.Context,
	bucket, key string,
	d contracts.LifecycleDecider,
) (*contracts.LifecycleOutcome, error) {
	c.mu.Lock()
	before := c.beforeApply
	c.mu.Unlock()
	if c.locked != nil && c.locked(key) {
		return &contracts.LifecycleOutcome{Busy: true}, nil
	}
	if before != nil {
		if err := before(key); err != nil {
			return nil, err
		}
	}
	out, err := c.StorageProvider.ApplyLifecycle(ctx, bucket, key, d)
	if err == nil && !out.Busy {
		c.mu.Lock()
		c.applied[key]++
		c.mu.Unlock()
	}
	return out, err
}

func (c *countingStore) AbortLifecycleUpload(
	ctx context.Context,
	bucket, key, uploadID string,
	initiated time.Time,
) (*contracts.LifecycleAbort, error) {
	if c.locked != nil && c.locked(key) {
		return &contracts.LifecycleAbort{Busy: true}, nil
	}
	if c.beforeAbort != nil {
		if err := c.beforeAbort(uploadID); err != nil {
			return nil, err
		}
	}
	return c.StorageProvider.AbortLifecycleUpload(ctx, bucket, key, uploadID, initiated)
}

func (c *countingStore) setBeforeApply(fn func(key string) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.beforeApply = fn
}

// appliedCount returns how many times ApplyLifecycle applied each key.
func (c *countingStore) appliedCount() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.applied))
	for k, v := range c.applied {
		out[k] = v
	}
	return out
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

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type fixture struct {
	t     *testing.T
	ctx   context.Context //nolint:containedctx // the test's
	dir   *dirimpl.Service
	store *countingStore
	svc   *Service
	clock *fakeClock
	logs  *logBuffer
	ready chan struct{}
}

// newFixture returns a sweep service, not started, over a fresh storage
// on the same fake clock, with a bucket named "bucket".
func newFixture(t *testing.T) *fixture {
	t.Helper()
	cfg := config.New(config.WithProvider(configtest.NewStaticProvider(nil)))
	if err := cfg.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}
	clock := newFakeClock()
	ctx := context.Background()
	dir := dirimpl.NewService(&dirimpl.Settings{RootDir: t.TempDir()}, clock)(
		cfg, &gas.NopLogger{})
	if err := dir.Init(ctx); err != nil {
		t.Fatalf("init storage: %v", err)
	}
	t.Cleanup(func() {
		if err := dir.Close(ctx); err != nil {
			t.Errorf("close storage: %v", err)
		}
	})
	logs := &logBuffer{}
	zl := zerolog.New(logs)
	logger, err := gaszerolog.New(gaszerolog.WithLogger(&zl))()
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	store := &countingStore{StorageProvider: dir, applied: map[string]int{}}
	ready := make(chan struct{})
	svc := NewService(Settings{Clock: clock, Ready: ready})(logger, store)
	f := &fixture{
		t: t, ctx: ctx, dir: dir, store: store, svc: svc, clock: clock, logs: logs, ready: ready,
	}
	if _, err = dir.CreateBucket(ctx, "bucket", nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return f
}

// start starts the service, and closes it at the end of the test.
func (f *fixture) start() {
	f.t.Helper()
	if err := f.svc.Init(f.ctx); err != nil {
		f.t.Fatalf("init: %v", err)
	}
	f.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := f.svc.Close(ctx); err != nil {
			f.t.Errorf("close: %v", err)
		}
	})
}

func (f *fixture) ref(bucket string) contracts.BucketRef {
	f.t.Helper()
	gen, _, err := f.dir.GetBucketPolicy(f.ctx, bucket)
	if err != nil && !errors.Is(err, contracts.ErrNoSuchBucketPolicy) {
		f.t.Fatalf("read bucket %s: %v", bucket, err)
	}
	return contracts.BucketRef{Name: bucket, Generation: gen}
}

func (f *fixture) pin(bucket string) (context.Context, func()) {
	f.t.Helper()
	ctx, release, err := f.dir.Pin(f.ctx, f.ref(bucket))
	if err != nil {
		f.t.Fatalf("pin: %v", err)
	}
	return ctx, release
}

// configure stores rules, the Rule elements of a lifecycle configuration,
// as bucket's.
func (f *fixture) configure(bucket string, rules ...string) {
	f.t.Helper()
	cfg, err := lifecycle.Parse([]byte("<LifecycleConfiguration>" + strings.Join(rules, "") +
		"</LifecycleConfiguration>"))
	if err != nil {
		f.t.Fatalf("parse: %v", err)
	}
	raw, err := lifecycle.Encode(f.ref(bucket).Generation, cfg, lifecycle.TransitionDefault128K)
	if err != nil {
		f.t.Fatalf("encode: %v", err)
	}
	ctx, release := f.pin(bucket)
	defer release()
	if err = f.dir.PutBucketConfig(ctx, bucket, contracts.ConfigLifecycle, raw); err != nil {
		f.t.Fatalf("put configuration: %v", err)
	}
}

// expireRule is the rule "expire", which expires the objects under prefix
// a day after they are written.
func expireRule(prefix string) string {
	return `<Rule><ID>expire</ID><Filter><Prefix>` + prefix + `</Prefix></Filter>` +
		`<Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule>`
}

// abortRule aborts the uploads under prefix days after they are initiated.
func abortRule(id, prefix string, days int) string {
	return fmt.Sprintf(`<Rule><ID>%s</ID><Filter><Prefix>%s</Prefix></Filter>`+
		`<Status>Enabled</Status><AbortIncompleteMultipartUpload>`+
		`<DaysAfterInitiation>%d</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule>`,
		id, prefix, days)
}

func (f *fixture) put(bucket string, keys ...string) {
	f.t.Helper()
	ctx, release := f.pin(bucket)
	defer release()
	for _, key := range keys {
		if _, err := f.dir.PutObject(ctx, bucket, key, strings.NewReader("x"),
			contracts.PutObjectInput{}, contracts.Precondition{}); err != nil {
			f.t.Fatalf("put %s: %v", key, err)
		}
	}
}

func (f *fixture) exists(bucket, key string) bool {
	f.t.Helper()
	ctx, release := f.pin(bucket)
	defer release()
	_, err := f.dir.StatObject(ctx, contracts.ObjectRef{Bucket: bucket, Key: key})
	if err != nil && !errors.Is(err, storage.ErrObjectNotFound) {
		f.t.Fatalf("stat %s: %v", key, err)
	}
	return err == nil
}

// requireExists fails the test unless each of keys of the bucket "bucket"
// exists, or does not, as want says.
func (f *fixture) requireExists(want bool, keys ...string) {
	f.t.Helper()
	for _, key := range keys {
		if got := f.exists("bucket", key); got != want {
			f.t.Fatalf("%s exists: got %v, want %v", key, got, want)
		}
	}
}

// pastExpiry is how far to move the clock from 10:00 for a 1-day rule to
// expire what was written at 10:00: to 00:30 two days on.
const pastExpiry = 38*time.Hour + 30*time.Minute

func (f *fixture) tick() bool {
	return f.svc.tick(f.ctx, f.clock.Now())
}

// eventually fails the test unless cond holds within 5 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestNoSweepBeforeReady(t *testing.T) {
	f := newFixture(t)
	f.put("bucket", "logs/a")
	f.configure("bucket", expireRule("logs/"))
	f.clock.Advance(pastExpiry)
	f.start()

	time.Sleep(50 * time.Millisecond)
	if n := f.store.targets.Load(); n != 0 {
		t.Fatalf("lifecycle targets read before ready: %d", n)
	}
	f.requireExists(true, "logs/a")

	close(f.ready)
	eventually(t, "the first sweep", func() bool { return !f.exists("bucket", "logs/a") })
	if !strings.Contains(f.logs.String(), `"operation":"S3.EXPIRE.OBJECT"`) {
		t.Fatalf("no expiry logged: %s", f.logs.String())
	}
}

func TestOneSweepADay(t *testing.T) {
	f := newFixture(t)
	f.put("bucket", "logs/a", "keep/b")
	f.configure("bucket", expireRule("logs/"))

	if f.tick() {
		t.Fatal("the first sweep is unfinished")
	}
	if n := f.store.scans.Load(); n != 1 {
		t.Fatalf("first tick: %d scans, want 1", n)
	}
	f.requireExists(true, "logs/a")

	f.clock.Advance(time.Hour)
	f.tick()
	if n := f.store.scans.Load(); n != 1 {
		t.Fatalf("a second tick the same day scanned: %d scans", n)
	}

	f.clock.Advance(pastExpiry - time.Hour)
	f.tick()
	if n := f.store.scans.Load(); n != 2 {
		t.Fatalf("after midnight: %d scans, want 2", n)
	}
	f.requireExists(false, "logs/a")
	f.requireExists(true, "keep/b")
	logs := f.logs.String()
	for _, want := range []string{
		`"actor":"lifecycle"`, `"operation":"S3.EXPIRE.OBJECT"`, `"bucket":"bucket"`,
		`"key":"logs/a"`, `"rule_id":"expire"`,
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log lacks %s: %s", want, logs)
		}
	}
	if got := f.store.appliedCount()["keep/b"]; got != 0 {
		t.Fatalf("a key no rule matches was applied %d times", got)
	}
}

func TestAConfigurationChangeSweepsAtTheNextTick(t *testing.T) {
	f := newFixture(t)
	f.put("bucket", "logs/a", "tmp/b")
	f.configure("bucket", expireRule("logs/"))
	f.tick()
	f.clock.Advance(pastExpiry)
	f.tick()
	f.requireExists(false, "logs/a")
	f.requireExists(true, "tmp/b")
	scans := f.store.scans.Load()

	f.clock.Advance(time.Hour)
	f.configure("bucket", expireRule("tmp/"))
	f.tick()
	if n := f.store.scans.Load(); n != scans+1 {
		t.Fatalf("after a change: %d scans, want %d", n, scans+1)
	}
	f.requireExists(false, "tmp/b")
}

func TestDeletingTheConfigurationStopsTheSweeps(t *testing.T) {
	f := newFixture(t)
	f.put("bucket", "logs/a")
	f.configure("bucket", expireRule("logs/"))
	f.tick()
	ctx, release := f.pin("bucket")
	if err := f.dir.DeleteBucketConfig(ctx, "bucket", contracts.ConfigLifecycle); err != nil {
		t.Fatal(err)
	}
	release()
	f.clock.Advance(pastExpiry)
	f.tick()
	f.requireExists(true, "logs/a")
	if n := f.store.scans.Load(); n != 1 {
		t.Fatalf("%d scans, want 1", n)
	}
	if _, ok := f.svc.buckets["bucket"]; ok {
		t.Fatal("the state of a bucket without rules is kept")
	}
}

func TestTheBudgetLeavesTheSweepToContinue(t *testing.T) {
	f := newFixture(t)
	keys := []string{"logs/0", "logs/1", "logs/2", "logs/3", "logs/4"}
	f.put("bucket", keys...)
	f.configure("bucket", expireRule("logs/"))
	f.clock.Advance(pastExpiry)
	f.svc.budget = 2
	close(f.ready)
	f.start()

	eventually(t, "the first tick", func() bool { return f.clock.waiting() == 1 })
	if n := len(f.store.appliedCount()); n != 2 {
		t.Fatalf("first tick applied %d keys, want 2", n)
	}
	if f.svc.continueAfter >= f.svc.checkInterval {
		t.Fatal("continueAfter is not shorter than checkInterval")
	}
	// Only the continuation's wait ends this soon.
	for want := 4; want <= 5; want++ {
		f.clock.Advance(f.svc.continueAfter)
		eventually(t, "the next tick", func() bool {
			return len(f.store.appliedCount()) >= min(want, 5) && f.clock.waiting() == 1
		})
	}
	f.requireExists(false, keys...)
	f.requireAppliedOnce()
	// Finished: the next tick waits checkInterval.
	f.clock.Advance(f.svc.continueAfter)
	time.Sleep(20 * time.Millisecond)
	if n := f.store.targets.Load(); n != 3 {
		t.Fatalf("%d ticks, want 3", n)
	}
}

func TestDeleteBucketWinsOverASweep(t *testing.T) {
	f := newFixture(t)
	if _, err := f.dir.CreateBucket(f.ctx, "big", nil); err != nil {
		t.Fatal(err)
	}
	// One scan's worth, which the sweep keeps applying, a pin at a time,
	// once the test has deleted them.
	keys := make([]string, f.svc.pinBatch)
	for i := range keys {
		keys[i] = fmt.Sprintf("logs/%03d", i)
	}
	f.put("big", keys...)
	f.configure("big", expireRule("logs/"))
	f.clock.Advance(pastExpiry)
	var calls atomic.Int64
	f.store.setBeforeApply(func(string) error {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return nil
	})
	done := make(chan bool)
	go func() { done <- f.tick() }()
	eventually(t, "the sweep to start", func() bool { return calls.Load() >= 1 })

	ctx, release := f.pin("big")
	for _, key := range keys {
		if _, err := f.dir.DeleteObject(
			ctx,
			contracts.ObjectRef{Bucket: "big", Key: key},
		); err != nil {
			t.Fatalf("delete %s: %v", key, err)
		}
	}
	release()
	start := time.Now()
	if err := f.dir.DeleteBucket(f.ctx, f.ref("big")); err != nil {
		t.Fatalf("delete bucket during a sweep: %v", err)
	}
	// The sweep's pins alone last 64 x 20ms.
	if d := time.Since(start); d > 600*time.Millisecond {
		t.Fatalf("delete bucket took %v", d)
	}
	<-done
	if n := calls.Load(); n >= int64(len(keys)) {
		t.Fatalf("the sweep went on after its bucket was deleted: %d calls", n)
	}
	if _, ok := f.svc.buckets["big"]; ok {
		t.Fatal("the state of a deleted bucket is kept")
	}
}

func TestCloseMidSweepAndRestart(t *testing.T) {
	f := newFixture(t)
	keys := []string{"logs/0", "logs/1", "logs/2", "logs/3", "logs/4"}
	f.put("bucket", keys...)
	f.configure("bucket", expireRule("logs/"))
	f.clock.Advance(pastExpiry)
	reached, unblock := make(chan struct{}), make(chan struct{})
	calls := 0
	f.store.setBeforeApply(func(string) error {
		// Called on the sweep's goroutine only.
		calls++
		if calls == 3 {
			close(reached)
			<-unblock
		}
		return nil
	})
	close(f.ready)
	f.start()
	<-reached

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := f.svc.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close with a key in progress: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("close took %v past its deadline", d)
	}
	f.svc.run.mu.Lock()
	wg := f.svc.run.wg
	f.svc.run.mu.Unlock()
	close(unblock)
	wg.Wait()
	f.store.setBeforeApply(nil)
	// The key in progress finished; the run stopped before the next.
	if n := len(f.store.appliedCount()); n != 3 {
		t.Fatalf("%d keys applied before the run stopped, want 3: %s", n, f.logs.String())
	}

	// As RestartService does.
	if err := f.svc.Init(f.ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the sweep to resume", func() bool { return len(f.store.appliedCount()) == 5 })
	f.requireExists(false, keys...)
	f.requireAppliedOnce()
	if n := f.store.scans.Load(); n != 1 {
		t.Fatalf("%d scans, want 1: the resumed sweep started over", n)
	}
}

func TestStorageErrorsAreRetriedAtTheNextTick(t *testing.T) {
	f := newFixture(t)
	keys := []string{"logs/0", "logs/1", "logs/2"}
	f.put("bucket", keys...)
	f.configure("bucket", expireRule("logs/"))
	f.clock.Advance(pastExpiry)
	calls := 0
	f.store.setBeforeApply(func(string) error {
		calls++
		if calls == 2 {
			return errors.New("disk on fire")
		}
		return nil
	})
	if f.tick() {
		t.Fatal("a failed sweep asked to continue at once")
	}
	if !strings.Contains(f.logs.String(), "disk on fire") ||
		!strings.Contains(f.logs.String(), `"level":"error"`) {
		t.Fatalf("the error was not logged: %s", f.logs.String())
	}
	if n := len(f.store.appliedCount()); n != 1 || calls != 2 {
		t.Fatalf("%d keys applied in %d calls, want 1 in 2: the sweep went on", n, calls)
	}

	f.store.setBeforeApply(nil)
	f.clock.Advance(time.Minute)
	if f.tick() {
		t.Fatal("the retried sweep is unfinished")
	}
	if n := skips(f.logs); n != 0 {
		t.Fatalf("a key that failed once was skipped: %d skips", n)
	}
	f.requireExists(false, keys...)
	f.requireAppliedOnce()
}

func TestBusyKeysAreRetried(t *testing.T) {
	f := newFixture(t)
	f.put("bucket", "logs/a", "logs/b")
	f.configure("bucket", expireRule("logs/"))
	f.clock.Advance(pastExpiry)

	// What ApplyLifecycle reports while a write holds the key's lock.
	busy := true
	f.store.StorageProvider = busyStore{StorageProvider: f.dir, busy: &busy, key: "logs/a"}
	if !f.tick() {
		t.Fatal("a sweep with a busy key is finished")
	}
	f.requireExists(true, "logs/a")
	f.requireExists(false, "logs/b")

	busy = false
	f.clock.Advance(time.Second)
	if f.tick() {
		t.Fatal("the sweep is unfinished after the retry")
	}
	f.requireExists(false, "logs/a")
	if n := f.store.scans.Load(); n != 1 {
		t.Fatalf("the retry scanned again: %d scans", n)
	}
}

// busyStore reports key Busy while *busy is set.
type busyStore struct {
	contracts.StorageProvider
	busy *bool
	key  string
}

func (b busyStore) ApplyLifecycle(
	ctx context.Context,
	bucket, key string,
	d contracts.LifecycleDecider,
) (*contracts.LifecycleOutcome, error) {
	if key == b.key && *b.busy {
		return &contracts.LifecycleOutcome{Busy: true}, nil
	}
	return b.StorageProvider.ApplyLifecycle(ctx, bucket, key, d)
}

func (f *fixture) createUpload(key string) contracts.UploadInfo {
	f.t.Helper()
	ctx, release := f.pin("bucket")
	defer release()
	id, _, err := f.dir.CreateMultipartUpload(ctx, "bucket", key, contracts.PutObjectInput{})
	if err != nil {
		f.t.Fatalf("create upload %s: %v", key, err)
	}
	return contracts.UploadInfo{Key: key, UploadID: id}
}

func (f *fixture) uploads() map[string]bool {
	f.t.Helper()
	ctx, release := f.pin("bucket")
	defer release()
	all, err := f.dir.ListMultipartUploads(ctx, "bucket")
	if err != nil {
		f.t.Fatalf("list uploads: %v", err)
	}
	out := map[string]bool{}
	for _, u := range all {
		out[u.UploadID] = true
	}
	return out
}

func TestOldUploadsUnderThePrefixAreAborted(t *testing.T) {
	f := newFixture(t)
	old := f.createUpload("logs/old")
	outside := f.createUpload("other/old")
	f.clock.Advance(pastExpiry)
	young := f.createUpload("logs/young")
	f.configure("bucket", abortRule("abort", "logs/", 1))

	if f.tick() {
		t.Fatal("the sweep is unfinished")
	}
	left := f.uploads()
	if left[old.UploadID] {
		t.Fatal("the old upload under the prefix is kept")
	}
	if !left[outside.UploadID] || !left[young.UploadID] {
		t.Fatalf("uploads left: %v", left)
	}
	logs := f.logs.String()
	for _, want := range []string{
		`"operation":"S3.DELETE.UPLOAD"`, `"upload_id":"` + old.UploadID + `"`,
		`"rule_id":"abort"`, `"key":"logs/old"`,
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log lacks %s: %s", want, logs)
		}
	}
}

func TestVersionedExpiryCreatesADeleteMarker(t *testing.T) {
	f := newFixture(t)
	ctx, release := f.pin("bucket")
	if err := f.dir.PutBucketVersioning(ctx, "bucket",
		contracts.VersioningConfig{Status: contracts.VersioningEnabled}); err != nil {
		t.Fatal(err)
	}
	release()
	f.put("bucket", "logs/a")
	f.configure("bucket", expireRule("logs/"))
	f.clock.Advance(pastExpiry)
	f.tick()
	f.requireExists(false, "logs/a")
	if !strings.Contains(f.logs.String(), `"operation":"S3.CREATE.DELETEMARKER"`) {
		t.Fatalf("no marker logged: %s", f.logs.String())
	}
}

// requireAppliedOnce fails the test unless every key applied was applied
// once.
func (f *fixture) requireAppliedOnce() {
	f.t.Helper()
	for key, n := range f.store.appliedCount() {
		if n != 1 {
			f.t.Fatalf("%s applied %d times", key, n)
		}
	}
}

// requireOneSkip fails the test unless one skip is logged, a warning
// naming field.
func (f *fixture) requireOneSkip(field string) {
	f.t.Helper()
	logs := f.logs.String()
	if n := skips(f.logs); n != 1 || !strings.Contains(logs, `"level":"warn"`) ||
		!strings.Contains(logs, field) {
		f.t.Fatalf("%d skips logged: %s", n, logs)
	}
}

// skips counts the skips logged.
func skips(logs *logBuffer) int {
	return strings.Count(logs.String(), `"message":"skipped for this sweep"`)
}

func TestAKeyThatKeepsFailingIsSkippedUntilTheNextSweep(t *testing.T) {
	f := newFixture(t)
	keys := []string{"logs/0", "logs/1", "logs/2", "logs/3", "logs/4"}
	f.put("bucket", keys...)
	f.configure("bucket", expireRule("logs/"))
	f.clock.Advance(pastExpiry)
	failures := 0
	f.store.setBeforeApply(func(key string) error {
		if key != "logs/2" {
			return nil
		}
		failures++
		return errors.New("disk on fire")
	})
	for tick := 1; tick < maxFailures; tick++ {
		if f.tick() {
			t.Fatalf("tick %d: a failed sweep asked to continue at once", tick)
		}
		if n := skips(f.logs); n != 0 {
			t.Fatalf("tick %d: skipped after %d failures", tick, failures)
		}
		f.clock.Advance(time.Minute)
	}
	if f.tick() {
		t.Fatal("the sweep is unfinished after the skip")
	}
	if failures != maxFailures {
		t.Fatalf("%d failures, want %d", failures, maxFailures)
	}
	f.requireOneSkip(`"key":"logs/2"`)
	// The failures before the last are errors; the last is the warning.
	if n := strings.Count(f.logs.String(), `"level":"error"`); n != maxFailures-1 {
		t.Fatalf("%d errors logged, want %d", n, maxFailures-1)
	}
	f.requireExists(true, "logs/2")
	f.requireExists(false, "logs/0", "logs/1", "logs/3", "logs/4")
	if f.svc.buckets["bucket"].sweeping {
		t.Fatal("the sweep did not finish past the skipped key")
	}

	// Not again the same day.
	f.store.setBeforeApply(nil)
	f.clock.Advance(time.Hour)
	f.tick()
	f.requireExists(true, "logs/2")

	f.clock.Advance(24 * time.Hour)
	f.tick()
	f.requireExists(false, "logs/2")
}

func TestAnUploadThatKeepsFailingIsSkipped(t *testing.T) {
	f := newFixture(t)
	failing := f.createUpload("logs/a")
	other := f.createUpload("logs/b")
	f.clock.Advance(pastExpiry)
	f.configure("bucket", abortRule("abort", "logs/", 1))
	f.store.beforeAbort = func(id string) error {
		if id == failing.UploadID {
			return errors.New("disk on fire")
		}
		return nil
	}
	for range maxFailures - 1 {
		f.tick()
		f.clock.Advance(time.Minute)
	}
	if f.tick() {
		t.Fatal("the sweep is unfinished after the skip")
	}
	left := f.uploads()
	if !left[failing.UploadID] || left[other.UploadID] {
		t.Fatalf("uploads left: %v", left)
	}
	f.requireOneSkip(`"upload_id":"` + failing.UploadID + `"`)
}

// ticksUntilDone ticks, a second apart, until a tick reports the sweep
// finished, at most limit times, and returns how many it took.
func (f *fixture) ticksUntilDone(limit int) int {
	f.t.Helper()
	for n := 1; n <= limit; n++ {
		if !f.tick() {
			return n
		}
		f.clock.Advance(time.Second)
	}
	f.t.Fatalf("the sweep is unfinished after %d ticks", limit)
	return 0
}

func TestAKeyThatStaysBusyIsSkippedUntilTheNextSweep(t *testing.T) {
	f := newFixture(t)
	f.put("bucket", "logs/a", "logs/b")
	f.configure("bucket", expireRule("logs/"))
	f.clock.Advance(pastExpiry)
	locked := true
	f.store.locked = func(key string) bool { return locked && key == "logs/a" }

	if n := f.ticksUntilDone(2 * maxBusy); n != maxBusy {
		t.Fatalf("skipped after %d ticks, want %d", n, maxBusy)
	}
	f.requireOneSkip(`"key":"logs/a"`)
	f.requireExists(true, "logs/a")
	f.requireExists(false, "logs/b")

	locked = false
	f.clock.Advance(24 * time.Hour)
	f.tick()
	f.requireExists(false, "logs/a")
}

func TestAnUploadThatStaysBusyIsSkipped(t *testing.T) {
	f := newFixture(t)
	u := f.createUpload("logs/a")
	f.clock.Advance(pastExpiry)
	f.configure("bucket", abortRule("abort", "logs/", 1))
	f.store.locked = func(key string) bool { return key == "logs/a" }

	if n := f.ticksUntilDone(2 * maxBusy); n != maxBusy {
		t.Fatalf("skipped after %d ticks, want %d", n, maxBusy)
	}
	f.requireOneSkip(`"upload_id":"` + u.UploadID + `"`)
	if !f.uploads()[u.UploadID] {
		t.Fatal("the busy upload was aborted")
	}
}

// requireApplied fails the test unless as many keys of the buckets alpha
// and beta were applied as want says.
func (f *fixture) requireApplied(when string, alpha, beta int) {
	f.t.Helper()
	got := map[string]int{}
	for key := range f.store.appliedCount() {
		got[strings.Split(key, "/")[0]]++
	}
	if got["alpha"] != alpha || got["beta"] != beta {
		f.t.Fatalf("%s: %d and %d keys applied, want %d and %d",
			when, got["alpha"], got["beta"], alpha, beta)
	}
}

func TestEveryBucketMakesProgress(t *testing.T) {
	f := newFixture(t)
	for _, b := range []string{"alpha", "beta"} {
		if _, err := f.dir.CreateBucket(f.ctx, b, nil); err != nil {
			t.Fatal(err)
		}
		f.put(b, b+"/0", b+"/1", b+"/2", b+"/3", b+"/4")
		f.configure(b, expireRule(b+"/"))
	}
	f.clock.Advance(pastExpiry)
	f.svc.budget = 2

	f.tick()
	f.requireApplied("first tick", 2, 0)
	// Without the rotation, beta would wait for alpha's sweep to end.
	f.tick()
	f.requireApplied("second tick", 2, 2)
	f.ticksUntilDone(10)
	f.requireApplied("at the end", 5, 5)
	f.requireAppliedOnce()
}

func TestASweepSpanningMidnightFinishesThenTheNewDaySweeps(t *testing.T) {
	f := newFixture(t)
	keys := []string{"logs/0", "logs/1", "logs/2", "logs/3", "logs/4"}
	f.put("bucket", keys...)
	f.configure("bucket", expireRule("logs/"))
	// 23:59:30 on the day the keys became eligible.
	f.clock.Advance(pastExpiry + 23*time.Hour + 29*time.Minute)
	f.svc.budget = 2
	started := lifecycle.Midnight(f.clock.Now())

	if !f.tick() {
		t.Fatal("the sweep finished within its budget")
	}
	f.clock.Advance(time.Minute)
	if lifecycle.Midnight(f.clock.Now()).Equal(started) {
		t.Fatal("the clock did not pass midnight")
	}
	f.ticksUntilDone(5)
	f.requireExists(false, keys...)
	f.requireAppliedOnce()
	if n := f.store.scans.Load(); n != 3 {
		t.Fatalf("%d scans, want 3: the sweep started over at midnight", n)
	}
	if got := f.svc.buckets["bucket"].sweptDay; !got.Equal(started) {
		t.Fatalf("swept day %v, want the day the sweep started, %v", got, started)
	}

	// The new day has not been swept: the next tick does.
	f.tick()
	if n := f.store.scans.Load(); n != 4 {
		t.Fatalf("%d scans, want 4: the new day was not swept", n)
	}
}

func TestARecreatedBucketStartsAFreshSweep(t *testing.T) {
	f := newFixture(t)
	keys := []string{"logs/0", "logs/1", "logs/2", "logs/3", "logs/4"}
	f.put("bucket", keys...)
	f.configure("bucket", expireRule("logs/"))
	f.clock.Advance(pastExpiry)
	f.svc.budget = 2
	if !f.tick() {
		t.Fatal("the sweep finished within its budget")
	}
	old := f.ref("bucket")

	ctx, release := f.pin("bucket")
	for _, key := range keys {
		if _, err := f.dir.DeleteObject(
			ctx,
			contracts.ObjectRef{Bucket: "bucket", Key: key},
		); err != nil {
			t.Fatal(err)
		}
	}
	release()
	if err := f.dir.DeleteBucket(f.ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.CreateBucket(f.ctx, "bucket", nil); err != nil {
		t.Fatal(err)
	}
	// Written now, so not eligible.
	f.put("bucket", keys...)
	f.configure("bucket", expireRule("logs/"))
	before := len(f.store.cursors)
	applied := f.store.appliedCount()

	f.ticksUntilDone(5)
	if c := f.store.cursors[before]; c != "" {
		t.Fatalf("the new bucket's sweep started at cursor %q", c)
	}
	f.requireExists(true, keys...)
	if got := f.svc.buckets["bucket"].generation; got == old.Generation {
		t.Fatal("the sweep state is the old generation's")
	}
	for key, n := range f.store.appliedCount() {
		if n-applied[key] != 1 {
			t.Fatalf("%s applied %d times in the new bucket, want 1", key, n-applied[key])
		}
	}
}
