// Package notify delivers the bucket events the storage records in its
// outbox to their destinations, in the background, as S3 sends event
// notifications as s3.amazonaws.com: "designed to be delivered at least
// once. Typically, event notifications are delivered in seconds but can
// sometimes take a minute or longer" (S3 User Guide, "Amazon S3 Event
// Notifications").
//
// The storage wakes the deliverer once it confirms an event (Settings'
// Signal), and the deliverer looks at the outbox every Poll besides, in
// case a wake-up was missed. A pass reads up to Batch pending events that
// are due, each bucket's in the order they were recorded, the buckets in
// turn, starting after the last bucket the previous full batch reached,
// and delivers them with up to Workers buckets at once and one event in
// flight per bucket. The events waiting for a retry, and the buckets with
// a delivery under way, are left out of the read, so neither takes up the
// batch: an event waiting to be retried, or many, do not hold back the
// bucket's later events, nor do many buckets the others, so
// under retries events may arrive out of order, which S3 allows: events
// "aren't guaranteed to arrive in the same order" (S3 User Guide, "Event
// notification types and destinations"), and the sequencer orders the
// events of one key.
//
// A failure is retried, Backoff after the first, doubling up to
// MaxBackoff, until MaxAttempts deliveries failed; a destination that
// does not exist or does not allow the service counts too, since it may
// be fixed meanwhile. An event is dropped at once when the destination
// refuses its content (contracts.ErrDestinationRejected), as SQS "rejects
// the notification and doesn't deliver the event" over its
// MaximumMessageSize, or when its service has no endpoint now. An event
// recorded more than MaxAge ago is dropped when it is read. Drops are
// logged at Warn, summarized per bucket, reason and destination, each at
// most once every Poll, so neither a large backlog dropped at once nor a
// steady trickle logs a line per event; each drop is logged at Debug. An
// acknowledgement that fails is retried with the same backoff.
//
// The attempts are counted in memory, kept across a Close and Init and
// lost on exit, so a start grants an event another MaxAttempts deliveries;
// MaxAge bounds that. An event in flight when the service closes stays in
// the outbox and is delivered again later, a duplicate S3 allows ("On
// rare occasions ... duplicate S3 Event Notifications").
//
// Every Poll, the deliverer also confirms the events whose confirm failed
// once their change was committed (StorageProvider's ConfirmCommitted),
// and removes the empty outboxes of deleted buckets (RemoveEmptyOutboxes).
//
// Its timing is on its own clock, the system's in the app, never the
// storage's: an event's Created is the wall time it was recorded at.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/contracts"
)

// The defaults of Settings. AWS documents no retry policy for event
// notifications, so the numbers are this service's.
const (
	defaultBackoff     = time.Second
	defaultMaxBackoff  = time.Minute
	defaultMaxAge      = 24 * time.Hour
	defaultPoll        = 5 * time.Second
	defaultCallTimeout = 10 * time.Second
	defaultMaxAttempts = 10
	defaultWorkers     = 4
	defaultBatch       = 100
)

// Settings configures a Service. A zero duration or count is its default.
type Settings struct {
	// Clock is what the deliverer waits on and reads the time of, for
	// retries and the age bound; nil is contracts.SystemClock.
	Clock contracts.Clock
	// Ready, when set, is closed by the app's last ready hook, after the
	// storage recovered its events; nothing is delivered before it.
	Ready <-chan struct{}
	// Signal, when set, is sent on by the storage once it confirms an
	// event, to wake the deliverer.
	Signal <-chan struct{}
	// Account is the owner of every bucket, the aws:SourceAccount of each
	// delivery.
	Account string
	// Backoff is the wait after the first failure, doubled after each
	// next one up to MaxBackoff (1s and 60s).
	Backoff    time.Duration
	MaxBackoff time.Duration
	// MaxAge is how long after it was recorded an event is dropped (24h).
	MaxAge time.Duration
	// Poll is how often the outbox is read without a signal (5s).
	Poll time.Duration
	// CallTimeout bounds one delivery (10s).
	CallTimeout time.Duration
	// MaxAttempts is how many deliveries of an event may fail before it
	// is dropped (10).
	MaxAttempts int
	// Workers is how many buckets are delivered at once (4).
	Workers int
	// Batch is how many pending events a pass reads at most (100).
	Batch int
}

// withDefaults returns s with its zero fields set to their defaults.
func (s Settings) withDefaults() Settings {
	if s.Clock == nil {
		s.Clock = contracts.SystemClock
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	defInt := func(n *int, v int) {
		if *n <= 0 {
			*n = v
		}
	}
	def(&s.Backoff, defaultBackoff)
	def(&s.MaxBackoff, defaultMaxBackoff)
	def(&s.MaxAge, defaultMaxAge)
	def(&s.Poll, defaultPoll)
	def(&s.CallTimeout, defaultCallTimeout)
	defInt(&s.MaxAttempts, defaultMaxAttempts)
	defInt(&s.Workers, defaultWorkers)
	defInt(&s.Batch, defaultBatch)
	return s
}

// Service delivers bucket events.
type Service struct {
	logger gas.Logger
	store  contracts.StorageProvider
	dests  contracts.Destinations
	run    *runner
	drops  *dropLog
	// attempts are the failed deliveries of the events not yet
	// acknowledged, kept across a Close and Init. mu guards them, busy,
	// ended, epoch, running, lastStarted, readAfter, housekept and drops.
	attempts map[eventKey]*attempt
	// busy are the buckets a delivery run is under way for, running how
	// many, and lastStarted the bucket of the last run started: the next
	// pass starts runs from the bucket after it, by name, so no bucket
	// waits on the others for a worker. A run that ends sends on poke.
	busy map[string]bool
	// ended is the epoch each bucket's last run ended in, epoch counting
	// the runs ended: a pass that read its batch before a bucket's run
	// ended starts none for it, as the batch may hold events that run
	// delivered.
	ended       map[string]uint64
	poke        chan struct{}
	lastStarted string
	// readAfter is the bucket the next pass's read starts after: the
	// bucket of the last event a full batch held, so with more buckets
	// holding events than a batch reads, each is read in turn.
	readAfter string
	// housekept is when the last pass confirmed committed events and
	// removed empty outboxes.
	housekept time.Time
	settings  Settings
	running   int
	epoch     uint64
	mu        sync.Mutex
	// passMu is held for a whole pass, so a run a Close gave up waiting
	// for never passes alongside the next.
	passMu sync.Mutex
}

// eventKey names a pending event.
type eventKey struct{ bucket, id string }

// attempt is where an event's delivery is.
type attempt struct {
	// next is when the event may be delivered again.
	next time.Time
	// last is the failure of the last delivery.
	last error
	// n counts the failed deliveries, and ackFails the failed
	// acknowledgements of a settled event.
	n        int
	ackFails int
	// settled is set once the event was delivered or dropped and only its
	// AckEvent failed, which is all a later run tries again, at next.
	settled bool
}

var _ gas.Service = (*Service)(nil)

const serviceName = "tule/services/notifications"

// NewService returns the constructor gas uses to build a Service from
// settings, delivering the events of store through dests.
func NewService(
	settings Settings,
) func(gas.Logger, contracts.StorageProvider, contracts.Destinations) *Service {
	return func(
		logger gas.Logger, store contracts.StorageProvider, dests contracts.Destinations,
	) *Service {
		return &Service{
			logger:   logger.With().Str("service", serviceName).Logger(),
			store:    store,
			dests:    dests,
			settings: settings.withDefaults(),
			run:      newRunner(),
			attempts: map[eventKey]*attempt{},
			drops:    newDropLog(),
			busy:     map[string]bool{},
			ended:    map[string]uint64{},
			poke:     make(chan struct{}, 1),
		}
	}
}

// Name identifies the service to gas.
func (s *Service) Name() string {
	return serviceName
}

// Init starts the deliverer. gas's RestartService calls it again after
// Close, which starts it over with the attempts kept.
func (s *Service) Init(context.Context) error {
	s.run.begin()
	s.run.goBackground(s.loop) //nolint:contextcheck // it lives as long as the service
	return nil
}

// Close stops the deliverer, canceling the deliveries in flight, which
// stay in the outbox, waits for it, up to ctx's end, and logs the drops
// not yet logged.
func (s *Service) Close(ctx context.Context) error {
	err := s.run.stop(ctx)
	s.mu.Lock()
	s.drops.flush(s.logger, s.settings.Clock.Now(), 0)
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("%s: the deliverer did not stop in time: %w", s.Name(), err)
	}
	return nil
}

// loop waits for Ready, then passes at once, and again on each signal,
// when a bucket's delivery run ends, every Poll, and when the next retry
// is due, until ctx ends.
func (s *Service) loop(ctx context.Context) {
	if s.settings.Ready != nil {
		select {
		case <-ctx.Done():
			return
		case <-s.settings.Ready:
		}
	}
	for {
		next := s.pass(ctx)
		if ctx.Err() != nil || !s.wait(ctx, next) {
			return
		}
	}
}

// wait waits for a signal, a run's end, Poll, or next when it is set and
// earlier, and reports false when ctx ended first.
func (s *Service) wait(ctx context.Context, next time.Time) bool {
	wait := s.settings.Poll
	if !next.IsZero() {
		wait = min(wait, max(next.Sub(s.settings.Clock.Now()), 0))
	}
	select {
	case <-ctx.Done():
		return false
	case <-s.settings.Signal:
	case <-s.poke:
	case <-s.settings.Clock.After(wait):
	}
	return true
}

// pass reads one batch of the pending events that are due, leaving out
// those waiting for a retry and the buckets with a run under way, which
// read none until the run ends; drops those recorded too long ago; and
// starts a delivery run for each bucket with an event due that has none
// under way, while fewer than Workers are (see start). The read starts
// after the bucket of the last event the previous full batch held. It
// returns when the next pass is due earlier than Poll: now when the batch
// was full and it dropped events, so a backlog drains without waiting,
// the earliest retry, or zero for neither. A run that ends wakes the loop
// for the next pass.
func (s *Service) pass(ctx context.Context) time.Time {
	s.passMu.Lock()
	defer s.passMu.Unlock()
	now := s.settings.Clock.Now()
	s.housekeep(ctx, now)
	s.mu.Lock()
	read := s.epoch
	q, skipped := s.query(now)
	s.mu.Unlock()
	evs, err := s.store.PendingEvents(ctx, s.settings.Batch, q)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("failed to read the pending bucket events").Err("error", err).Send()
		}
		return time.Time{}
	}
	full := len(evs) == s.settings.Batch
	s.forget(evs, skipped, full)
	order, queues, dropped := s.queue(ctx, evs, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readAfter = ""
	if full {
		s.readAfter = evs[len(evs)-1].Bucket
	}
	s.start(order, queues, read) //nolint:contextcheck // the runs live as long as the service's run
	end := s.settings.Clock.Now()
	s.drops.flush(s.logger, end, s.settings.Poll)
	if full && dropped > 0 {
		return end
	}
	return s.nextRetry()
}

// query returns the PendingEvents query of a pass at now: it skips the
// events waiting for a retry, and every event of a bucket with a run under
// way, and records each it skips in skipped, which only the read writes.
// The caller holds mu.
func (s *Service) query(now time.Time) (contracts.PendingQuery, map[eventKey]bool) {
	busy := maps.Clone(s.busy)
	waiting := map[eventKey]bool{}
	for k, a := range s.attempts {
		if now.Before(a.next) {
			waiting[k] = true
		}
	}
	skipped := map[eventKey]bool{}
	return contracts.PendingQuery{
		After: s.readAfter,
		Skip: func(bucket, id string) bool {
			k := eventKey{bucket, id}
			if busy[bucket] || waiting[k] {
				skipped[k] = true
				return true
			}
			return false
		},
	}, skipped
}

// queue drops the events of evs recorded more than MaxAge before now, and
// returns the others by bucket, the buckets in name order, and how many it
// dropped. evs holds no event of a bucket with a run under way, so an
// event a run holds is never dropped meanwhile: its age is checked on a
// pass after the run. A settled event, delivered or dropped already, only
// waits for its acknowledgement, so it is never dropped for its age.
func (s *Service) queue(
	ctx context.Context, evs []contracts.PendingEvent, now time.Time,
) ([]string, map[string][]contracts.PendingEvent, int) {
	queues := map[string][]contracts.PendingEvent{}
	dropped := 0
	for _, e := range evs {
		if now.Sub(e.Created) > s.settings.MaxAge && !s.settled(eventKey{e.Bucket, e.ID}) {
			s.drop(ctx, e, reasonAge, nil)
			dropped++
			continue
		}
		queues[e.Bucket] = append(queues[e.Bucket], e)
	}
	return slices.Sorted(maps.Keys(queues)), queues, dropped
}

// settled reports whether k is settled, only its acknowledgement left.
func (s *Service) settled(k eventKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attempts[k]
	return a != nil && a.settled
}

// start starts a delivery run for each bucket of order, from the one after
// lastStarted and wrapping around, that has an event due in its queue,
// no run under way and none ended since the queues were read, in epoch
// read, while fewer than Workers runs are (see runBucket). The caller
// holds mu.
func (s *Service) start(
	order []string, queues map[string][]contracts.PendingEvent, read uint64,
) {
	i, _ := slices.BinarySearch(order, s.lastStarted)
	if i < len(order) && order[i] == s.lastStarted {
		i++
	}
	for _, b := range append(order[i:], order[:i]...) {
		if s.running >= s.settings.Workers {
			return
		}
		if s.busy[b] || s.ended[b] > read || !s.anyDue(queues[b]) {
			continue
		}
		delete(s.ended, b)
		if !s.run.goBackground(s.runBucket(b, queues[b])) {
			return
		}
		s.busy[b] = true
		s.running++
		s.lastStarted = b
	}
}

// runBucket returns the delivery run of bucket's queue q: it delivers q,
// then marks the run ended, logs the drops that are due (see dropLog), and
// wakes the loop.
func (s *Service) runBucket(bucket string, q []contracts.PendingEvent) func(context.Context) {
	return func(ctx context.Context) {
		s.deliverBucket(ctx, q)
		s.mu.Lock()
		delete(s.busy, bucket)
		s.running--
		s.epoch++
		s.ended[bucket] = s.epoch
		s.drops.flush(s.logger, s.settings.Clock.Now(), s.settings.Poll)
		s.mu.Unlock()
		select {
		case s.poke <- struct{}{}:
		default:
		}
	}
}

// anyDue reports whether an event of q is due: never tried, or past its
// retry time, a delivery's or, for a settled event, an acknowledgement's.
// The caller holds mu.
func (s *Service) anyDue(q []contracts.PendingEvent) bool {
	now := s.settings.Clock.Now()
	for _, e := range q {
		a := s.attempts[eventKey{e.Bucket, e.ID}]
		if a == nil || !now.Before(a.next) {
			return true
		}
	}
	return false
}

// housekeep confirms the committed events and removes the empty outboxes
// of deleted buckets, at most once every Poll. Failures are logged; both
// are tried again at the next one.
func (s *Service) housekeep(ctx context.Context, now time.Time) {
	s.mu.Lock()
	due := s.housekept.IsZero() || now.Sub(s.housekept) >= s.settings.Poll
	if due {
		s.housekept = now
	}
	s.mu.Unlock()
	if !due {
		return
	}
	n, err := s.store.ConfirmCommitted(ctx)
	if err != nil && ctx.Err() == nil {
		s.logger.Error("failed to confirm committed bucket events").Err("error", err).Send()
	}
	if n > 0 {
		s.logger.Info("confirmed committed bucket events").Int("confirmed", n).Send()
	}
	if _, err = s.store.RemoveEmptyOutboxes(ctx); err != nil && ctx.Err() == nil {
		s.logger.Error("failed to remove empty event outboxes").Err("error", err).Send()
	}
}

// forget drops the attempts of the events no longer pending, when the
// batch evs was not full: evs and the events skipped reading it are every
// pending event.
func (s *Service) forget(evs []contracts.PendingEvent, skipped map[eventKey]bool, full bool) {
	if full {
		return
	}
	live := make(map[eventKey]bool, len(evs))
	for _, e := range evs {
		live[eventKey{e.Bucket, e.ID}] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.attempts {
		if !live[k] && !skipped[k] {
			delete(s.attempts, k)
		}
	}
}

// nextRetry returns when the earliest retry not yet due is, zero for
// none. A retry due already is started by this pass, or, for a bucket
// with a run under way or with every worker busy, once a run ends, which
// wakes the loop. The caller holds mu.
func (s *Service) nextRetry() time.Time {
	now := s.settings.Clock.Now()
	var next time.Time
	for k, a := range s.attempts {
		if s.busy[k.bucket] || !a.next.After(now) {
			continue
		}
		if next.IsZero() || a.next.Before(next) {
			next = a.next
		}
	}
	return next
}

// deliverBucket delivers the events of q, one bucket's in the order they
// were recorded, one at a time. An event not yet due for a retry is
// passed over, and so is one whose delivery fails, until its retry: the
// bucket's later events do not wait for it. A Close's cancellation stops
// it, the event in flight left as it was.
func (s *Service) deliverBucket(ctx context.Context, q []contracts.PendingEvent) {
	for _, e := range q {
		if ctx.Err() != nil {
			return
		}
		settled, waiting := s.due(eventKey{e.Bucket, e.ID})
		switch {
		case waiting:
		case settled:
			s.ack(ctx, e)
		default:
			s.deliverOne(ctx, e)
		}
	}
}

// due reports whether k is settled, only its acknowledgement left to do,
// and whether it is waiting for a retry, of its delivery or of that
// acknowledgement.
func (s *Service) due(k eventKey) (settled, waiting bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attempts[k]
	if a == nil {
		return false, false
	}
	return a.settled, s.settings.Clock.Now().Before(a.next)
}

// deliverOne delivers e, drops it, or counts the failure for a retry. A
// Close's cancellation is not counted.
func (s *Service) deliverOne(ctx context.Context, e contracts.PendingEvent) {
	err := s.deliver(ctx, e)
	switch {
	case err == nil:
		s.ack(ctx, e)
	case ctx.Err() != nil:
	case errors.Is(err, contracts.ErrDestinationRejected):
		s.drop(ctx, e, reasonRejected, err)
	case errors.Is(err, contracts.ErrDestinationUnconfigured):
		s.drop(ctx, e, reasonUnconfigured, err)
	case s.failed(eventKey{e.Bucket, e.ID}, err):
		s.drop(ctx, e, reasonAttempts, err)
	}
}

// deliver sends e to its destination, as for its bucket and the owner.
func (s *Service) deliver(ctx context.Context, e contracts.PendingEvent) error {
	ctx, cancel := context.WithTimeout(ctx, s.settings.CallTimeout)
	defer cancel()
	//nolint:wrapcheck // the contracts errors, which the caller matches
	return s.dests.Deliver(ctx, e.Destination, e.Body, contracts.SourceRef{
		ARN: "arn:aws:s3:::" + e.Bucket, Account: s.settings.Account,
	})
}

// failed counts a failed delivery of k, err, and sets when it is tried
// again, reporting whether that was the last attempt allowed.
func (s *Service) failed(k eventKey, err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attempts[k]
	if a == nil {
		a = &attempt{}
		s.attempts[k] = a
	}
	a.n++
	a.last = err
	if a.n >= s.settings.MaxAttempts {
		return true
	}
	a.next = s.settings.Clock.Now().Add(s.backoff(a.n))
	s.logger.Debug("failed to deliver a bucket event; retrying").
		Str("bucket", k.bucket).Str("id", k.id).Int("attempt", a.n).Err("error", err).Send()
	return false
}

// backoff is the wait after the nth failed delivery: Backoff doubled n-1
// times, at most MaxBackoff.
func (s *Service) backoff(n int) time.Duration {
	d := s.settings.Backoff
	for i := 1; i < n && d < s.settings.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, s.settings.MaxBackoff)
}

// ack acknowledges e, delivered or dropped. A failure is logged, and
// marks e settled, so a later run only acknowledges it again, after the
// backoff of its failed acknowledgements: an outbox that refuses every
// removal is tried at most once every MaxBackoff.
func (s *Service) ack(ctx context.Context, e contracts.PendingEvent) {
	k := eventKey{e.Bucket, e.ID}
	err := s.store.AckEvent(ctx, e.Bucket, e.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.logger.Error("failed to remove a settled bucket event from the outbox").
			Str("bucket", e.Bucket).Str("id", e.ID).Err("error", err).Send()
		a := s.attempts[k]
		if a == nil {
			a = &attempt{}
			s.attempts[k] = a
		}
		a.settled = true
		a.ackFails++
		a.next = s.settings.Clock.Now().Add(s.backoff(a.ackFails))
		return
	}
	delete(s.attempts, k)
}

// The reasons an event is dropped.
const (
	reasonAge          = "recorded too long ago"
	reasonRejected     = "the destination refuses the message"
	reasonUnconfigured = "the destination's service is not configured"
	reasonAttempts     = "every delivery attempt failed"
)

// drop logs e's drop for reason, the last delivery failing with err, and
// acknowledges it.
func (s *Service) drop(ctx context.Context, e contracts.PendingEvent, reason string, err error) {
	s.mu.Lock()
	if err == nil {
		if a := s.attempts[eventKey{e.Bucket, e.ID}]; a != nil {
			err = a.last
		}
	}
	key, name := describe(e.Body)
	s.drops.add(e, key, name, reason, err)
	s.mu.Unlock()
	s.logger.Debug("dropped a bucket event").
		Str("bucket", e.Bucket).Str("id", e.ID).Str("key", key).Str("event", name).
		Str("destination", e.Destination.ARN).Str("reason", reason).Err("error", err).Send()
	s.ack(ctx, e)
}

// describe returns the key, URL-encoded, and the event name of the
// message body, empty when it is not one.
func describe(body []byte) (key, name string) {
	var msg struct {
		Records []struct {
			EventName string `json:"eventName"`
			S3        struct {
				Object struct {
					Key string `json:"key"`
				} `json:"object"`
			} `json:"s3"`
		} `json:"Records"`
	}
	if json.Unmarshal(body, &msg) != nil || len(msg.Records) == 0 {
		return "", ""
	}
	return msg.Records[0].S3.Object.Key, msg.Records[0].EventName
}

// dropLog summarizes drops by bucket, reason and destination, and logs
// each group at most once every Poll: a group's first drop is logged at
// once, and those after it within the Poll in one line once it is over,
// so a steady trickle of drops, or a large backlog dropped at once, does
// not log a line per event. Close logs what is left.
type dropLog struct {
	groups map[dropGroup]*dropSummary
	// flushed is when each group was last logged, kept for a Poll after.
	flushed map[dropGroup]time.Time
}

type dropGroup struct{ bucket, reason, destination string }

// dropSummary is a group's count, and its first drop's details.
type dropSummary struct {
	err            error
	id, key, event string
	count          int
}

func newDropLog() *dropLog {
	return &dropLog{groups: map[dropGroup]*dropSummary{}, flushed: map[dropGroup]time.Time{}}
}

func (l *dropLog) add(e contracts.PendingEvent, key, name, reason string, err error) {
	g := dropGroup{bucket: e.Bucket, reason: reason, destination: e.Destination.ARN}
	sum := l.groups[g]
	if sum == nil {
		sum = &dropSummary{id: e.ID, key: key, event: name, err: err}
		l.groups[g] = sum
	}
	sum.count++
}

// flush logs a Warn line, in order, for each group last logged every ago
// or more, or never, and forgets it: every 0 logs every group.
func (l *dropLog) flush(logger gas.Logger, now time.Time, every time.Duration) {
	groups := slices.SortedFunc(maps.Keys(l.groups), func(a, b dropGroup) int {
		return strings.Compare(a.bucket+"\x00"+a.reason+"\x00"+a.destination,
			b.bucket+"\x00"+b.reason+"\x00"+b.destination)
	})
	for _, g := range groups {
		if last, ok := l.flushed[g]; ok && every > 0 && now.Sub(last) < every {
			continue
		}
		sum := l.groups[g]
		ev := logger.Warn("dropped bucket events").
			Str("bucket", g.bucket).Str("destination", g.destination).Str("reason", g.reason).
			Int("count", sum.count).Str("first_id", sum.id).Str("first_key", sum.key).
			Str("first_event", sum.event)
		if sum.err != nil {
			ev = ev.Err("error", sum.err)
		}
		ev.Send()
		delete(l.groups, g)
		l.flushed[g] = now
	}
	for g, last := range l.flushed {
		if _, pending := l.groups[g]; !pending && now.Sub(last) >= every {
			delete(l.flushed, g)
		}
	}
}
