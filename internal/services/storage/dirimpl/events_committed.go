package dirimpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// witnessCommitted is the witness of an event marked committed: the change
// was committed, whatever the store holds now.
const witnessCommitted witnessKind = "committed"

// markCommitted marks p's event committed, after its confirm failed once
// its change was: it writes the event, as completed, with the witness
// witnessCommitted, as "<id>.committed", and removes the intent. When that
// cannot be written, as on a full disk, the intent is renamed to it as it
// is, without the details a write in place fills in, which
// ConfirmCommitted reads back from the object if it can. A failure is
// logged; the intent is then left as it was, for RecoverEvents. The caller
// holds the key's lock.
func (p *pendingEvent) markCommitted() {
	s, intent := p.s, eventFile(p.bucket, p.rec.ID, intentExt)
	mark := eventFile(p.bucket, p.rec.ID, committedExt)
	rec := p.rec
	rec.Witness = witness{Kind: witnessCommitted}
	raw, err := json.Marshal(&rec)
	if err == nil {
		err = s.unsynced(mark, s.writeDurable(mark, raw))
	}
	if err == nil {
		if rmErr := s.removeIfExists(intent); rmErr != nil {
			s.eventFailed("mark committed", &p.rec.Event,
				fmt.Errorf("remove the intent: %w", rmErr))
		}
		return
	}
	s.eventFailed("mark committed", &p.rec.Event, err)
	if err = s.root.Rename(intent, mark); err != nil {
		s.eventFailed("mark committed", &p.rec.Event, fmt.Errorf("rename the intent: %w", err))
		return
	}
	if err = s.syncDir(filepath.Dir(mark)); err != nil {
		s.eventFailed("mark committed", &p.rec.Event, fmt.Errorf("sync the outbox: %w", err))
	}
}

// ConfirmCommitted confirms the events marked committed, whose confirm
// failed once their change was committed (see markCommitted), without
// checking a witness or the bucket's generation: an event outlives its
// bucket. It returns how many it confirmed, and the failures, joined, of
// those it could not, which stay marked for the next call. It is safe to
// call while the store serves, as its deliverer does on each poll.
func (s *Service) ConfirmCommitted(ctx context.Context) (int, error) {
	s.committedMu.Lock()
	defer s.committedMu.Unlock()
	buckets, err := fs.ReadDir(s.root.FS(), eventsDir)
	if err != nil {
		return 0, fmt.Errorf("%s: read the event outboxes: %w", s.Name(), err)
	}
	n := 0
	var errs []error
	for _, b := range buckets {
		if !b.IsDir() {
			continue
		}
		done, bErr := s.confirmCommittedIn(ctx, b.Name())
		n += done
		errs = append(errs, bErr)
	}
	if n > 0 {
		s.signal()
	}
	if err = errors.Join(errs...); err != nil {
		return n, fmt.Errorf("%s: confirm committed bucket events: %w", s.Name(), err)
	}
	return n, nil
}

// confirmCommittedIn is ConfirmCommitted for bucket's outbox.
func (s *Service) confirmCommittedIn(ctx context.Context, bucket string) (int, error) {
	entries, err := fs.ReadDir(s.root.FS(), filepath.Join(eventsDir, bucket))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the outbox of bucket %s: %w", bucket, err)
	}
	n := 0
	var errs []error
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), committedExt)
		if !ok || !eventIDRe.MatchString(id) {
			continue
		}
		if cErr := s.confirmMarked(ctx, bucket, id); cErr != nil {
			errs = append(errs, fmt.Errorf("event %s of bucket %s: %w", id, bucket, cErr))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// confirmMarked confirms event id of bucket, marked committed: it writes
// the event, unless written already, then removes the mark and any intent
// left beside it.
func (s *Service) confirmMarked(ctx context.Context, bucket, id string) error {
	mark := eventFile(bucket, id, committedExt)
	_, err := s.root.Stat(eventFile(bucket, id, eventExt))
	if errors.Is(err, fs.ErrNotExist) {
		rec, rErr := s.readIntent(mark, id)
		if rErr != nil {
			return rErr
		}
		s.fillMarked(ctx, bucket, rec)
		err = s.writeEvent(bucket, rec)
	}
	if err != nil {
		return err
	}
	if err = s.removeIfExists(eventFile(bucket, id, intentExt)); err != nil {
		return err
	}
	return s.removeIfExists(mark)
}

// fillMarked fills in the details of rec, the committed mark of a write in
// place renamed from its intent, which lacks them, from the object if it
// is still the one written. Otherwise the event goes without them, its
// HasObject unset so its message leaves them out rather than report a size
// of 0 and no ETag, which is logged.
func (s *Service) fillMarked(ctx context.Context, bucket string, rec *intentRecord) {
	if rec.Witness.Kind != witnessWrite {
		return
	}
	held, err := s.witnessedWrite(ctx, bucket, rec)
	if err == nil && held {
		return
	}
	rec.Event.HasObject = false
	s.logger.Warn("a committed bucket event goes without its object's size and ETag").
		Str("bucket", bucket).Str("key", rec.Event.Key).Str("event", string(rec.Event.Name)).
		Err("error", err).Send()
}

// seqLeaseFile holds the sequence lease: a sequence above every one an
// event was recorded with, from which nextSeq resumes after a start (see
// leaseSeq). It is written only once an event is, so a store without
// notification configurations never has one.
var seqLeaseFile = filepath.Join(stateDir, "eventseq")

// seqLeaseAhead is how far above an event's sequence the lease is stored,
// so it is written about once a minute while events are recorded.
const seqLeaseAhead = int64(time.Minute)

// leaseSeq makes the stored lease exceed seq, the sequence of an event
// about to be recorded, before its intent is written, so the events
// recorded after a start sort after it even on a clock that went back:
// the key's object, which a write in place starts above (see
// legacyEntry), may be gone, deleted in place. The caller holds the key's
// lock.
func (s *Service) leaseSeq(seq int64) error {
	if seq < s.leased.Load() {
		return nil
	}
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if seq < s.leased.Load() {
		return nil
	}
	lease := seq + seqLeaseAhead
	if err := s.unsynced(seqLeaseFile, s.writeDurable(seqLeaseFile,
		[]byte(strconv.FormatInt(lease, 10)))); err != nil {
		return fmt.Errorf("store the sequence lease: %w", err)
	}
	s.leased.Store(lease)
	return nil
}

// resumeSeq resumes the commit sequence above the stored lease, if there
// is one. A lease that cannot be read or decoded is logged at Error, and
// the sequence resumes from the clock: the lease only orders events, which
// must not keep the store from starting. The next event stores a new one.
func (s *Service) resumeSeq() {
	raw, err := s.root.ReadFile(seqLeaseFile)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var lease int64
	if err == nil {
		lease, err = strconv.ParseInt(string(raw), 10, 64)
	}
	if err != nil {
		s.logger.Error("failed to read the sequence lease; events recorded after this "+
			"start may sort before earlier ones if the clock went back").
			Str("file", seqLeaseFile).Err("error", err).Send()
		return
	}
	s.seq.Store(lease)
	s.leased.Store(lease)
}
