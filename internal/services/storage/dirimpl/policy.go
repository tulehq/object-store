package dirimpl

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"tule.dev/object-store/internal/contracts"
)

// policyFile is the file bucket's policy is stored in. Bucket names hold
// only lowercase letters, digits, dots and hyphens, and HeadBucket refuses
// any other, so a name is a valid file name of its own.
func policyFile(bucket string) string {
	return filepath.Join(policiesDir, bucket)
}

// GetBucketPolicy returns bucket's stored policy, reading it under
// bucketsMu with the bucket's existence.
func (s *Service) GetBucketPolicy(ctx context.Context, bucket string) ([]byte, error) {
	s.bucketsMu.RLock()
	defer s.bucketsMu.RUnlock()

	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return nil, err
	}
	doc, err := s.root.ReadFile(policyFile(bucket))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, contracts.ErrNoSuchBucketPolicy
	}
	if err != nil {
		return nil, fmt.Errorf(
			"%s: failed to read the policy of bucket %s: %w",
			s.Name(),
			bucket,
			err,
		)
	}
	return doc, nil
}

// PutBucketPolicy stores doc as bucket's policy. It is written to a temp
// file and renamed into place, so a reader sees the old policy or the new
// one, never part of either.
func (s *Service) PutBucketPolicy(ctx context.Context, bucket string, doc []byte) error {
	s.bucketsMu.Lock()
	defer s.bucketsMu.Unlock()

	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return err
	}
	name := policyFile(bucket)
	tmp := name + ".tmp-" + rand.Text()
	if err := s.root.WriteFile(tmp, doc, 0o600); err != nil {
		return errors.Join(fmt.Errorf("%s: failed to store the policy of bucket %s: %w",
			s.Name(), bucket, err), s.removeIfExists(tmp))
	}
	if err := s.root.Rename(tmp, name); err != nil {
		return errors.Join(fmt.Errorf("%s: failed to store the policy of bucket %s: %w",
			s.Name(), bucket, err), s.removeIfExists(tmp))
	}
	return nil
}

// DeleteBucketPolicy removes bucket's policy, if it has one.
func (s *Service) DeleteBucketPolicy(ctx context.Context, bucket string) error {
	s.bucketsMu.Lock()
	defer s.bucketsMu.Unlock()

	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return err
	}
	return s.removePolicy(bucket)
}

// removePolicy removes bucket's policy file, if there is one. The caller
// holds bucketsMu.
func (s *Service) removePolicy(bucket string) error {
	if err := s.removeIfExists(policyFile(bucket)); err != nil {
		return fmt.Errorf("%s: failed to delete the policy of bucket %s: %w", s.Name(), bucket, err)
	}
	return nil
}
