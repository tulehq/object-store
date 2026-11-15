package object

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/lifecycle"
	"tule.dev/object-store/internal/s3api"
)

// lifecycleConfig returns bucket's lifecycle configuration, nil when it has
// none stored for the generation caller evaluated it in: a configuration
// a server that did not know lifecycle configurations left from an earlier
// bucket of the name is none. ctx is pinned to bucket. The configuration
// is read and parsed on each call. One that cannot be read or decoded is
// logged at Warn and taken as none, as the sweep skips it: the headers it
// answers are advisory, and its damage fails none of the bucket's object
// operations. Only an error of caller's bucket or of the bucket itself
// is returned.
func (m *Module) lifecycleConfig(
	ctx context.Context,
	caller *authz.Caller,
	bucket string,
) (*lifecycle.Configuration, error) {
	//nolint:contextcheck // the bucket was read when it was pinned
	ref, err := caller.Ref(bucket)
	if err != nil {
		return nil, err //nolint:wrapcheck // an S3 error, answered as is
	}
	raw, err := m.storage.GetBucketConfig(ctx, bucket, contracts.ConfigLifecycle)
	switch {
	case errors.Is(err, contracts.ErrNoSuchBucketConfig):
		return nil, nil
	case errors.Is(err, storage.ErrBucketNotFound):
		return nil, err //nolint:wrapcheck // answered as NoSuchBucket
	case err != nil:
		m.skipLifecycleConfig(bucket, err)
		return nil, nil
	}
	stored, cfg, err := lifecycle.Decode(raw)
	if err != nil {
		m.skipLifecycleConfig(bucket, err)
		return nil, nil
	}
	if stored.Generation != ref.Generation {
		return nil, nil
	}
	return cfg, nil
}

// skipLifecycleConfig logs that bucket's lifecycle configuration, which
// failed with err, answers no lifecycle header.
func (m *Module) skipLifecycleConfig(bucket string, err error) {
	m.logger.Warn("left out the lifecycle headers of a lifecycle configuration "+
		"that could not be read").Str("bucket", bucket).Err("error", err).Send()
}

// setExpiration sets x-amz-expiration for info, the current version of its
// key, when an Expiration of cfg, a configuration lifecycleConfig returned,
// expires it. Only an Expiration by Days or Date expires a current version,
// so a rule with only NoncurrentVersionExpiration or
// ExpiredObjectDeleteMarker, or a Disabled rule, sets none.
func setExpiration(
	w http.ResponseWriter,
	cfg *lifecycle.Configuration,
	info *contracts.ObjectInfo,
) {
	if cfg == nil {
		return
	}
	at, ruleID, ok := cfg.Expiry(lifecycle.Object{
		LastModified: info.LastModified,
		Key:          info.Key,
		Tags:         info.Tags,
		Size:         info.Size,
	})
	if ok {
		w.Header().Set(s3api.HeaderExpiration, lifecycle.ExpirationHeader(at, ruleID))
	}
}

// setReadExpiration sets x-amz-expiration for info, a version GetObject or
// HeadObject read, as setExpiration does when it is its key's current
// version: Expiration "applies only to the current version" (S3 User
// Guide, "Expiring objects"), so a noncurrent version answers none.
func setReadExpiration(
	w http.ResponseWriter,
	cfg *lifecycle.Configuration,
	info *contracts.ObjectInfo,
) {
	if info.IsLatest {
		setExpiration(w, cfg, info)
	}
}

// setAbortDate sets x-amz-abort-date and x-amz-abort-rule-id for an upload
// of key initiated at initiated, when an AbortIncompleteMultipartUpload of
// cfg, a configuration lifecycleConfig returned, aborts it.
func setAbortDate(
	w http.ResponseWriter,
	cfg *lifecycle.Configuration,
	key string,
	initiated time.Time,
) {
	if cfg == nil {
		return
	}
	at, ruleID, ok := cfg.AbortDate(key, initiated)
	if ok {
		w.Header().Set(s3api.HeaderAbortDate, at.UTC().Format(http.TimeFormat))
		w.Header().Set(s3api.HeaderAbortRuleID, ruleID)
	}
}
