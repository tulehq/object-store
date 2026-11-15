package lifecycle

import (
	"errors"
	"fmt"
	"time"

	"tule.dev/object-store/internal/contracts"
)

// Version, Key and Plan are the storage contract's types, so that
// storage hands a key's versions to a Decider and applies its Plan.
type (
	// Version is a version or delete marker as lifecycle rules see it.
	Version = contracts.LifecycleVersion
	// Key is a key's versions, newest first, read under its lock.
	Key = contracts.LifecycleKey
	// Plan is what lifecycle rules do to a key now.
	Plan = contracts.LifecyclePlan
)

var _ contracts.LifecycleDecider = (*Decider)(nil)

// Decider evaluates a configuration's rules against the versions of one
// key at one time.
type Decider struct {
	c   *Configuration
	now time.Time
}

// Decider returns the decider of c's rules at now.
func (c *Configuration) Decider(now time.Time) *Decider {
	return &Decider{c: c, now: now}
}

var errNoNoncurrentTime = errors.New("lifecycle: a noncurrent version has no noncurrent time")

// Plan returns what the rules do to k now, on its versions as read; the
// removal of a delete marker left alone is asked after it is applied
// (RemoveSoleMarker).
//
// Noncurrent versions (index 1 and on) are deleted permanently by
// NoncurrentVersionExpiration, in a versioned bucket only ("has no
// effect" in an Unversioned one, S3 User Guide table "Lifecycle actions
// and bucket versioning state"). The current version, when it is not a
// delete marker, is expired by Expiration Days or Date: storage deletes
// it in an Unversioned bucket, and covers it with a delete marker
// otherwise, dated the UTC midnight of now ("When S3 Lifecycle creates a
// delete marker … the timestamp is set to midnight UTC on the day the
// action occurred", S3 User Guide) but never before the version it
// covers (L4). A current delete marker is left alone: "Amazon S3 doesn't
// take any action if there are one or more object versions and the
// delete marker is the current version".
//
// Noncurrent expiry is decided on the versions as read, before this
// plan's Expiration, so a version made noncurrent now is not also
// expired now. Of several rules that apply to one version, the earliest
// is the one named; ties go to the rule first in the document (L28).
//
// Storage must apply a plan in this order: first delete every version in
// Remove, then expire the current version (ExpireCurrentRule), then ask
// RemoveSoleMarker. Only that order is safe: in a Suspended bucket,
// [v2, a noncurrent null] can plan {ExpireCurrentRule, Remove{"null"}},
// and expiring v2 places a null marker that replaces the null version, so
// a Remove applied after it would name the new marker rather than the
// version it was decided on.
func (d *Decider) Plan(k *Key) (Plan, error) {
	var plan Plan
	if len(k.Versions) == 0 {
		return plan, nil
	}
	tags := make([]lazyTags, len(k.Versions))
	for i := range k.Versions {
		tags[i] = lazyTags{v: &k.Versions[i], key: k.Key}
	}
	if k.Versioning != contracts.Unversioned {
		removals, err := d.removals(k, tags)
		if err != nil {
			return Plan{}, err
		}
		plan.Remove = removals
	}
	ruleID, err := d.currentRule(k, &tags[0])
	if err != nil || ruleID == "" {
		return plan, err
	}
	plan.ExpireCurrentRule = ruleID
	if k.Versioning != contracts.Unversioned {
		plan.MarkerTime = Midnight(d.now)
		if lm := k.Versions[0].LastModified; plan.MarkerTime.Before(lm) {
			plan.MarkerTime = lm
		}
	}
	return plan, nil
}

// removals maps the noncurrent versions of k that are deleted now to the
// rule that deletes them, nil for none.
func (d *Decider) removals(k *Key, tags []lazyTags) (map[string]string, error) {
	var remove map[string]string
	for i := 1; i < len(k.Versions); i++ {
		ruleID, err := d.noncurrentRule(k, i, &tags[i])
		if err != nil {
			return nil, err
		}
		if ruleID == "" {
			continue
		}
		if remove == nil {
			remove = map[string]string{}
		}
		remove[k.Versions[i].VersionID] = ruleID
	}
	return remove, nil
}

// currentRule is the rule whose Expiration expires the current version of
// k now, "" for none or when it is a delete marker.
func (d *Decider) currentRule(k *Key, tags *lazyTags) (string, error) {
	cur := &k.Versions[0]
	if cur.Marker {
		return "", nil
	}
	var err error
	at, ruleID, ok := d.c.earliest(func(r *Rule) (time.Time, bool) {
		t, has := r.expiresAt(cur.LastModified)
		if !has || err != nil {
			return time.Time{}, false
		}
		p := r.predicate()
		var admitted bool
		admitted, err = p.admitsVersion(k.Key, cur.Size, tags)
		return t, admitted
	})
	if err != nil || !ok || d.now.Before(at) {
		return "", err
	}
	return ruleID, nil
}

// noncurrentRule is the rule whose NoncurrentVersionExpiration deletes
// the version at index i of k now, "" for none. The version is eligible
// NoncurrentDays after it became noncurrent, rounded as EligibleAt
// rounds, and, with NewerNoncurrentVersions N, only when at least N
// noncurrent versions are newer than it: the N newest noncurrent
// versions are retained, so newer >= N deletes ("Specifies how many
// noncurrent versions Amazon S3 will retain ... Amazon S3 will
// permanently delete any additional noncurrent versions beyond the
// specified number to retain", NoncurrentVersionExpiration API reference;
// "how many newer noncurrent versions must exist", S3 User Guide,
// "Lifecycle configuration elements"; L6). The User Guide's "Examples of
// S3 Lifecycle configurations" says otherwise ("more than 5 newer
// noncurrent versions must exist before Amazon S3 can expire a given
// version", which would retain N+1); the API reference's reading is the
// one taken. That noncurrent delete markers count toward N, as they are
// noncurrent versions S3 expires ("including noncurrent delete marker
// objects", same page), is a choice (L7).
func (d *Decider) noncurrentRule(k *Key, i int, tags *lazyTags) (string, error) {
	v := &k.Versions[i]
	if v.NoncurrentSince.IsZero() {
		return "", fmt.Errorf("%w: key %q, version %q", errNoNoncurrentTime, k.Key, v.VersionID)
	}
	newer := i - 1
	var err error
	_, ruleID, _ := d.c.earliest(func(r *Rule) (time.Time, bool) {
		e := r.NoncurrentVersionExpiration
		if err != nil || e == nil || e.NoncurrentDays == nil ||
			e.NewerNoncurrentVersions != nil && newer < *e.NewerNoncurrentVersions {
			return time.Time{}, false
		}
		at := EligibleAt(v.NoncurrentSince, *e.NoncurrentDays)
		if d.now.Before(at) {
			return time.Time{}, false
		}
		p := r.predicate()
		var admitted bool
		admitted, err = p.admitsVersion(k.Key, v.Size, tags)
		return at, admitted
	})
	if err != nil {
		return "", err
	}
	return ruleID, nil
}

// RemoveSoleMarker returns the rule that removes marker, the only version
// of key left, now, "" for none: a rule that admits it with
// ExpiredObjectDeleteMarker true removes it at once ("To clean up delete
// markers as soon as they become the only version, create a separate
// rule with only the ExpiredObjectDeleteMarker tag", S3 User Guide), and
// one with Expiration Days once it is that old, or Date once that date is
// reached ("When you specify the Days tag, Amazon S3 automatically
// performs ExpiredObjectDeleteMarker cleanup when the delete markers are
// old enough"; L26). That a Date reached removes it as Days does is a
// choice: the User Guide names only Days. A marker has size 0 and no
// tags, so a rule with tags or ObjectSizeGreaterThan never admits it.
func (d *Decider) RemoveSoleMarker(key string, marker Version) (string, error) {
	if !marker.Marker {
		return "", fmt.Errorf("lifecycle: version %q of key %q is not a delete marker",
			marker.VersionID, key)
	}
	_, ruleID, _ := d.c.earliest(func(r *Rule) (time.Time, bool) {
		at, due := d.markerDue(r, marker)
		p := r.predicate()
		return at, due && p.admitsKey(key, 0) && p.admitsTags(nil)
	})
	return ruleID, nil
}

// markerDue is when r's Expiration removes a sole delete marker, and
// whether that is now: at once for ExpiredObjectDeleteMarker true, and by
// Days or Date otherwise.
func (d *Decider) markerDue(r *Rule, marker Version) (time.Time, bool) {
	e := r.Expiration
	switch {
	case e == nil:
		return time.Time{}, false
	case e.ExpiredObjectDeleteMarker != nil:
		return time.Time{}, *e.ExpiredObjectDeleteMarker
	}
	at, has := r.expiresAt(marker.LastModified)
	return at, has && !d.now.Before(at)
}

// admitsVersion reports whether p admits a version of key of size, whose
// tags l reads, only when p names tags.
func (p *predicate) admitsVersion(key string, size int64, l *lazyTags) (bool, error) {
	if !p.admitsKey(key, size) {
		return false, nil
	}
	if len(p.tags) == 0 {
		return true, nil
	}
	tags, err := l.get()
	if err != nil {
		return false, err
	}
	return p.admitsTags(tags), nil
}

// lazyTags reads a version's tags on first use, once: only rules with
// tags need them. A delete marker has none.
type lazyTags struct {
	err  error
	v    *Version
	key  string
	tags []contracts.Tag
	done bool
}

func (l *lazyTags) get() ([]contracts.Tag, error) {
	if l.done {
		return l.tags, l.err
	}
	l.done = true
	if l.v.Marker || l.v.Tags == nil {
		return nil, nil
	}
	l.tags, l.err = l.v.Tags()
	if l.err != nil {
		l.err = fmt.Errorf("lifecycle: read the tags of key %q, version %q: %w",
			l.key, l.v.VersionID, l.err)
	}
	return l.tags, l.err
}
