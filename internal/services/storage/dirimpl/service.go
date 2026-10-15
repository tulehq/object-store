// Package dirimpl implements contracts.StorageProvider on a local directory,
// one subdirectory per bucket, on top of the gas dir storage backend.
package dirimpl

import (
	"fmt"
	"os"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/storage/dir"

	"tule.dev/object-store/internal/contracts"
)

// Service stores buckets and objects under Settings.RootDir.
type Service struct {
	dir *dir.Service

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
func NewService(settings *Settings) func(logger gas.Logger) *Service {
	return func(logger gas.Logger) *Service {
		return &Service{settings: settings, logger: logger}
	}
}

// Name identifies the service to gas.
func (s *Service) Name() string {
	return "tule/services/storage/dir"
}

// Init creates the storage root if needed and opens it.
func (s *Service) Init() (err error) {
	if s.root, err = s.openRoot(); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}

	s.dir = dir.New(dir.WithRoot(s.root))(nil, s.logger)
	if err = s.dir.Init(); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}
	return
}

// Close closes the backend and the storage root.
func (s *Service) Close() error {
	if err := s.dir.Close(); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}
	if err := s.root.Close(); err != nil {
		return fmt.Errorf("%s: %w", s.Name(), err)
	}
	return nil
}
