// Package dirimpl implements contracts.StorageProvider on a local directory,
// on top of the gas dir storage backend.
//
// The backend stores objects under hashes of their bucket names and keys and
// has no listing, so this package keeps what S3 needs on top of it under
// stateDir in the same root: a registry of bucket names, an index of the keys
// in each bucket, and the staging area of multipart uploads.
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

// Init creates the storage root if needed, opens it, and builds the backend
// on it.
func (s *Service) Init(context.Context) (err error) {
	if s.root, err = s.openRoot(); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}

	if s.dir, err = dir.NewProvider(dir.WithRoot(s.root))(s.config); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), errors.Join(err, s.root.Close()))
	}
	return nil
}

// Close closes the storage root. The backend holds nothing open between
// calls, so it has nothing to close.
func (s *Service) Close(context.Context) error {
	if err := s.root.Close(); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}
	return nil
}
