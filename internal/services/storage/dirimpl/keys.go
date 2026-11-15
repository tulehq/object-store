package dirimpl

import (
	"context"
	"errors"
	"sync"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// keyLocks serializes writes to the same object, so a precondition checked
// before a write still holds when the write commits.
type keyLocks struct {
	m  map[string]*keyLock
	mu sync.Mutex
}

type keyLock struct {
	sync.Mutex
	refs int
}

// lock locks the object bucket/key and returns the function that unlocks it.
func (l *keyLocks) lock(bucket, key string) func() {
	name := bucket + "/" + key

	l.mu.Lock()
	if l.m == nil {
		l.m = make(map[string]*keyLock)
	}
	kl, ok := l.m[name]
	if !ok {
		kl = &keyLock{}
		l.m[name] = kl
	}
	kl.refs++
	l.mu.Unlock()

	kl.Lock()
	return func() {
		kl.Unlock()
		l.mu.Lock()
		kl.refs--
		if kl.refs == 0 {
			delete(l.m, name)
		}
		l.mu.Unlock()
	}
}

// tryLock locks the object bucket/key if no one holds its lock, and returns
// the function that unlocks it, or false when the lock is held. It never
// waits, so a caller that must not stall behind a slow write, which holds
// the lock while it reads its body, can come back to the key later.
func (l *keyLocks) tryLock(bucket, key string) (func(), bool) {
	name := bucket + "/" + key

	l.mu.Lock()
	if l.m == nil {
		l.m = make(map[string]*keyLock)
	}
	kl, ok := l.m[name]
	if !ok {
		kl = &keyLock{}
		l.m[name] = kl
	}
	// A lock held has a reference, so one just made is never held.
	if !kl.TryLock() {
		l.mu.Unlock()
		return nil, false
	}
	kl.refs++
	l.mu.Unlock()

	return func() {
		kl.Unlock()
		l.mu.Lock()
		kl.refs--
		if kl.refs == 0 {
			delete(l.m, name)
		}
		l.mu.Unlock()
	}, true
}

// checkPrecondition returns contracts.ErrPreconditionFailed when cond does not
// hold for the object at bucket/key, and storage.ErrObjectNotFound when cond
// requires an object that does not exist.
func (s *Service) checkPrecondition(
	ctx context.Context,
	bucket, key string,
	cond contracts.Precondition,
) error {
	if cond == (contracts.Precondition{}) {
		return nil
	}

	info, err := s.StatObject(ctx, contracts.ObjectRef{Bucket: bucket, Key: key})
	if err != nil && !errors.Is(err, storage.ErrObjectNotFound) {
		return err
	}
	etag := ""
	if err == nil {
		etag = info.ETag
	}
	return preconditionHolds(cond, err == nil, etag)
}

// preconditionHolds returns contracts.ErrPreconditionFailed when cond does
// not hold for the current object, which exists with etag, or does not,
// and storage.ErrObjectNotFound when cond requires an object that does not
// exist.
func preconditionHolds(cond contracts.Precondition, exists bool, etag string) error {
	if cond.IfNoneMatch && exists {
		return contracts.ErrPreconditionFailed
	}
	if cond.IfMatch != "" {
		if !exists {
			return storage.ErrObjectNotFound
		}
		if cond.IfMatch != "*" && cond.IfMatch != etag {
			return contracts.ErrPreconditionFailed
		}
	}
	return nil
}
