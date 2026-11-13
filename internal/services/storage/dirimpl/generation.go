package dirimpl

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// A bucket's generation tells apart the buckets created under one name: a
// bucket deleted and created again gets a new one. It is a random token in
// generationFile in the bucket's registry entry, written with the entry.
const generationFile = "generation"

// legacyGeneration is the generation of a bucket whose registry entry has
// no generationFile, created before generations were recorded, and of an
// upload whose manifest names none.
const legacyGeneration = "legacy"

// bucketLocks are one reader/writer lock per bucket name. Creating and
// deleting a bucket, which change its generation, hold its write lock; a
// pin holds its read lock for as long as an operation acts on the bucket.
type bucketLocks struct {
	m  map[string]*bucketLock
	mu sync.Mutex
}

type bucketLock struct {
	sync.RWMutex
	refs int
}

// get returns bucket's lock, referenced; put releases the reference.
func (l *bucketLocks) get(bucket string) *bucketLock {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = make(map[string]*bucketLock)
	}
	bl, ok := l.m[bucket]
	if !ok {
		bl = &bucketLock{}
		l.m[bucket] = bl
	}
	bl.refs++
	return bl
}

func (l *bucketLocks) put(bucket string, bl *bucketLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	bl.refs--
	if bl.refs == 0 {
		delete(l.m, bucket)
	}
}

// lock write-locks bucket and returns the function that unlocks it.
func (l *bucketLocks) lock(bucket string) func() {
	bl := l.get(bucket)
	bl.Lock()
	return func() {
		bl.Unlock()
		l.put(bucket, bl)
	}
}

// rlock read-locks bucket and returns the function that unlocks it.
func (l *bucketLocks) rlock(bucket string) func() {
	bl := l.get(bucket)
	bl.RLock()
	return func() {
		bl.RUnlock()
		l.put(bucket, bl)
	}
}

// pinKey is the context key of the buckets a context is pinned to: a map
// of bucket name to the generation pinned.
type pinKey struct{}

func pinnedGenerations(ctx context.Context) map[string]string {
	pins, _ := ctx.Value(pinKey{}).(map[string]string)
	return pins
}

// Pin read-locks each bucket refs names, in name order, and checks that it
// still exists in the generation named, failing with
// storage.ErrBucketNotFound when one does not, as when it was deleted, or
// deleted and created again, since its generation was read. A ref with an
// empty generation, for a bucket that did not exist when it was read,
// never matches. On success it returns a context pinned to the buckets,
// which the bucket's operations require, and the function that releases
// them; the buckets cannot be deleted or created until it is called.
func (s *Service) Pin(
	ctx context.Context,
	refs ...contracts.BucketRef,
) (context.Context, func(), error) {
	pins := map[string]string{}
	for name, gen := range pinnedGenerations(ctx) {
		pins[name] = gen
	}
	sorted := slices.Clone(refs)
	slices.SortFunc(sorted, func(a, b contracts.BucketRef) int {
		return strings.Compare(a.Name, b.Name)
	})
	var unlocks []func()
	release := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	for _, ref := range sorted {
		if gen, ok := pins[ref.Name]; ok {
			if gen != ref.Generation {
				release()
				return nil, nil, storage.ErrBucketNotFound
			}
			continue
		}
		unlocks = append(unlocks, s.bucketLocks.rlock(ref.Name))
		gen, err := s.generation(ctx, ref.Name)
		if err == nil && (ref.Generation == "" || gen != ref.Generation) {
			err = storage.ErrBucketNotFound
		}
		if err != nil {
			release()
			return nil, nil, err
		}
		pins[ref.Name] = gen
	}
	return context.WithValue(ctx, pinKey{}, pins), release, nil
}

// requirePin returns the generation ctx pins bucket to, or an error when it
// pins none: an operation on a bucket must run under a Pin, so it acts on
// the bucket its caller looked up.
func requirePin(ctx context.Context, bucket string) (string, error) {
	gen, ok := pinnedGenerations(ctx)[bucket]
	if !ok {
		return "", fmt.Errorf("dirimpl: bucket %s is acted on without a pin", bucket)
	}
	return gen, nil
}

// generation returns bucket's generation, or storage.ErrBucketNotFound
// when there is no such bucket. The caller holds bucket's lock.
func (s *Service) generation(ctx context.Context, bucket string) (string, error) {
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return "", err
	}
	b, err := s.root.ReadFile(filepath.Join(bucketsDir, bucket, generationFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return legacyGeneration, nil
	case err != nil:
		return "", fmt.Errorf("%s: read the generation of bucket %s: %w", s.Name(), bucket, err)
	}
	return string(b), nil
}

// newGeneration returns a fresh generation token.
func newGeneration() string {
	return rand.Text()
}
