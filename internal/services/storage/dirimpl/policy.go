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

// GetBucketPolicy returns bucket's generation and its stored policy,
// reading both under the bucket's lock, or the bucket's pin when ctx is
// pinned to it: ErrNoSuchBucketPolicy with the generation when the bucket
// has no policy, storage.ErrBucketNotFound when there is no bucket.
func (s *Service) GetBucketPolicy(ctx context.Context, bucket string) (string, []byte, error) {
	if _, pinned := pinnedGenerations(ctx)[bucket]; !pinned {
		unlock := s.bucketLocks.rlock(bucket)
		defer unlock()
	}
	gen, err := s.generation(ctx, bucket)
	if err != nil {
		return "", nil, err
	}
	doc, err := s.root.ReadFile(policyFile(bucket))
	if errors.Is(err, fs.ErrNotExist) {
		return gen, nil, contracts.ErrNoSuchBucketPolicy
	}
	if err != nil {
		return "", nil, fmt.Errorf("%s: failed to read the policy of bucket %s: %w",
			s.Name(), bucket, err)
	}
	return gen, doc, nil
}

// PutBucketPolicy stores doc as bucket's policy. ctx must be pinned to
// bucket. It is written to a temp file and renamed into place, so a reader
// sees the old policy or the new one, never part of either.
func (s *Service) PutBucketPolicy(ctx context.Context, bucket string, doc []byte) error {
	if _, err := requirePin(ctx, bucket); err != nil {
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

// DeleteBucketPolicy removes bucket's policy, if it has one. ctx must be
// pinned to bucket.
func (s *Service) DeleteBucketPolicy(ctx context.Context, bucket string) error {
	if _, err := requirePin(ctx, bucket); err != nil {
		return err
	}
	return s.removePolicy(bucket)
}

// removePolicy removes bucket's policy file, if there is one. The caller
// holds the bucket's write lock or a pin.
func (s *Service) removePolicy(bucket string) error {
	if err := s.removeIfExists(policyFile(bucket)); err != nil {
		return fmt.Errorf("%s: failed to delete the policy of bucket %s: %w", s.Name(), bucket, err)
	}
	return nil
}
