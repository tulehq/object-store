// Package dirimpl implements contracts.StorageProvider on a local directory,
// on top of the gas dir storage backend.
//
// This package keeps what S3 needs on top of the backend under stateDir in
// the same root: a registry of bucket names, an index of the keys in each
// bucket, and the staging area of multipart uploads. The backend stores each
// object under the hash of its key, with the object's metadata in a trailer
// after its bytes (see trailer.go).
package dirimpl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/storage/dir"

	"tule.dev/object-store/internal/contracts"
)

// Service stores buckets and objects under Settings.RootDir.
type Service struct {
	dir *dir.Provider

	config   gas.ConfigProvider
	logger   gas.Logger
	settings *Settings
	// clock stamps what is stored: an object's LastModified, a delete
	// marker's, and an upload's initiation.
	clock contracts.Clock
	root  *os.Root
	// hook, when set, is called at each stage of a change to an object's
	// versions and of a read of one (see versions.go), and an error it
	// returns fails the change or read there, as a crash would stop it.
	// Tests set it.
	hook func(stage string) error
	// events routes and renders the bucket events s records (see
	// events.go).
	events Events
	locks  keyLocks
	// bucketLocks order creating and deleting a bucket, which change its
	// generation, against the operations pinned to it and the reads of its
	// policy.
	bucketLocks bucketLocks
	// deleteWait bounds how long DeleteBucket waits for the operations
	// pinned to its bucket.
	deleteWait time.Duration
	// seq is the last commit sequence nextSeq returned.
	seq atomic.Int64
}

// defaultDeleteWait is deleteWait's value.
const defaultDeleteWait = 2 * time.Second

var (
	_ contracts.StorageProvider = (*Service)(nil)
	_ gas.Service               = (*Service)(nil)
	_ gas.ReadyReporter         = (*Service)(nil)
)

// NewService returns the constructor gas uses to build a Service from
// settings, which stamps what it stores with clock's time and records
// bucket events with events. Settings are bound from the configuration, so
// clock and events are arguments of their own. A nil clock panics here,
// when the service is registered.
func NewService(
	settings *Settings,
	clock contracts.Clock,
	events Events,
) func(config gas.ConfigProvider, logger gas.Logger) *Service {
	if clock == nil {
		panic("dirimpl: NewService needs a clock; pass contracts.SystemClock for the system's")
	}
	return func(config gas.ConfigProvider, logger gas.Logger) *Service {
		return &Service{
			settings: settings, clock: clock, config: config, logger: logger,
			deleteWait: defaultDeleteWait, events: events,
		}
	}
}

// now is the time s stamps what it stores with.
func (s *Service) now() time.Time {
	return s.clock.Now()
}

// Name identifies the service to gas.
func (s *Service) Name() string {
	return "tule/services/storage/dir"
}

// Init creates the storage root if needed, opens it, and builds and
// initializes the backend on it. The backend is given its configuration
// rather than binding the storage section, which holds this service's
// settings.
func (s *Service) Init(ctx context.Context) (err error) {
	if s.root, err = s.openRoot(); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}
	if err = s.removeOrphanUploads(); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), errors.Join(err, s.root.Close()))
	}

	s.dir, err = dir.New(
		dir.WithRoot(s.root),
		dir.WithConfig(dir.DefaultConfig()),
	)(
		s.config,
		s.logger,
	)
	if err == nil {
		err = s.dir.Init(ctx)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", s.Name(), errors.Join(err, s.root.Close()))
	}
	return nil
}

// CheckReady reports whether the bucket registry can be read, which fails
// once the service is closed or the storage root goes away.
func (s *Service) CheckReady(context.Context) error {
	if _, err := s.root.Stat(bucketsDir); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}
	return nil
}

// Close closes the backend, then the storage root, which the backend does
// not close.
func (s *Service) Close(ctx context.Context) error {
	if err := errors.Join(s.dir.Close(ctx), s.root.Close()); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}
	return nil
}
