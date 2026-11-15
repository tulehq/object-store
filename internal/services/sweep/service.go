// Package sweep applies the buckets' lifecycle rules in the background, as
// S3 does itself: without a principal or a policy, through the storage's
// lifecycle primitives, and logged with the operation names of S3's server
// access log ("How S3 Lifecycle interacts with other bucket
// configurations", S3 User Guide).
//
// Every checkInterval the scheduler ticks. A tick sweeps each bucket that
// has a lifecycle configuration at most once per UTC day of the clock, and
// once more after its configuration or generation changes. A sweep walks
// the bucket's key index in batches, each under a pin it holds for at most
// pinBatch keys or pinTime, and leaves pinGap between two pins, so a
// DeleteBucket waiting for the pin still wins; a tick does at most budget
// units of work. An unfinished sweep
// resumes after continueAfter from where it stopped, kept in memory.
package sweep

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/lifecycle"
)

// The defaults of a Service's cadence and bounds. AWS documents only that
// S3 Lifecycle "doesn't guarantee a completion time" ("Setting an S3
// Lifecycle configuration on a bucket", S3 User Guide), so the numbers are
// this service's.
const (
	// checkInterval is how often the scheduler ticks, on the clock.
	checkInterval = time.Minute
	// continueAfter is how soon the scheduler ticks again, on the clock,
	// while a sweep is unfinished.
	continueAfter = time.Second
	// budget is how many units of work a tick does: index entries read,
	// busy keys retried and uploads aborted.
	budget = 1000
	// pinBatch is how many keys or uploads are applied under one pin.
	pinBatch = 64
	// pinTime is how long, in wall time, one pin is held at most, well
	// under the 2s DeleteBucket waits for pins.
	pinTime = 200 * time.Millisecond
	// pinGap is how long, in wall time, a sweep waits between two pins
	// of one bucket. DeleteBucket polls for the bucket's lock every 5ms
	// rather than waiting for it, so a pin taken again at once after its
	// release would leave it no moment to take the lock in.
	pinGap = 10 * time.Millisecond
	// maxFailures is how many times in a row, in one sweep, applying the
	// rules to a key or aborting an upload may fail before the sweep
	// skips it, so one key that always fails does not stall its bucket.
	// The next sweep tries it again.
	maxFailures = 3
	// maxBusy is how many times in one sweep a key or upload may be found
	// locked before the sweep skips it. Once the rest of the bucket is
	// done, busy ones are retried continueAfter apart, so 60 retries give
	// a write at least a minute to release the lock: far longer than a
	// write holds it to read one request body, while a key held by a
	// stalled or stuck writer no longer keeps the bucket's sweep, and so
	// the next day's, from finishing. The next sweep tries it again.
	maxBusy = 60
)

// Settings configures a Service. A nil Clock is contracts.SystemClock.
type Settings struct {
	Clock contracts.Clock
	// Ready, when set, is closed by the app's last ready hook; no sweep
	// runs before it.
	Ready <-chan struct{}
}

// Service applies lifecycle rules.
type Service struct {
	logger gas.Logger
	store  contracts.StorageProvider
	clock  contracts.Clock
	ready  <-chan struct{}
	run    *runner
	// buckets is the sweep state of each bucket, by name, kept across a
	// Close and Init, lost on process exit. mu guards it, and is held for
	// a whole tick, so a run a Close gave up waiting for never ticks
	// alongside the next.
	buckets map[string]*bucketState
	// stopped is the bucket the last tick spent its budget in; the next
	// tick starts with the bucket after it, by name, so every bucket
	// makes progress however large the others are.
	stopped string
	mu      sync.Mutex

	checkInterval time.Duration
	continueAfter time.Duration
	budget        int
	pinBatch      int
	pinTime       time.Duration
	pinGap        time.Duration
}

// bucketState is where a bucket's sweep is.
type bucketState struct {
	// day is the UTC midnight of the day the current or last sweep
	// started, and sweptDay that of the last sweep finished.
	day      time.Time
	sweptDay time.Time
	// failures count the failures in a row of each key, and
	// uploadFailures of each upload, by ID, in this sweep; busyCount and
	// busyUploadCount how many times each was found locked.
	failures        map[string]int
	uploadFailures  map[string]int
	busyCount       map[string]int
	busyUploadCount map[string]int
	generation      string
	// cursor is the ScanKeys cursor after pending, the keys scanned and
	// not yet applied; scanned reports that the scan has reached the end.
	cursor  string
	pending []string
	// busy are keys found locked, retried first next tick.
	busy []string
	// uploads are the eligible uploads not yet aborted, once listed, and
	// busyUploads those found locked, retried next tick.
	uploads     []contracts.UploadInfo
	busyUploads []contracts.UploadInfo
	configSum   [32]byte
	sweeping    bool
	scanned     bool
	listed      bool
}

// reset starts a sweep on day.
func (b *bucketState) reset(day time.Time) {
	*b = bucketState{
		generation: b.generation, configSum: b.configSum, sweptDay: b.sweptDay,
		day: day, sweeping: true, failures: map[string]int{}, uploadFailures: map[string]int{},
		busyCount: map[string]int{}, busyUploadCount: map[string]int{},
	}
}

var _ gas.Service = (*Service)(nil)

const serviceName = "tule/services/lifecycle"

// NewService returns the constructor gas uses to build a Service from
// settings, applying the lifecycle rules of store's buckets.
func NewService(settings Settings) func(gas.Logger, contracts.StorageProvider) *Service {
	return func(logger gas.Logger, store contracts.StorageProvider) *Service {
		clock := settings.Clock
		if clock == nil {
			clock = contracts.SystemClock
		}
		return &Service{
			logger:        logger.With().Str("service", serviceName).Logger(),
			store:         store,
			clock:         clock,
			ready:         settings.Ready,
			run:           newRunner(),
			buckets:       map[string]*bucketState{},
			checkInterval: checkInterval,
			continueAfter: continueAfter,
			budget:        budget,
			pinBatch:      pinBatch,
			pinTime:       pinTime,
			pinGap:        pinGap,
		}
	}
}

// Name identifies the service to gas.
func (s *Service) Name() string {
	return serviceName
}

// Init starts the scheduler. gas's RestartService calls it again after
// Close, which starts it over with the sweep state kept.
func (s *Service) Init(context.Context) error {
	s.run.begin()
	s.run.goBackground(s.schedule) //nolint:contextcheck // it lives as long as the service
	return nil
}

// Close stops the scheduler, between two keys, and waits for it, up to
// ctx's end.
func (s *Service) Close(ctx context.Context) error {
	if err := s.run.stop(ctx); err != nil {
		return fmt.Errorf("%s: the sweep did not stop in time: %w", s.Name(), err)
	}
	return nil
}

// schedule waits for Ready, then ticks at once and every checkInterval
// after, or continueAfter while a sweep is unfinished, until ctx ends.
func (s *Service) schedule(ctx context.Context) {
	if s.ready != nil {
		select {
		case <-ctx.Done():
			return
		case <-s.ready:
		}
	}
	for {
		wait := s.checkInterval
		if s.tick(ctx, s.clock.Now()) {
			wait = s.continueAfter
		}
		select {
		case <-ctx.Done():
			return
		case <-s.clock.After(wait):
		}
	}
}

// tick sweeps the buckets with a lifecycle configuration as of now, within
// one budget, and reports whether a sweep is left unfinished by the
// budget, or with keys or uploads that were busy.
func (s *Service) tick(ctx context.Context, now time.Time) (more bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	targets, err := s.store.LifecycleTargets(ctx)
	if err != nil {
		s.logger.Error("failed to list the buckets with lifecycle rules").Err("error", err).Send()
		return false
	}
	s.forget(targets)
	left := s.budget
	order := s.rotate(targets)
	for i, t := range order {
		if ctx.Err() != nil {
			return false
		}
		_, cfg, dErr := lifecycle.Decode(t.Config)
		if dErr != nil {
			s.logger.Error("failed to decode a lifecycle configuration").
				Str("bucket", t.Ref.Name).Err("error", dErr).Send()
			continue
		}
		if !s.sweepBucket(ctx, t, cfg, now, &left) {
			more = true
		}
		if left <= 0 {
			// The buckets after it are left for the next tick, which
			// starts with them.
			s.stopped = t.Ref.Name
			return (more || i < len(order)-1) && ctx.Err() == nil
		}
	}
	return more && ctx.Err() == nil
}

// forget drops the state of the buckets that are not among targets: gone,
// or without a lifecycle configuration now.
func (s *Service) forget(targets []contracts.LifecycleTarget) {
	live := make(map[string]bool, len(targets))
	for _, t := range targets {
		live[t.Ref.Name] = true
	}
	for name := range s.buckets {
		if !live[name] {
			delete(s.buckets, name)
		}
	}
}

// rotate returns targets sorted by name, starting with the first after
// the bucket the last tick spent its budget in, and wrapping around.
func (s *Service) rotate(targets []contracts.LifecycleTarget) []contracts.LifecycleTarget {
	sorted := slices.SortedFunc(slices.Values(targets),
		func(a, b contracts.LifecycleTarget) int { return strings.Compare(a.Ref.Name, b.Ref.Name) })
	i, _ := slices.BinarySearchFunc(
		sorted,
		s.stopped,
		func(t contracts.LifecycleTarget, name string) int { return strings.Compare(t.Ref.Name, name) },
	)
	if i < len(sorted) && sorted[i].Ref.Name == s.stopped {
		i++
	}
	return append(sorted[i:], sorted[:i]...)
}

// sweepBucket sweeps t's bucket as of now, as far as budget allows, and
// reports whether it is done for now: swept, not due, failed (retried at
// the next tick) or gone. It decrements budget by the work it did.
func (s *Service) sweepBucket(
	ctx context.Context,
	t contracts.LifecycleTarget,
	cfg *lifecycle.Configuration,
	now time.Time,
	budget *int,
) (done bool) {
	st := s.state(t, now)
	if !st.sweeping {
		return true
	}
	retry, retryUploads := st.busy, st.busyUploads
	st.busy, st.busyUploads = nil, nil
	defer func() {
		// What was not retried this tick is retried the next.
		st.busy = slices.Concat(retry, st.busy)
		st.busyUploads = slices.Concat(retryUploads, st.busyUploads)
	}()
	b := &batch{
		svc: s, st: st, ref: t.Ref, cfg: cfg, d: cfg.Decider(now), now: now,
		budget: budget, retry: &retry, retryUploads: &retryUploads,
	}
	for {
		finished, err := b.run(ctx)
		switch {
		case errors.Is(err, storage.ErrBucketNotFound):
			delete(s.buckets, t.Ref.Name)
			return true
		case err != nil:
			s.logger.Error("failed to apply lifecycle rules; retrying at the next tick").
				Str("bucket", t.Ref.Name).Err("error", err).Send()
			return true
		case finished:
			if len(st.busy) > 0 || len(st.busyUploads) > 0 {
				return false
			}
			st.sweeping, st.sweptDay = false, st.day
			return true
		case *budget <= 0 || !s.pause(ctx):
			return false
		}
	}
}

// pause waits pinGap between two pins, and reports whether ctx is still
// live.
func (s *Service) pause(ctx context.Context) bool {
	t := time.NewTimer(s.pinGap)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// state returns t's sweep state as of now, a sweep started when the
// bucket has none yet, its configuration or generation changed ("When a
// lifecycle rule is created or modified, objects that already meet the
// eligibility criteria may be processed immediately", S3 User Guide), or
// its last sweep finished started before today.
func (s *Service) state(t contracts.LifecycleTarget, now time.Time) *bucketState {
	sum := sha256.Sum256(t.Config)
	today := lifecycle.Midnight(now)
	st, ok := s.buckets[t.Ref.Name]
	if !ok {
		st = &bucketState{}
		s.buckets[t.Ref.Name] = st
	}
	if !ok || st.generation != t.Ref.Generation || st.configSum != sum {
		st.generation, st.configSum = t.Ref.Generation, sum
		st.reset(today)
		return st
	}
	if !st.sweeping && st.sweptDay.Before(today) {
		st.reset(today)
	}
	return st
}

// batch is the work of one bucket's sweep in one tick, done in runs each
// under one pin.
type batch struct {
	svc          *Service
	st           *bucketState
	cfg          *lifecycle.Configuration
	d            contracts.LifecycleDecider
	now          time.Time
	budget       *int
	retry        *[]string
	retryUploads *[]contracts.UploadInfo
	ref          contracts.BucketRef
}

// run does the next units of the sweep under one pin, until pinBatch
// keys or uploads, pinTime, the budget or ctx ends, and reports whether
// nothing is left to do this tick. ctx is checked between units only: the
// storage calls run without its cancellation, so a key or upload in
// progress when the service closes is finished rather than failed midway.
func (b *batch) run(ctx context.Context) (finished bool, err error) {
	pinned, release, err := b.svc.store.Pin(context.WithoutCancel(ctx), b.ref)
	if err != nil {
		return false, err
	}
	defer release()
	start := time.Now()
	for n := 0; n < b.svc.pinBatch && time.Since(start) < b.svc.pinTime; {
		if ctx.Err() != nil {
			return false, nil
		}
		applied, stop, uErr := b.step(pinned, b.svc.pinBatch-n)
		if uErr != nil || stop {
			return uErr == nil && b.idle(), uErr
		}
		n += applied
	}
	return false, nil
}

// idle reports whether nothing is left to do this tick.
func (b *batch) idle() bool {
	st := b.st
	return len(*b.retry) == 0 && len(st.pending) == 0 && st.scanned &&
		(st.listed || !b.cfg.HasUploadRules()) && len(*b.retryUploads) == 0 &&
		len(st.uploads) == 0
}

// step does the next unit of the sweep: a key retried, a key applied, a
// scan of up to room keys, the uploads listed, or an upload aborted. It
// reports how many keys or uploads it applied, and stop when there is
// nothing left to do or the budget is spent.
func (b *batch) step(ctx context.Context, room int) (applied int, stop bool, err error) {
	if applied, ok, kErr := b.keyStep(ctx, room); ok {
		return applied, false, kErr
	}
	if applied, ok, uErr := b.uploadStep(ctx); ok {
		return applied, false, uErr
	}
	return 0, true, nil
}

// keyStep is step on the keys, ok when there was one to take.
func (b *batch) keyStep(ctx context.Context, room int) (applied int, ok bool, err error) {
	st := b.st
	switch {
	case len(*b.retry) > 0 && *b.budget > 0:
		*b.budget--
		key := (*b.retry)[0]
		*b.retry = (*b.retry)[1:]
		if err = b.keyFailed(key, b.apply(ctx, key)); err != nil {
			st.busy = append(st.busy, key)
		}
		return 1, true, err
	case len(st.pending) > 0:
		return 1, true, b.applyPending(ctx)
	case !st.scanned && *b.budget > 0:
		n := min(room, *b.budget)
		*b.budget -= n
		keys, next, sErr := b.svc.store.ScanKeys(ctx, b.ref.Name, st.cursor, n)
		if sErr != nil {
			return 0, true, sErr
		}
		st.pending, st.cursor, st.scanned = keys, next, next == ""
		return 0, true, nil
	}
	return 0, false, nil
}

// uploadStep is step on the uploads, once the keys are scanned, ok when
// there was one to take.
func (b *batch) uploadStep(ctx context.Context) (applied int, ok bool, err error) {
	st := b.st
	if !st.scanned || *b.budget <= 0 {
		return 0, false, nil
	}
	switch {
	case !st.listed && b.cfg.HasUploadRules():
		*b.budget--
		return 0, true, b.listUploads(ctx)
	case len(*b.retryUploads) > 0:
		*b.budget--
		u := (*b.retryUploads)[0]
		*b.retryUploads = (*b.retryUploads)[1:]
		if err = b.uploadFailed(u, b.abort(ctx, u)); err != nil {
			st.busyUploads = append(st.busyUploads, u)
		}
		return 1, true, err
	case len(st.uploads) > 0:
		*b.budget--
		if err = b.uploadFailed(st.uploads[0], b.abort(ctx, st.uploads[0])); err != nil {
			return 1, true, err
		}
		st.uploads = st.uploads[1:]
		return 1, true, nil
	}
	return 0, false, nil
}

// applyPending applies the rules to the first key scanned and not yet
// applied, which stays first when that fails, unless it is skipped.
func (b *batch) applyPending(ctx context.Context) error {
	key := b.st.pending[0]
	if err := b.keyFailed(key, b.apply(ctx, key)); err != nil {
		return err
	}
	b.st.pending = b.st.pending[1:]
	return nil
}

// keyFailed records how applying the rules to key ended, err, and returns
// err, the caller to retry key at the next tick, or nil once err is the
// maxFailures-th failure in a row in this sweep, which skips key until
// the next sweep.
func (b *batch) keyFailed(key string, err error) error {
	if !failedOut(b.st.failures, key, err) {
		return err
	}
	b.skipped(err).Str("key", key).Send()
	return nil
}

// uploadFailed is keyFailed for aborting upload u.
func (b *batch) uploadFailed(u contracts.UploadInfo, err error) error {
	if !failedOut(b.st.uploadFailures, u.UploadID, err) {
		return err
	}
	b.skipped(err).Str("key", u.Key).Str("upload_id", u.UploadID).Send()
	return nil
}

// failedOut counts err, a failure of id, in counts, and reports whether
// it is id's maxFailures-th failure in a row; nil counts nothing, an
// attempt that succeeded having cleared the count, and a busy one counting
// as neither.
func failedOut(counts map[string]int, id string, err error) bool {
	if err == nil {
		return false
	}
	counts[id]++
	if counts[id] < maxFailures {
		return false
	}
	delete(counts, id)
	return true
}

// skipped starts the warning that a sweep skips what failed last with
// err.
func (b *batch) skipped(err error) gas.LogEvent {
	return b.svc.logger.Warn("skipped for this sweep").Str("bucket", b.ref.Name).
		Err("error", err)
}

// errBusy is what a key or upload skipped as locked maxBusy times last
// failed with.
var errBusy = fmt.Errorf("found locked %d times in this sweep", maxBusy)

// stillBusy counts that id was found locked in counts, and reports
// whether it is to be retried, false once it was maxBusy times.
func stillBusy(counts map[string]int, id string) bool {
	counts[id]++
	return counts[id] < maxBusy
}

// apply applies the rules to key, one the rules may match, and logs what
// was done; a key found locked is added to busy.
func (b *batch) apply(ctx context.Context, key string) error {
	if !b.cfg.MayMatch(key) {
		return nil
	}
	out, err := b.svc.store.ApplyLifecycle(ctx, b.ref.Name, key, b.d)
	if out != nil && out.Busy {
		if stillBusy(b.st.busyCount, key) {
			b.st.busy = append(b.st.busy, key)
		} else {
			b.skipped(errBusy).Str("key", key).Send()
		}
		return nil
	}
	if out != nil {
		b.logOutcome(key, out)
	}
	if err != nil {
		return fmt.Errorf("apply the lifecycle rules to %q: %w", key, err)
	}
	delete(b.st.failures, key)
	return nil
}

// Operation names of S3's server access log for what lifecycle rules do
// ("How S3 Lifecycle interacts with other bucket configurations").
const (
	opExpireObject       = "S3.EXPIRE.OBJECT"
	opCreateDeleteMarker = "S3.CREATE.DELETEMARKER"
	opDeleteUpload       = "S3.DELETE.UPLOAD"
)

// logOutcome logs each action out reports for key.
func (b *batch) logOutcome(key string, out *contracts.LifecycleOutcome) {
	for _, d := range out.Deleted {
		b.action(opExpireObject, d.RuleID).Str("key", key).Str("version_id", d.VersionID).
			Bool("delete_marker", d.Marker).Send()
	}
	if out.MarkerID != "" {
		b.action(opCreateDeleteMarker, out.MarkerRule).Str("key", key).
			Str("version_id", out.MarkerID).Send()
	}
}

// action starts the log event of a lifecycle action.
func (b *batch) action(op, rule string) gas.LogEvent {
	return b.svc.logger.Info("lifecycle action").Str("actor", "lifecycle").
		Str("operation", op).Str("bucket", b.ref.Name).Str("rule_id", rule)
}

// listUploads lists the bucket's uploads, and keeps those eligible for
// abort now.
func (b *batch) listUploads(ctx context.Context) error {
	all, err := b.svc.store.ListMultipartUploads(ctx, b.ref.Name)
	if err != nil {
		return fmt.Errorf("list the multipart uploads: %w", err)
	}
	for _, u := range all {
		if at, _, ok := b.cfg.AbortDate(u.Key, u.Initiated); ok && !at.After(b.now) {
			b.st.uploads = append(b.st.uploads, u)
		}
	}
	b.st.listed = true
	return nil
}

// abort aborts u, an upload eligible for abort, and logs it; one whose key
// is found locked is added to busyUploads.
func (b *batch) abort(ctx context.Context, u contracts.UploadInfo) error {
	out, err := b.svc.store.AbortLifecycleUpload(ctx, b.ref.Name, u.Key, u.UploadID, u.Initiated)
	if out != nil && out.Busy {
		if stillBusy(b.st.busyUploadCount, u.UploadID) {
			b.st.busyUploads = append(b.st.busyUploads, u)
		} else {
			b.skipped(errBusy).Str("key", u.Key).Str("upload_id", u.UploadID).Send()
		}
		return nil
	}
	if out != nil && out.Aborted {
		_, rule, _ := b.cfg.AbortDate(u.Key, u.Initiated)
		b.action(opDeleteUpload, rule).Str("key", u.Key).Str("upload_id", u.UploadID).Send()
	}
	if err != nil {
		return fmt.Errorf("abort upload %s of %q: %w", u.UploadID, u.Key, err)
	}
	delete(b.st.uploadFailures, u.UploadID)
	return nil
}
