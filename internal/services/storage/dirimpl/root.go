package dirimpl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// stateDir holds what this package keeps next to the backend's own files. The
// backend names its directories by hex hashes and ".gas", so this cannot
// collide with them.
const stateDir = ".tule"

var (
	// bucketsDir holds one empty directory per bucket, named after it. A
	// bucket exists when its directory does, and the directory's
	// modification time is the bucket's creation date.
	bucketsDir = filepath.Join(stateDir, "buckets")

	// indexDir holds one directory per bucket, with one file per key that may
	// hold an object, named by keyHash and containing the key.
	indexDir = filepath.Join(stateDir, "index")

	// multipartDir is where in-progress multipart uploads are staged, one
	// directory per upload ID.
	multipartDir = filepath.Join(stateDir, "multipart")
)

func (s *Service) openRoot() (*os.Root, error) {
	if err := os.MkdirAll(s.settings.RootDir, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create storage root: %w", err)
	}
	root, err := os.OpenRoot(s.settings.RootDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open storage root: %w", err)
	}
	for _, d := range []string{bucketsDir, indexDir, multipartDir} {
		if err = root.MkdirAll(d, 0o750); err != nil {
			return nil, errors.Join(
				fmt.Errorf("failed to create %s in storage root: %w", d, err), root.Close())
		}
	}
	return root, nil
}
