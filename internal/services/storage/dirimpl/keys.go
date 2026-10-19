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

// checkPrecondition returns contracts.ErrPreconditionFailed when cond does not
// hold for the object at bucket/key, and storage.ErrKeyNotFound when cond
// requires an object that does not exist.
func (s *Service) checkPrecondition(
	ctx context.Context,
	bucket, key string,
	cond contracts.Precondition,
) error {
	if cond == (contracts.Precondition{}) {
		return nil
	}

	info, err := s.StatObject(ctx, bucket, key)
	if err != nil && !errors.Is(err, storage.ErrKeyNotFound) {
		return err
	}
	exists := err == nil

	if cond.IfNoneMatch && exists {
		return contracts.ErrPreconditionFailed
	}
	if cond.IfMatch != "" {
		if !exists {
			return storage.ErrKeyNotFound
		}
		if cond.IfMatch != "*" && cond.IfMatch != info.ETag {
			return contracts.ErrPreconditionFailed
		}
	}
	return nil
}
