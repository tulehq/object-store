package dirimpl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

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

// supportedKey reports whether key maps onto a file path one-to-one. Keys
// with a trailing slash or empty, "." or ".." segments would be stored under
// a different key than the one written.
func supportedKey(key string) bool {
	return !strings.HasSuffix(key, "/") && path.Clean(key) == key
}

// isNotDir reports whether err comes from using a file as a directory, which
// is how a path under an object key fails.
func isNotDir(err error) bool {
	return errors.Is(err, syscall.ENOTDIR)
}

// checkWritable returns contracts.ErrUnsupportedKey or contracts.ErrKeyConflict
// when key cannot be written to bucket. A directory left at key by deleted
// nested keys does not count as a conflict and is removed.
func (s *Service) checkWritable(bucket, key string) error {
	if !supportedKey(key) {
		return contracts.ErrUnsupportedKey
	}

	segments := strings.Split(key, "/")
	for i := 1; i < len(segments); i++ {
		info, err := s.root.Stat(filepath.Join(bucket, filepath.Join(segments[:i]...)))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: check key %q: %w", s.Name(), key, err)
		}
		if !info.IsDir() {
			return contracts.ErrKeyConflict
		}
	}

	name := filepath.Join(bucket, key)
	info, err := s.root.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: check key %q: %w", s.Name(), key, err)
	}
	if !info.IsDir() {
		return nil
	}

	return s.removeEmptyDir(name)
}

// removeEmptyDir removes the directory tree at name when it holds no files,
// and returns contracts.ErrKeyConflict when it does.
func (s *Service) removeEmptyDir(name string) error {
	err := fs.WalkDir(s.root.FS(), name, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return contracts.ErrKeyConflict
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, contracts.ErrKeyConflict) {
			return contracts.ErrKeyConflict
		}
		return fmt.Errorf("%s: check key %q: %w", s.Name(), name, err)
	}

	if err = s.root.RemoveAll(name); err != nil {
		return fmt.Errorf("%s: remove empty directory %q: %w", s.Name(), name, err)
	}
	return nil
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
