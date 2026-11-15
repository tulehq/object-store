package dirimpl

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// A change that causes a bucket event records it in the bucket's outbox,
// a directory of eventsDir named after the bucket, in two steps, both
// under the key's lock and without network I/O:
//
//   - before the change's commit point, an intent, "<id>.intent", which
//     holds the event as known then and a witness: what the store holds
//     once the change is committed, and not before;
//   - after it, the event, "<id>.json", which holds the message rendered
//     for the destination, and the intent is removed.
//
// An id is "<seq:020d>-<rand.Text()>", seq the event's commit sequence, or
// a fresh one for an event without a sequencer, so the names sort as the
// events were recorded. A stop between the two steps leaves the intent,
// which RecoverEvents settles by its witness before the store serves: an
// event whose change was committed is confirmed, and the intent of one
// that was not is removed, so a committed change never loses its event.
//
// A failure to record an event never fails the change: it is logged, and
// the change goes on without its event. Only the hook a test sets (see
// Service.at) can stop a change at the new stages, as a crash would.

// Events is what a Service records bucket events with.
type Events struct {
	// Router decides where an event goes and renders its message. A nil
	// Router records no events.
	Router contracts.EventRouter
	// Signal, when set, is called once an event is confirmed, to wake its
	// deliverer. It must not block.
	Signal func()
}

// Stages of recording an event that Service.hook is called at.
const (
	// stageEventIntent: an event's intent is written, the change it is
	// for not yet committed.
	stageEventIntent = "eventIntent"
	// stageEventConfirmed: an event is written as pending, its intent not
	// yet removed.
	stageEventConfirmed = "eventConfirmed"
)

// The file extensions of an outbox, and the version of what they hold.
const (
	intentExt     = ".intent"
	eventExt      = ".json"
	recordVersion = 1
)

// eventIDRe matches an event's id: a zero-padded sequence and a
// rand.Text.
var eventIDRe = regexp.MustCompile(`^[0-9]{20}-[A-Z2-7]{26}$`)

// witnessKind names what an intent's change leaves in the store once it is
// committed.
type witnessKind string

// The witnesses of the changes that record events.
const (
	// witnessEntry: the key's manifest holds the entry VersionID, of
	// sequence Seq (a version or marker committed).
	witnessEntry witnessKind = "entry"
	// witnessNoEntry: the key's manifest does not hold the entry
	// VersionID, or is empty or absent (a version deleted).
	witnessNoEntry witnessKind = "noEntry"
	// witnessWrite: the object at keyHash has WriteID in its trailer (an
	// object written in place).
	witnessWrite witnessKind = "write"
	// witnessGone: there is no object at keyHash (an object deleted in
	// place).
	witnessGone witnessKind = "gone"
	// witnessSideFile: the side file File holds bytes of digest SHA256
	// (tags or an ACL stored).
	witnessSideFile witnessKind = "sideFile"
)

// witness is what an intent's change leaves in the store once committed.
type witness struct {
	Kind      witnessKind `json:"kind"`
	VersionID string      `json:"versionId,omitempty"`
	WriteID   string      `json:"writeId,omitempty"`
	File      string      `json:"file,omitempty"`
	SHA256    string      `json:"sha256,omitempty"`
	Seq       int64       `json:"seq,omitempty"`
}

// intentRecord is what an intent file holds. Generation is the bucket's
// when the event was recorded: a bucket of another generation is not the
// one the witness is about.
type intentRecord struct {
	Created     time.Time             `json:"created"`
	Destination contracts.Destination `json:"destination"`
	Witness     witness               `json:"witness"`
	ID          string                `json:"id"`
	Generation  string                `json:"generation"`
	Event       contracts.BucketEvent `json:"event"`
	V           int                   `json:"v"`
}

// eventRecord is what a pending event's file holds: Body is the message
// rendered for Destination.
type eventRecord struct {
	Created     time.Time             `json:"created"`
	Destination contracts.Destination `json:"destination"`
	ID          string                `json:"id"`
	Body        []byte                `json:"body"`
	V           int                   `json:"v"`
}

// eventFile is the file of event id of bucket's outbox with extension ext.
func eventFile(bucket, id, ext string) string {
	return filepath.Join(eventsDir, bucket, id+ext)
}

// digest is the hex sha256 of b, a side file's witness.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// pendingEvent is an event whose intent is written, for the change about
// to be committed. Its methods do nothing on nil, the event of a change
// that records none.
type pendingEvent struct {
	s      *Service
	bucket string
	rec    intentRecord
}

// event records the intent of ev, the event of a change of ev.Key in
// ev.Bucket, to which ctx is pinned, about to be committed, and returns
// it: nil when the bucket has no notification configuration, which costs
// one ENOENT, when the configuration routes ev nowhere, or when ev.Name is
// "". Route is given ev with its Name, Bucket, Key and Time; build, called
// only once ev is routed, fills in the rest and returns the change's
// witness, or false for a change that causes no event after all. A failure
// to record is logged and records none. The error is only the
// stageEventIntent hook's. The caller holds the key's lock.
func (s *Service) event(
	ctx context.Context,
	ev contracts.BucketEvent,
	build func(ev *contracts.BucketEvent) (witness, bool, error),
) (*pendingEvent, error) {
	if ev.Name == "" || s.events.Router == nil {
		return nil, nil
	}
	d, gen, ok := s.routeEvent(ctx, &ev)
	if !ok {
		return nil, nil
	}
	w, ok, err := build(&ev)
	if err != nil {
		s.eventFailed("record", &ev, err)
		return nil, nil
	}
	if !ok {
		return nil, nil
	}
	ev.Source, _ = contracts.EventSourceFrom(ctx)
	seq := ev.Sequence
	if seq == 0 {
		seq = s.nextSeq(0)
	}
	p := &pendingEvent{s: s, bucket: ev.Bucket, rec: intentRecord{
		V: recordVersion, ID: fmt.Sprintf("%020d-%s", seq, rand.Text()), Created: time.Now(),
		Generation: gen, Event: ev, Destination: d, Witness: w,
	}}
	raw, err := json.Marshal(&p.rec)
	if err == nil {
		err = s.writeDurable(eventFile(p.bucket, p.rec.ID, intentExt), raw)
	}
	if err != nil {
		s.eventFailed("record", &ev, err)
		return nil, nil
	}
	if err = s.at(stageEventIntent); err != nil {
		return nil, err
	}
	return p, nil
}

// routeEvent returns the destination the notification configuration of
// ev.Bucket, the bucket ctx is pinned to, sends ev to, and the bucket's
// generation, which the configuration must have been stored for: false
// for none. A failure to read or decode the configuration is logged.
func (s *Service) routeEvent(
	ctx context.Context,
	ev *contracts.BucketEvent,
) (contracts.Destination, string, bool) {
	gen, err := requirePin(ctx, ev.Bucket)
	if err != nil {
		s.eventFailed("route", ev, err)
		return contracts.Destination{}, "", false
	}
	doc, err := s.root.ReadFile(configFile(contracts.ConfigNotification, ev.Bucket))
	if errors.Is(err, fs.ErrNotExist) {
		return contracts.Destination{}, "", false
	}
	if err != nil {
		s.eventFailed("route", ev, fmt.Errorf("read the notification configuration: %w", err))
		return contracts.Destination{}, "", false
	}
	d, ok, err := s.events.Router.Route(doc, gen, ev)
	if err != nil {
		s.eventFailed("route", ev, err)
		return contracts.Destination{}, "", false
	}
	return d, gen, ok
}

// eventFailed logs err, a failure at step of recording ev, which the
// change it is for goes on without.
func (s *Service) eventFailed(step string, ev *contracts.BucketEvent, err error) {
	s.logger.Error("failed to "+step+" a bucket event").
		Str("bucket", ev.Bucket).Str("key", ev.Key).Str("event", string(ev.Name)).
		Err("error", err).Send()
}

// confirm records p's event as committed: fill, when set, completes the
// event first; then its message is rendered and written as pending, the
// intent removed and the deliverer signaled. A failure is logged, and
// leaves the intent for RecoverEvents. The error is only the
// stageEventConfirmed hook's. The caller holds the key's lock.
func (p *pendingEvent) confirm(fill func(ev *contracts.BucketEvent)) error {
	if p == nil {
		return nil
	}
	if fill != nil {
		fill(&p.rec.Event)
	}
	s := p.s
	if err := s.writeEvent(p.bucket, &p.rec); err != nil {
		s.eventFailed("confirm", &p.rec.Event, err)
		return nil
	}
	if err := s.at(stageEventConfirmed); err != nil {
		return err
	}
	if err := s.removeIfExists(eventFile(p.bucket, p.rec.ID, intentExt)); err != nil {
		s.eventFailed("confirm", &p.rec.Event, fmt.Errorf("remove the intent: %w", err))
	}
	s.signal()
	return nil
}

// committed calls the stageCommitted hook, once the change p is for was
// committed, then confirms p (see confirm). The error is only the hooks'.
func (s *Service) committed(p *pendingEvent, fill func(ev *contracts.BucketEvent)) error {
	if err := s.at(stageCommitted); err != nil {
		return err
	}
	return p.confirm(fill)
}

// abort removes p's intent, for a change that was not committed. A
// failure is logged, and leaves the intent for RecoverEvents, whose
// witness check removes it.
func (p *pendingEvent) abort() {
	if p == nil {
		return
	}
	if err := p.s.removeIfExists(eventFile(p.bucket, p.rec.ID, intentExt)); err != nil {
		p.s.eventFailed("abort", &p.rec.Event, err)
	}
}

// settle is for a change whose commit step failed, which may or may not
// have committed it: p's witness decides, as it would for RecoverEvents,
// and p is confirmed or aborted. A failure to check the witness is logged,
// and leaves the intent for RecoverEvents. The error is only the
// stageEventConfirmed hook's. The caller holds the key's lock.
func (p *pendingEvent) settle(ctx context.Context) error {
	if p == nil {
		return nil
	}
	held, err := p.s.witnessed(ctx, p.bucket, &p.rec)
	if err != nil {
		p.s.eventFailed("settle", &p.rec.Event, err)
		return nil
	}
	if !held {
		p.abort()
		return nil
	}
	return p.confirm(nil)
}

// signal wakes the deliverer, when there is one.
func (s *Service) signal() {
	if s.events.Signal != nil {
		s.events.Signal()
	}
}

// writeEvent renders rec's event for its destination and writes it as
// pending in bucket's outbox.
func (s *Service) writeEvent(bucket string, rec *intentRecord) error {
	if s.events.Router == nil {
		return errors.New("no event router to render the message with")
	}
	body, err := s.events.Router.Render(rec.Destination, &rec.Event)
	if err != nil {
		return fmt.Errorf("render the message: %w", err)
	}
	raw, err := json.Marshal(&eventRecord{
		V: recordVersion, ID: rec.ID, Created: rec.Created, Destination: rec.Destination,
		Body: body,
	})
	if err != nil {
		return fmt.Errorf("encode the event: %w", err)
	}
	if err = s.writeDurable(eventFile(bucket, rec.ID, eventExt), raw); err != nil {
		return fmt.Errorf("store the event: %w", err)
	}
	return nil
}

// witnessed reports whether the store holds rec's witness, in bucket: the
// change rec is for was committed. For an object written in place, it
// fills in rec's event from the object: its ETag, size and modification
// time, which were not known when the intent was written.
func (s *Service) witnessed(ctx context.Context, bucket string, rec *intentRecord) (bool, error) {
	w := &rec.Witness
	switch w.Kind {
	case witnessEntry, witnessNoEntry:
		return s.witnessedEntry(bucket, rec.Event.Key, w)
	case witnessGone:
		return s.witnessedGone(ctx, bucket, rec.Event.Key)
	case witnessWrite:
		return s.witnessedWrite(ctx, bucket, rec)
	case witnessSideFile:
		raw, err := s.root.ReadFile(w.File)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read %s: %w", w.File, err)
		}
		return digest(raw) == w.SHA256, nil
	}
	return false, fmt.Errorf("unknown witness %q", w.Kind)
}

// witnessedEntry is witnessed for witnessEntry and witnessNoEntry, of key
// in bucket.
func (s *Service) witnessedEntry(bucket, key string, w *witness) (bool, error) {
	m, _, err := s.readVersions(bucket, key)
	if err != nil {
		return false, err
	}
	i := -1
	if m != nil {
		i = m.find(w.VersionID)
	}
	if w.Kind == witnessNoEntry {
		return i < 0, nil
	}
	return i >= 0 && m.Versions[i].Seq == w.Seq, nil
}

// witnessedGone is witnessed for witnessGone, of key in bucket.
func (s *Service) witnessedGone(ctx context.Context, bucket, key string) (bool, error) {
	_, err := s.dir.HeadObject(ctx, &storage.HeadObjectInput{
		BaseObjectInput: dataInput(bucket, key, nil),
	})
	if errors.Is(err, storage.ErrObjectNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %q: %w", key, err)
	}
	return false, nil
}

// witnessedWrite is witnessed for witnessWrite.
func (s *Service) witnessedWrite(
	ctx context.Context,
	bucket string,
	rec *intentRecord,
) (bool, error) {
	key := rec.Event.Key
	obj, err := s.dir.GetObject(ctx, &storage.GetObjectInput{
		HeadObjectInput: storage.HeadObjectInput{BaseObjectInput: dataInput(bucket, key, nil)},
	})
	if errors.Is(err, storage.ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open %q: %w", key, err)
	}
	rs, ok := obj.Body.(io.ReadSeekCloser)
	if !ok {
		return false, errors.Join(fmt.Errorf("open %q: backend body %T is not seekable",
			key, obj.Body), obj.Body.Close())
	}
	tr, size, err := readTrailer(rs, obj.Size)
	if cErr := rs.Close(); cErr != nil {
		err = errors.Join(err, cErr)
	}
	if err != nil {
		return false, fmt.Errorf("read the trailer of %q: %w", key, err)
	}
	if tr.WriteID != rec.Witness.WriteID {
		return false, nil
	}
	info, err := objectInfo(key, tr, size)
	if err != nil {
		return false, fmt.Errorf("read the trailer of %q: %w", key, err)
	}
	rec.Event.ETag, rec.Event.Size, rec.Event.Time = info.ETag, info.Size, info.LastModified
	return true, nil
}

// RecoverEvents settles every intent left in the outboxes: an intent whose
// witness holds is confirmed, rendered again from what it and the store
// hold, and one whose witness does not, or whose bucket is of another
// generation now, is removed. An intent whose event was written already
// is removed, and counted as confirmed. Temp files left by an interrupted
// write are removed. A failure leaves the intent for the next start, and
// is returned, joined with the others, once every intent was tried. It
// runs before the store serves, so no change moves a witness meanwhile.
func (s *Service) RecoverEvents(ctx context.Context) (contracts.RecoveredEvents, error) {
	var out contracts.RecoveredEvents
	buckets, err := fs.ReadDir(s.root.FS(), eventsDir)
	if err != nil {
		return out, fmt.Errorf("%s: read the event outboxes: %w", s.Name(), err)
	}
	var errs []error
	for _, b := range buckets {
		if b.IsDir() {
			errs = append(errs, s.recoverBucket(ctx, b.Name(), &out))
		}
	}
	if out.Confirmed > 0 {
		s.signal()
	}
	if err = errors.Join(errs...); err != nil {
		return out, fmt.Errorf("%s: recover bucket events: %w", s.Name(), err)
	}
	return out, nil
}

// recoverBucket is RecoverEvents for bucket's outbox.
func (s *Service) recoverBucket(
	ctx context.Context,
	bucket string,
	out *contracts.RecoveredEvents,
) error {
	dir := filepath.Join(eventsDir, bucket)
	entries, err := fs.ReadDir(s.root.FS(), dir)
	if err != nil {
		return fmt.Errorf("read the outbox of bucket %s: %w", bucket, err)
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name()] = true
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if strings.Contains(name, ".tmp-") {
			if rmErr := s.removeIfExists(filepath.Join(dir, name)); rmErr != nil {
				errs = append(errs, fmt.Errorf("remove %s of bucket %s: %w", name, bucket, rmErr))
			}
			continue
		}
		id, ok := strings.CutSuffix(name, intentExt)
		if !ok || !eventIDRe.MatchString(id) {
			continue
		}
		if rErr := s.recoverIntent(ctx, bucket, id, names[id+eventExt], out); rErr != nil {
			errs = append(errs, fmt.Errorf("event %s of bucket %s: %w", id, bucket, rErr))
		}
	}
	return errors.Join(errs...)
}

// recoverIntent settles intent id of bucket, whose event was written
// already when written is set, and counts it in out.
func (s *Service) recoverIntent(
	ctx context.Context,
	bucket, id string,
	written bool,
	out *contracts.RecoveredEvents,
) error {
	name := eventFile(bucket, id, intentExt)
	if written {
		if err := s.removeIfExists(name); err != nil {
			return err
		}
		out.Confirmed++
		return nil
	}
	rec, err := s.readIntent(name, id)
	if err != nil {
		return err
	}
	held, err := s.recoveredWitness(ctx, bucket, rec)
	if err != nil {
		return err
	}
	if held {
		if err = s.writeEvent(bucket, rec); err != nil {
			return err
		}
	}
	if err = s.removeIfExists(name); err != nil {
		return err
	}
	if held {
		out.Confirmed++
	} else {
		out.Dropped++
	}
	return nil
}

// readIntent reads the intent file name, of event id.
func (s *Service) readIntent(name, id string) (*intentRecord, error) {
	raw, err := s.root.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read the intent: %w", err)
	}
	var rec intentRecord
	if err = json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("decode the intent: %w", err)
	}
	if rec.V != recordVersion || rec.ID != id {
		return nil, fmt.Errorf("the intent is of version %d, id %q", rec.V, rec.ID)
	}
	return &rec, nil
}

// recoveredWitness is witnessed for rec, an intent of bucket that
// RecoverEvents found, which does not hold when bucket is not of the
// generation rec was recorded in: the store's bucket is not the one rec's
// witness is about. That is logged, since only a confirm that failed in a
// running store leaves such an intent.
func (s *Service) recoveredWitness(
	ctx context.Context,
	bucket string,
	rec *intentRecord,
) (bool, error) {
	gen, err := s.generation(ctx, bucket)
	if errors.Is(err, storage.ErrBucketNotFound) {
		gen, err = "", nil
	}
	if err != nil {
		return false, err
	}
	if gen != rec.Generation {
		s.logger.Warn("dropped the intent of a bucket event of another bucket generation").
			Str("bucket", bucket).Str("key", rec.Event.Key).Str("event", string(rec.Event.Name)).
			Send()
		return false, nil
	}
	return s.witnessed(ctx, bucket, rec)
}

// PendingEvents returns up to n of the pending events, each bucket's in
// name order, which is the order they were recorded in, the buckets taken
// in turn. An event acknowledged while they are read is left out. One
// that cannot be read or decoded is logged and left out, and stays in the
// outbox, so it can be looked at.
func (s *Service) PendingEvents(_ context.Context, n int) ([]contracts.PendingEvent, error) {
	if n <= 0 {
		return nil, nil
	}
	queues, err := s.outboxes()
	if err != nil {
		return nil, err
	}
	var out []contracts.PendingEvent
	for round := 0; len(out) < n; round++ {
		taken := false
		for _, q := range queues {
			if round >= len(q.ids) || len(out) == n {
				continue
			}
			taken = true
			if ev, ok := s.readPending(q.bucket, q.ids[round]); ok {
				out = append(out, ev)
			}
		}
		if !taken {
			break
		}
	}
	return out, nil
}

// outbox is a bucket's outbox, with the ids of its pending events in name
// order.
type outbox struct {
	bucket string
	ids    []string
}

// outboxes returns the outboxes that hold pending events, in bucket name
// order.
func (s *Service) outboxes() ([]outbox, error) {
	buckets, err := fs.ReadDir(s.root.FS(), eventsDir)
	if err != nil {
		return nil, fmt.Errorf("%s: read the event outboxes: %w", s.Name(), err)
	}
	var out []outbox
	for _, b := range buckets {
		if !b.IsDir() {
			continue
		}
		ids, lErr := s.pendingIDs(b.Name())
		if lErr != nil {
			return nil, lErr
		}
		if len(ids) > 0 {
			out = append(out, outbox{bucket: b.Name(), ids: ids})
		}
	}
	return out, nil
}

// pendingIDs returns the ids of bucket's pending events, in name order.
func (s *Service) pendingIDs(bucket string) ([]string, error) {
	entries, err := fs.ReadDir(s.root.FS(), filepath.Join(eventsDir, bucket))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: read the outbox of bucket %s: %w", s.Name(), bucket, err)
	}
	var ids []string
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), eventExt); ok && eventIDRe.MatchString(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// readPending returns pending event id of bucket, false when it is gone or
// cannot be read, which is logged.
func (s *Service) readPending(bucket, id string) (contracts.PendingEvent, bool) {
	raw, err := s.root.ReadFile(eventFile(bucket, id, eventExt))
	if errors.Is(err, fs.ErrNotExist) {
		return contracts.PendingEvent{}, false
	}
	var rec eventRecord
	if err == nil {
		err = json.Unmarshal(raw, &rec)
	}
	if err == nil && (rec.V != recordVersion || rec.ID != id) {
		err = fmt.Errorf("the event is of version %d, id %q", rec.V, rec.ID)
	}
	if err != nil {
		s.logger.Error("failed to read a pending bucket event").
			Str("bucket", bucket).Str("id", id).Err("error", err).Send()
		return contracts.PendingEvent{}, false
	}
	return contracts.PendingEvent{
		Created: rec.Created, Destination: rec.Destination, Bucket: bucket, ID: id, Body: rec.Body,
	}, true
}

// AckEvent removes pending event id of bucket. One already removed is not
// an error; a bucket name or id that cannot name an event is.
func (s *Service) AckEvent(_ context.Context, bucket, id string) error {
	if !bucketNameRe.MatchString(bucket) || !eventIDRe.MatchString(id) {
		return fmt.Errorf("%s: no event %q of bucket %q can exist", s.Name(), id, bucket)
	}
	if err := s.removeIfExists(eventFile(bucket, id, eventExt)); err != nil {
		return fmt.Errorf("%s: remove event %s of bucket %s: %w", s.Name(), id, bucket, err)
	}
	return nil
}
