package dirimpl

import (
	"fmt"
	"os"
)

func (s *Service) openRoot() (*os.Root, error) {
	if err := os.MkdirAll(s.settings.RootDir, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create storage root: %w", err)
	}
	root, err := os.OpenRoot(s.settings.RootDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open storage root: %w", err)
	}
	return root, nil
}
