package dirimpl

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
)

// The key index lists the keys of each bucket, which the backend cannot. An
// entry is added before an object is written and removed after it is
// deleted or when its write fails, so the index lists every object. A crash
// between those steps can leave an entry for a key that holds no object, so
// readers check each key against the backend and skip those. Such an entry
// can still show up as a common prefix in a delimited listing.

// indexEntryRe matches index entry names. Anything else in an index directory
// is a temp file of an entry being written.
var indexEntryRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// indexEntry returns the index entry of key in bucket: the hex SHA-256 of the
// key, which is a valid file name whatever the key holds.
func indexEntry(bucket, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(indexDir, bucket, hex.EncodeToString(sum[:]))
}

// addToIndex records key in bucket's index. The entry is written to a temp
// file and renamed into place, so readers never see a partial key.
func (s *Service) addToIndex(bucket, key string) error {
	name := indexEntry(bucket, key)

	if _, err := s.root.Stat(name); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: index %q: %w", s.Name(), key, err)
	}

	tmp := name + ".tmp-" + rand.Text()
	if err := s.root.WriteFile(tmp, []byte(key), 0o600); err != nil {
		return errors.Join(
			fmt.Errorf("%s: index %q: %w", s.Name(), key, err), s.removeIfExists(tmp))
	}
	if err := s.root.Rename(tmp, name); err != nil {
		return errors.Join(
			fmt.Errorf("%s: index %q: %w", s.Name(), key, err), s.removeIfExists(tmp))
	}
	return nil
}

// removeFromIndex drops key from bucket's index.
func (s *Service) removeFromIndex(bucket, key string) error {
	if err := s.removeIfExists(indexEntry(bucket, key)); err != nil {
		return fmt.Errorf("%s: unindex %q: %w", s.Name(), key, err)
	}
	return nil
}

func (s *Service) removeIfExists(name string) error {
	if err := s.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err //nolint:wrapcheck // wrapped at the caller site
	}
	return nil
}

// indexedKeys returns the keys in bucket's index, sorted. Some may hold no
// object; see the index description above.
func (s *Service) indexedKeys(bucket string) ([]string, error) {
	d := filepath.Join(indexDir, bucket)
	entries, err := fs.ReadDir(s.root.FS(), d)
	if err != nil {
		return nil, fmt.Errorf("%s: read index of bucket %s: %w", s.Name(), bucket, err)
	}

	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		if !indexEntryRe.MatchString(e.Name()) {
			continue
		}
		b, rErr := s.root.ReadFile(filepath.Join(d, e.Name()))
		if errors.Is(rErr, fs.ErrNotExist) {
			// Removed by a delete while listing.
			continue
		}
		if rErr != nil {
			return nil, fmt.Errorf("%s: read index of bucket %s: %w", s.Name(), bucket, rErr)
		}
		keys = append(keys, string(b))
	}

	slices.Sort(keys)
	return keys, nil
}
