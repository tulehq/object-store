package contracts

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// HostIDBytes is how many random bytes a host ID encodes: 48, which base64
// writes as 64 characters, the length of the x-amz-id-2 values S3 answers.
const HostIDBytes = 48

// NewRequestID returns a new request ID, the value of x-amz-request-id: 8
// random bytes as 16 uppercase hex characters.
func NewRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // never fails: crypto/rand.Read crashes the program instead
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

// NewHostID returns a new host ID, the value of x-amz-id-2: base64 of
// HostIDBytes random bytes.
func NewHostID() string {
	b := make([]byte, HostIDBytes)
	_, _ = rand.Read(b) // never fails, as above
	return base64.StdEncoding.EncodeToString(b)
}
