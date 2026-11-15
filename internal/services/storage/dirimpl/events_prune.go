package dirimpl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/gasmod/gas/storage"
)

// RemoveEmptyOutboxes removes the outboxes of buckets that no longer exist
// once they hold no file, so a deleted bucket's outbox does not stay
// behind once its events are delivered, and returns how many it removed.
// The outbox of a bucket that exists is kept, empty or not: its next event
// would only create it again. An outbox a write fills meanwhile is left,
// and so is one whose bucket's lock is held.
//
// A writer of an outbox must not find it removed between finding or
// creating the directory and writing into it (writeDurable stats or
// creates the directory, then creates the temp file in it, then renames
// it). Each writer is one of:
//
//   - an operation on the bucket, which records its events while it holds
//     the bucket's read lock, from its Pin until the release (see event):
//     recording an intent, confirming, marking committed or aborted,
//     settling, and the lifecycle's events;
//   - ConfirmCommitted, which holds committedMu and writes only into an
//     outbox holding the mark it confirms, removed after the write;
//   - RecoverEvents, before the store serves and before the deliverer
//     calling this runs.
//
// So an outbox is removed only while its bucket's write lock is held, taken
// without waiting (a held lock is tried again on the next call), and
// committedMu, and only once the bucket is found not to exist under that
// lock: CreateBucket, which takes the write lock too, cannot create the
// bucket again, and no operation can be pinned to it, until the removal is
// done. The removal itself is rmdir's, which fails on a directory that is
// not empty, so an outbox filled after it was listed as empty is kept.
func (s *Service) RemoveEmptyOutboxes(ctx context.Context) (int, error) {
	buckets, err := fs.ReadDir(s.root.FS(), eventsDir)
	if err != nil {
		return 0, fmt.Errorf("%s: read the event outboxes: %w", s.Name(), err)
	}
	n := 0
	var errs []error
	for _, b := range buckets {
		if !b.IsDir() || !bucketNameRe.MatchString(b.Name()) {
			continue
		}
		removed, rErr := s.removeEmptyOutbox(ctx, b.Name())
		if removed {
			n++
		}
		errs = append(errs, rErr)
	}
	if err = errors.Join(errs...); err != nil {
		return n, fmt.Errorf("%s: remove empty event outboxes: %w", s.Name(), err)
	}
	return n, nil
}

// stageOutboxPrune: removeEmptyOutbox holds the bucket's write lock and
// found the bucket gone and its outbox empty, and is about to remove it.
// An error the hook returns keeps the outbox.
const stageOutboxPrune = "outboxPrune"

// removeEmptyOutbox is RemoveEmptyOutboxes for bucket's outbox, and
// reports whether it removed it.
func (s *Service) removeEmptyOutbox(ctx context.Context, bucket string) (bool, error) {
	dir := filepath.Join(eventsDir, bucket)
	if empty, err := s.emptyDir(dir); err != nil || !empty {
		return false, err
	}
	unlock, ok := s.bucketLocks.tryLock(bucket, 0)
	if !ok {
		return false, nil
	}
	defer unlock()
	if _, err := s.HeadBucket(ctx, bucket); !errors.Is(err, storage.ErrBucketNotFound) {
		if err != nil {
			return false, fmt.Errorf("look up bucket %s: %w", bucket, err)
		}
		return false, nil
	}
	if err := s.at(stageOutboxPrune); err != nil {
		return false, nil //nolint:nilerr // the hook stands for a removal that did not happen
	}
	return s.removeOutboxDir(bucket, dir)
}

// removeOutboxDir removes dir, bucket's outbox, unless it is not empty,
// and reports whether it did. The caller holds the bucket's write lock.
func (s *Service) removeOutboxDir(bucket, dir string) (bool, error) {
	s.committedMu.Lock()
	err := s.root.Remove(dir)
	s.committedMu.Unlock()
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return err == nil, nil
	}
	// rmdir's error for a directory that is not empty differs between
	// systems, so the directory is read again rather than the error
	// matched.
	if empty, eErr := s.emptyDir(dir); eErr != nil || !empty {
		return false, eErr
	}
	return false, fmt.Errorf("remove the outbox of bucket %s: %w", bucket, err)
}

// emptyDir reports whether dir exists and holds no file.
func (s *Service) emptyDir(dir string) (bool, error) {
	entries, err := fs.ReadDir(s.root.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", dir, err)
	}
	return len(entries) == 0, nil
}
