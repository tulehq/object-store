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
	root     *os.Root
	locks    keyLocks
}

var (
	_ contracts.StorageProvider = (*Service)(nil)
	_ gas.Service               = (*Service)(nil)
	_ gas.ReadyReporter         = (*Service)(nil)
)

// NewService returns the constructor gas uses to build a Service from
// settings.
func NewService(settings *Settings) func(config gas.ConfigProvider, logger gas.Logger) *Service {
	return func(config gas.ConfigProvider, logger gas.Logger) *Service {
		return &Service{settings: settings, config: config, logger: logger}
	}
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
