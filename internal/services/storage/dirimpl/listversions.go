package dirimpl

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"tule.dev/object-store/internal/contracts"
)

// errVersionMarkerWithoutKey is a ListObjectVersions with a
// VersionIDMarker and no KeyMarker: "A version-id marker cannot be
// specified without a key marker."
var errVersionMarkerWithoutKey = errors.New("storage: a version-id marker needs a key marker")

// ListObjectVersions lists one page of the versions and delete markers of
// bucket's keys: keys in order, each with its versions and markers
// interleaved newest first, as its manifest lists them, the first of them
// the latest. A key without a manifest lists its object as its null
// version. Listing starts after in.KeyMarker, or with in.VersionIDMarker,
// after that version of it; one that is gone resumes at the first version
// of the key committed before it, and a gone null version at the key's
// first, so a page is repeated rather than skipped. Every call reads the
// bucket's whole key index, as ListObjects does, but only the manifests of
// the keys it lists, and opens only the objects of keys without one. ctx
// must be pinned to bucket.
func (s *Service) ListObjectVersions(
	ctx context.Context,
	bucket string,
	in contracts.ListObjectVersionsInput,
) (*contracts.ListObjectVersionsOutput, error) {
	if err := checkVersionListing(in); err != nil {
		return nil, err
	}
	keys, err := s.bucketKeys(ctx, bucket)
	if err != nil {
		return nil, err
	}
	start, _ := slices.BinarySearch(keys, max(in.KeyMarker, in.Prefix))

	p := &versionPage{in: in}
	for _, key := range keys[start:] {
		if !strings.HasPrefix(key, in.Prefix) || p.out.IsTruncated {
			break
		}
		cp := commonPrefix(key, in.Prefix, in.Delimiter)
		if p.skips(key, cp) {
			continue
		}
		versions, vErr := s.keyVersions(ctx, bucket, key)
		if vErr != nil {
			return nil, vErr
		}
		p.add(key, cp, versions)
	}

	if !p.out.IsTruncated {
		p.out.NextKeyMarker, p.out.NextVersionIDMarker = "", ""
	}
	return &p.out, nil
}

// checkVersionListing returns errVersionMarkerWithoutKey for in with a
// VersionIDMarker and no KeyMarker, and contracts.ErrInvalidVersionID for
// a VersionIDMarker this package does not issue.
func checkVersionListing(in contracts.ListObjectVersionsInput) error {
	if in.VersionIDMarker != "" && in.KeyMarker == "" {
		return errVersionMarkerWithoutKey
	}
	return checkVersionID(in.VersionIDMarker)
}

// versionPage is a page of ListObjectVersions being filled.
type versionPage struct {
	lastPrefix string
	in         contracts.ListObjectVersionsInput
	out        contracts.ListObjectVersionsOutput
	count      int
}

// room reports whether the page has room for another entry, and marks it
// truncated when it has none.
func (p *versionPage) room() bool {
	if p.count == p.in.MaxKeys {
		p.out.IsTruncated = true
		return false
	}
	p.count++
	return true
}

// skips reports whether key, which rolls up into common prefix cp, or ""
// for none, is passed over without reading its versions: under a prefix
// already added or not after the key marker, or the key marker itself
// when no version marker resumes within it.
func (p *versionPage) skips(key, cp string) bool {
	if cp != "" {
		return cp == p.lastPrefix || cp <= p.in.KeyMarker
	}
	return key == p.in.KeyMarker && p.in.VersionIDMarker == ""
}

// add adds versions, those of key, as common prefix cp, or as versions
// when cp is "", from the version marker on for the key marker.
func (p *versionPage) add(key, cp string, versions []versionEntry) {
	if cp != "" {
		p.addPrefix(cp, versions)
		return
	}
	from := 0
	if key == p.in.KeyMarker {
		from = resumeAt(versions, p.in.VersionIDMarker)
	}
	p.addVersions(key, versions[from:], from)
}

// addPrefix adds common prefix cp, which a key with versions rolls up
// into; a key without any, as an indexed key left by a crash, adds none.
func (p *versionPage) addPrefix(cp string, versions []versionEntry) {
	if len(versions) == 0 || !p.room() {
		return
	}
	p.out.CommonPrefixes = append(p.out.CommonPrefixes, cp)
	p.out.NextKeyMarker, p.out.NextVersionIDMarker = cp, ""
	p.lastPrefix = cp
}

// addVersions adds versions of key, the ones from index from of its
// versions, as long as there is room.
func (p *versionPage) addVersions(key string, versions []versionEntry, from int) {
	for i := range versions {
		if !p.room() {
			return
		}
		e := &versions[i]
		p.out.Versions = append(p.out.Versions, contracts.ObjectVersion{
			Key: key, VersionID: e.ID, ETag: e.ETag, Size: e.Size,
			LastModified: e.LastModified, IsLatest: from+i == 0, DeleteMarker: e.Marker,
		})
		p.out.NextKeyMarker, p.out.NextVersionIDMarker = key, e.ID
	}
}

// keyVersions returns the versions of key, newest first: its manifest's,
// or for a key without one, its object as its null version, if it has
// one.
func (s *Service) keyVersions(ctx context.Context, bucket, key string) ([]versionEntry, error) {
	m, _, err := s.readVersions(bucket, key)
	if err != nil {
		return nil, err
	}
	if m != nil {
		return m.Versions, nil
	}
	e, err := s.legacyEntry(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	if e != nil {
		return []versionEntry{*e}, nil
	}
	// The key's first versioned change may have removed its object since
	// the manifest was read.
	if m, _, err = s.readVersions(bucket, key); err != nil {
		return nil, fmt.Errorf("%s: list versions of %q: %w", s.Name(), key, err)
	}
	if m != nil {
		return m.Versions, nil
	}
	return nil, nil
}

// resumeAt returns the index in versions to resume a listing at whose
// last entry was version id of their key: the one after it, or when it is
// gone, the first committed before it, which for VersionNull, whose
// sequence is not in its ID, is the first.
func resumeAt(versions []versionEntry, id string) int {
	if i := slices.IndexFunc(versions, func(e versionEntry) bool { return e.ID == id }); i >= 0 {
		return i + 1
	}
	seq, ok := versionSeq(id)
	if !ok {
		return 0
	}
	if i := slices.IndexFunc(versions, func(e versionEntry) bool { return e.Seq < seq }); i >= 0 {
		return i
	}
	return len(versions)
}
