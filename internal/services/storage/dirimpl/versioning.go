package dirimpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"tule.dev/object-store/internal/contracts"
)

// mfaDeleteDisabled is the one MFADelete value a bucket's versioning
// configuration stores other than "".
const mfaDeleteDisabled = "Disabled"

// versioningState is what is stored in a bucket's versioning file.
type versioningState struct {
	Status    contracts.Versioning `json:"status"`
	MFADelete string               `json:"mfaDelete,omitempty"`
}

// versioningFile is the file bucket's versioning configuration is stored
// in, a side file as the bucket's policy is (see policyFile). A bucket
// without one is Unversioned.
func versioningFile(bucket string) string {
	return filepath.Join(versioningDir, bucket)
}

// GetBucketVersioning returns bucket's versioning configuration, read under
// the bucket's lock, or its pin when ctx is pinned to it: the zero value
// when it was never set, storage.ErrBucketNotFound when there is no bucket.
func (s *Service) GetBucketVersioning(
	ctx context.Context,
	bucket string,
) (*contracts.VersioningConfig, error) {
	if _, pinned := pinnedGenerations(ctx)[bucket]; !pinned {
		unlock := s.bucketLocks.rlock(bucket)
		defer unlock()
	}
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return nil, err
	}
	st, err := s.versioning(bucket)
	if err != nil {
		return nil, err
	}
	return &contracts.VersioningConfig{Status: st.Status, MFADelete: st.MFADelete}, nil
}

// PutBucketVersioning stores cfg as bucket's versioning configuration,
// replacing the one it had: contracts.ErrUnversionedState for a Status of
// Unversioned, which a bucket never returns to, an error wrapping
// contracts.ErrInvalidVersioningStatus for a Status that is none, and one
// wrapping contracts.ErrMFADeleteUnsupported for an MFADelete other than
// "" and "Disabled". It is written to a temp
// file and renamed into place, so the last writer wins; it takes no key
// lock, since every write reads the state under its key's lock. ctx must
// be pinned to bucket.
func (s *Service) PutBucketVersioning(
	ctx context.Context,
	bucket string,
	cfg contracts.VersioningConfig,
) error {
	if _, err := requirePin(ctx, bucket); err != nil {
		return err
	}
	if err := s.checkVersioning(cfg); err != nil {
		return err
	}
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return err
	}
	// The directory of the bucket's manifests is made with its state, so
	// the first manifests do not race to create it (see writeDurable).
	if err := s.makeDurableDir(filepath.Join(versionsDir, bucket)); err != nil {
		return fmt.Errorf("%s: failed to create the versions of bucket %s: %w",
			s.Name(), bucket, err)
	}
	raw, err := json.Marshal(versioningState{Status: cfg.Status, MFADelete: cfg.MFADelete})
	if err != nil {
		return fmt.Errorf("%s: failed to encode the versioning of bucket %s: %w",
			s.Name(), bucket, err)
	}
	if err = s.writeDurable(versioningFile(bucket), raw); err != nil {
		return fmt.Errorf("%s: failed to store the versioning of bucket %s: %w",
			s.Name(), bucket, err)
	}
	return nil
}

// versioning reads bucket's versioning state, the zero value, Unversioned,
// when it has none.
func (s *Service) versioning(bucket string) (versioningState, error) {
	raw, err := s.root.ReadFile(versioningFile(bucket))
	if errors.Is(err, fs.ErrNotExist) {
		return versioningState{}, nil
	}
	if err != nil {
		return versioningState{}, fmt.Errorf("%s: failed to read the versioning of bucket %s: %w",
			s.Name(), bucket, err)
	}
	var st versioningState
	if err = json.Unmarshal(raw, &st); err != nil {
		return versioningState{}, fmt.Errorf(
			"%s: failed to decode the versioning of bucket %s: %w", s.Name(), bucket, err)
	}
	return st, nil
}

// checkVersioning returns PutBucketVersioning's error for cfg, a
// configuration it does not store, and nil for one it does.
func (s *Service) checkVersioning(cfg contracts.VersioningConfig) error {
	switch cfg.Status {
	case contracts.VersioningEnabled, contracts.VersioningSuspended:
	case contracts.Unversioned:
		return contracts.ErrUnversionedState
	default:
		return fmt.Errorf("%s: %q: %w", s.Name(), cfg.Status, contracts.ErrInvalidVersioningStatus)
	}
	if cfg.MFADelete != "" && cfg.MFADelete != mfaDeleteDisabled {
		return fmt.Errorf("%s: MFA delete %q: %w", s.Name(), cfg.MFADelete,
			contracts.ErrMFADeleteUnsupported)
	}
	return nil
}
