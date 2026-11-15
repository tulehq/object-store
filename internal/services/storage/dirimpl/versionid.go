package dirimpl

import (
	"crypto/rand"
	"encoding/binary"
	"time"

	"tule.dev/object-store/internal/contracts"
)

// A version ID is 26 characters of base32hex, upper case: the first 13
// encode the 64-bit commit sequence of the version, big-endian, and the
// last 13 64 random bits. Thirteen characters hold 65 bits, so the first
// of each half is at most "F". The ID is drawn when the version is
// committed, under its key's lock, so IDs sort as the commits did, and is
// never VersionNull: "Version IDs are Unicode, UTF-8 encoded, URL-ready,
// opaque strings that are no more than 1,024 bytes long" (S3 User Guide,
// "How S3 Versioning works").

// versionIDHalf is the length of each half of a version ID.
const versionIDHalf = 13

// base32hex is the alphabet version IDs are written in.
const base32hex = "0123456789ABCDEFGHIJKLMNOPQRSTUV"

// nextSeq returns a commit sequence greater than after and than every one
// it returned before: the time in nanoseconds, or one more than the
// greater of those when the clock has not moved past it.
func (s *Service) nextSeq(after int64) int64 {
	for {
		last := s.seq.Load()
		next := max(time.Now().UnixNano(), last+1, after+1)
		if s.seq.CompareAndSwap(last, next) {
			return next
		}
	}
}

// newVersionID returns a version ID for commit sequence seq.
func newVersionID(seq int64) string {
	var r [8]byte
	_, _ = rand.Read(r[:]) // crypto/rand.Read never fails
	b := make([]byte, 0, 2*versionIDHalf)
	b = appendBase32Hex(b, uint64(seq)) //nolint:gosec // a sequence is never negative
	b = appendBase32Hex(b, binary.BigEndian.Uint64(r[:]))
	return string(b)
}

// appendBase32Hex appends v to b as versionIDHalf base32hex digits.
func appendBase32Hex(b []byte, v uint64) []byte {
	for i := versionIDHalf - 1; i >= 0; i-- {
		b = append(b, base32hex[(v>>(5*i))&31])
	}
	return b
}

// parseBase32Hex returns the value of s, versionIDHalf base32hex digits,
// and false when s is not such a value of at most 64 bits.
func parseBase32Hex(s string) (uint64, bool) {
	if len(s) != versionIDHalf {
		return 0, false
	}
	var v uint64
	for i := range len(s) {
		c := s[i]
		var d uint64
		switch {
		case c >= '0' && c <= '9':
			d = uint64(c - '0')
		case c >= 'A' && c <= 'V':
			d = uint64(c-'A') + 10
		default:
			return 0, false
		}
		if i == 0 && d > 15 {
			return 0, false
		}
		v = v<<5 | d
	}
	return v, true
}

// versionSeq returns the commit sequence id encodes, and false when id is
// not a version ID this package issues. VersionNull is not one.
func versionSeq(id string) (int64, bool) {
	if len(id) != 2*versionIDHalf {
		return 0, false
	}
	seq, ok := parseBase32Hex(id[:versionIDHalf])
	if !ok || seq > 1<<63-1 {
		return 0, false
	}
	if _, ok = parseBase32Hex(id[versionIDHalf:]); !ok {
		return 0, false
	}
	return int64(seq), true
}

// checkVersionID returns contracts.ErrInvalidVersionID unless id is "",
// the current version, VersionNull, or a version ID this package issues.
func checkVersionID(id string) error {
	if id == "" || id == contracts.VersionNull {
		return nil
	}
	if _, ok := versionSeq(id); !ok {
		return contracts.ErrInvalidVersionID
	}
	return nil
}
