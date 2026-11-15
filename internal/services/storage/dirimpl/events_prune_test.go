package dirimpl

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tule.dev/object-store/internal/contracts"
)

// removeEmpty runs RemoveEmptyOutboxes and returns how many it removed.
func (f *eventFixture) removeEmpty() int {
	f.t.Helper()
	n, err := f.s.RemoveEmptyOutboxes(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

// hasOutbox reports whether "bucket"'s outbox directory exists.
func (f *eventFixture) hasOutbox() bool {
	f.t.Helper()
	return f.exists(filepath.Join(eventsDir, "bucket"))
}

// ackAll acknowledges every pending event and returns how many there were.
func (f *eventFixture) ackAll() int {
	f.t.Helper()
	evs, err := f.s.PendingEvents(context.Background(), 1000, contracts.PendingQuery{})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, e := range evs {
		if err = f.s.AckEvent(context.Background(), e.Bucket, e.ID); err != nil {
			f.t.Fatal(err)
		}
	}
	return len(evs)
}

// The empty outbox of a bucket that exists is kept: its next event would
// only create it again.
func TestRemoveEmptyOutboxesKeepsALiveBucketsOutbox(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	f.put("k", "one")
	if n := f.ackAll(); n != 1 {
		t.Fatalf("pending %d, want 1", n)
	}
	f.release()
	if n := f.removeEmpty(); n != 0 || !f.hasOutbox() {
		t.Fatalf("a live bucket's empty outbox: removed %d, exists %v", n, f.hasOutbox())
	}
}

// The outbox of a deleted bucket is removed once its events are acked.
func TestRemoveEmptyOutboxOfADeletedBucket(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	f.put("k", "one")
	if _, err := f.s.DeleteObject(f.ctx, oref("bucket", "k")); err != nil {
		t.Fatal(err)
	}
	if err := f.deleteBucket(); err != nil {
		t.Fatal(err)
	}
	if n := f.removeEmpty(); n != 0 || !f.hasOutbox() {
		t.Fatalf("removed %d outboxes with events pending", n)
	}
	if n := f.ackAll(); n != 2 {
		t.Fatalf("pending %d, want 2", n)
	}
	if n := f.removeEmpty(); n != 1 || f.hasOutbox() {
		t.Fatalf("removed %d, exists %v", n, f.hasOutbox())
	}
}

// A removal run while a write of the bucket stands between creating its
// outbox directory and writing into it leaves the directory, so the
// write's event is recorded.
func TestRemoveEmptyOutboxesMidWriteKeepsTheEvent(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	f.put("k", "v1")
	f.ackAll()
	removed := -1
	f.s.hook = func(stage string) error {
		if stage == stageDurableDir && removed < 0 {
			removed, _ = f.s.RemoveEmptyOutboxes(f.ctx)
		}
		return nil
	}
	f.put("k", "v2")
	f.s.hook = nil
	if removed != 0 {
		t.Fatalf("removed %d outboxes during the write", removed)
	}
	if n := f.ackAll(); n != 1 {
		t.Fatalf("the write's event was lost: %d pending", n)
	}
}

// A deleted bucket created again while its empty outbox is being removed
// does not lose its first event: the removal holds the bucket's write
// lock, so CreateBucket, and so the write, waits for the removal to end,
// and the write creates the outbox again. Without the lock, the write
// would find the directory, the removal take it away, and the write fail.
func TestRemoveEmptyOutboxesRacesACreatedBucket(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	f.put("k", "v1")
	if _, err := f.s.DeleteObject(f.ctx, oref("bucket", "k")); err != nil {
		t.Fatal(err)
	}
	if err := f.deleteBucket(); err != nil {
		t.Fatal(err)
	}
	f.ackAll()
	dir := filepath.Join(eventsDir, "bucket")

	written := make(chan error, 1)
	f.s.hook = f.interleaveRecreate(dir, written)
	removed := f.removeEmpty()
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	f.s.hook = nil
	if removed != 1 {
		t.Fatalf("removed %d, want the deleted bucket's outbox", removed)
	}
	if n := f.ackAll(); n != 1 {
		t.Fatalf("the created bucket's first event was lost: %d pending", n)
	}
}

// interleaveRecreate returns the hook of
// TestRemoveEmptyOutboxesRacesACreatedBucket: once the removal of dir,
// the empty outbox of the deleted "bucket", is about to remove it, a
// write that creates the bucket again starts, which the removal waits for,
// up to 200ms, to find its empty outbox; the write then waits, up to
// 200ms, for the removal to take the directory. Its result is sent on
// written.
func (f *eventFixture) interleaveRecreate(dir string, written chan<- error) func(string) error {
	reached := make(chan struct{})
	var reachedOnce, startOnce sync.Once
	return func(stage string) error {
		switch stage {
		case stageOutboxPrune:
			startOnce.Do(func() { go func() { written <- f.recreateAndPut() }() })
			select {
			case <-reached:
			case <-time.After(200 * time.Millisecond):
			}
		case stageDurableDir:
			if empty, _ := f.s.emptyDir(dir); !empty {
				return nil
			}
			reachedOnce.Do(func() { close(reached) })
			deadline := time.Now().Add(200 * time.Millisecond)
			for f.exists(dir) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
		}
		return nil
	}
}

// recreateAndPut creates "bucket", gives it a notification configuration
// and writes "k", whose event it records.
func (f *eventFixture) recreateAndPut() error {
	ctx := context.Background()
	if _, err := f.s.CreateBucket(ctx, "bucket", nil); err != nil {
		return err
	}
	gen, _, err := f.s.GetBucketPolicy(ctx, "bucket")
	if err != nil && !errors.Is(err, contracts.ErrNoSuchBucketPolicy) {
		return err
	}
	pctx, release, err := f.s.Pin(ctx, contracts.BucketRef{Name: "bucket", Generation: gen})
	if err != nil {
		return err
	}
	defer release()
	if err = f.s.PutBucketConfig(pctx, "bucket", contracts.ConfigNotification,
		[]byte(`{"generation":"`+gen+`"}`)); err != nil {
		return err
	}
	_, err = f.s.PutObject(pctx, "bucket", "k", strings.NewReader("v2"),
		contracts.PutObjectInput{}, contracts.Precondition{})
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("the write failed on a removed outbox")
	}
	return err
}
