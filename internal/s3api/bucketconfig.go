package s3api

import (
	"context"
	"crypto/md5" //nolint:gosec // S3's integrity check of a customer key is MD5
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"tule.dev/object-store/internal/contracts"
)

// ConfigReader reads a bucket's stored configuration:
// contracts.StorageProvider's GetBucketConfig.
type ConfigReader interface {
	GetBucketConfig(ctx context.Context, bucket string, kind contracts.BucketConfig) ([]byte, error)
}

// Ownership returns bucket's Object Ownership setting. A bucket without
// one, created before new buckets got BucketOwnerEnforced or whose
// controls were deleted, behaves as ObjectWriter, the setting S3 applied
// before Object Ownership existed ("Controlling ownership of objects and
// disabling ACLs for your bucket", S3 User Guide).
func Ownership(ctx context.Context, cr ConfigReader, bucket string) (string, error) {
	doc, err := cr.GetBucketConfig(ctx, bucket, contracts.ConfigOwnership)
	if errors.Is(err, contracts.ErrNoSuchBucketConfig) {
		return OwnershipObjectWriter, nil
	}
	if err != nil {
		return "", err //nolint:wrapcheck // the storage's own error, answered as is
	}
	ownership, err := ParseOwnershipControls(doc)
	if err != nil {
		return "", fmt.Errorf("s3api: the stored ownership controls of bucket %s do not parse: %w",
			bucket, err)
	}
	return ownership, nil
}

// PublicAccessBlock returns bucket's Block Public Access settings, nil
// when it has none.
func PublicAccessBlock(
	ctx context.Context,
	cr ConfigReader,
	bucket string,
) (*PublicAccessBlockConfiguration, error) {
	doc, err := cr.GetBucketConfig(ctx, bucket, contracts.ConfigPublicAccessBlock)
	if errors.Is(err, contracts.ErrNoSuchBucketConfig) {
		return nil, nil
	}
	if err != nil {
		return nil, err //nolint:wrapcheck // the storage's own error, answered as is
	}
	block, err := ParsePublicAccessBlock(doc)
	if err != nil {
		return nil, fmt.Errorf("s3api: the stored public access block of bucket %s does not "+
			"parse: %w", bucket, err)
	}
	return block, nil
}

// Encryption returns bucket's default encryption, SSE-S3 when it was never
// set (see NewDefaultEncryption).
func Encryption(
	ctx context.Context,
	cr ConfigReader,
	bucket string,
) (*ServerSideEncryptionConfiguration, error) {
	doc, err := cr.GetBucketConfig(ctx, bucket, contracts.ConfigEncryption)
	if errors.Is(err, contracts.ErrNoSuchBucketConfig) {
		return NewDefaultEncryption(), nil
	}
	if err != nil {
		return nil, err //nolint:wrapcheck // the storage's own error, answered as is
	}
	c, err := ParseEncryption(doc)
	if err != nil {
		return nil, fmt.Errorf("s3api: the stored encryption of bucket %s does not parse: %w",
			bucket, err)
	}
	return c, nil
}

// CheckACL refuses grants, an ACL a request sets on a bucket or object of
// a bucket with Object Ownership ownership and Block Public Access block
// (nil for none), as S3 refuses it:
//
//   - with ACLs disabled (BucketOwnerEnforced), any ACL but FULL_CONTROL
//     for the bucket owner, bucketOwnerID, is
//     ErrAccessControlListNotSupported;
//   - otherwise, under BlockPublicAcls, a public ACL: "PutBucketAcl and
//     PutObjectAcl calls fail if the specified access control list (ACL)
//     is public. PutObject calls fail if the request includes a public
//     ACL." ("Blocking public access to your Amazon S3 storage", S3 User
//     Guide). S3 documents no error for it; 403 AccessDenied is a choice.
func CheckACL(
	ownership string,
	block *PublicAccessBlockConfiguration,
	grants []Grant,
	bucketOwnerID string,
) error {
	if ownership == OwnershipBucketOwnerEnforced {
		if !OnlyOwnerFullControl(grants, bucketOwnerID) {
			return ErrAccessControlListNotSupported()
		}
		return nil
	}
	if block != nil && block.BlockPublicAcls && PublicGrants(grants) {
		return NewError(http.StatusForbidden, "AccessDenied", "Access Denied")
	}
	return nil
}

// The encryption headers of a write and of its response.
const (
	HeaderSSE          = "X-Amz-Server-Side-Encryption"
	HeaderSSEKMSKeyID  = "X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"
	HeaderSSEBucketKey = "X-Amz-Server-Side-Encryption-Bucket-Key-Enabled"

	HeaderSSECAlgorithm = "X-Amz-Server-Side-Encryption-Customer-Algorithm"
	HeaderSSECKey       = "X-Amz-Server-Side-Encryption-Customer-Key"
	HeaderSSECKeyMD5    = "X-Amz-Server-Side-Encryption-Customer-Key-Md5"
)

// EncryptionHeaderNames are the encryption headers an object is stored
// with, in either form.
var EncryptionHeaderNames = []string{
	HeaderSSE, HeaderSSEKMSKeyID, HeaderSSEBucketKey, HeaderSSECAlgorithm, HeaderSSECKeyMD5,
}

// EncryptionHeaders returns the encryption headers an object written by r
// is stored and answered with: r's x-amz-server-side-encryption, its KMS
// key ID and its bucket key setting, or, for what r leaves out, the
// bucket's default, def. Nothing is encrypted with KMS; the headers are
// recorded and reported as S3 reports them.
//
// An algorithm other than AES256, aws:kms or aws:kms:dsse, a KMS key ID
// without a KMS algorithm, and a bucket key setting that is not a boolean
// are refused with 400 InvalidArgument. S3 answers an unsupported value
// with 400 ("Unrecognized or unsupported values won't write a destination
// object and will receive a 400 Bad Request response.", CopyObject API
// reference); the codes and messages are choices. The KMS key ID header is
// sent only when a key was named, and the bucket key header only when it
// is enabled for a KMS algorithm.
//
// A write with a customer-provided key (SSE-C) is stored and answered with
// x-amz-server-side-encryption-customer-algorithm and
// x-amz-server-side-encryption-customer-key-MD5 as sent, and no
// x-amz-server-side-encryption, as the PutObject API reference describes
// its response ("If server-side encryption with a customer-provided
// encryption key was requested, the response will include this header to
// confirm the encryption algorithm that's used."); the bucket default does
// not apply to it. See customerEncryption.
func EncryptionHeaders(
	r *http.Request,
	def *ServerSideEncryptionConfiguration,
) (map[string]string, error) {
	if customer, err := customerEncryption(r); customer != nil || err != nil {
		return customer, err
	}
	sse, keyID, bucketKeyHeader, err := requestEncryption(r)
	if err != nil {
		return nil, err
	}
	return managedEncryption(sse, keyID, bucketKeyHeader, def), nil
}

// managedEncryption returns the headers of an SSE-S3 or SSE-KMS write
// whose request named sse, keyID and bucketKeyHeader (nil for none),
// completed from def as EncryptionHeaders describes.
func managedEncryption(
	sse, keyID string,
	bucketKeyHeader *bool,
	def *ServerSideEncryptionConfiguration,
) map[string]string {
	defSSE, defKeyID, bucketKey := def.Default()
	if sse == "" {
		sse, keyID = defSSE, defKeyID
	} else if keyID == "" && IsKMS(sse) && IsKMS(defSSE) {
		keyID = defKeyID
	}
	if bucketKeyHeader != nil {
		bucketKey = *bucketKeyHeader
	}
	headers := map[string]string{HeaderSSE: sse}
	if IsKMS(sse) {
		if keyID != "" {
			headers[HeaderSSEKMSKeyID] = keyID
		}
		if bucketKey {
			headers[HeaderSSEBucketKey] = "true"
		}
	}
	return headers
}

// requestEncryption returns r's encryption headers, validated as
// EncryptionHeaders describes; bucketKey is nil when r sets none.
func requestEncryption(r *http.Request) (sse, keyID string, bucketKey *bool, err error) {
	sse = r.Header.Get(HeaderSSE)
	keyID = r.Header.Get(HeaderSSEKMSKeyID)
	if sse != "" && !ValidSSE(sse) {
		return "", "", nil, ErrInvalidArgument("The encryption method specified is not supported")
	}
	if keyID != "" && !IsKMS(sse) {
		return "", "", nil, ErrInvalidArgument("Server Side Encryption with AWS KMS managed key " +
			"requires HTTP header x-amz-server-side-encryption : aws:kms")
	}
	if v := r.Header.Get(HeaderSSEBucketKey); v != "" {
		b, pErr := strconv.ParseBool(strings.ToLower(v))
		if pErr != nil {
			return "", "", nil, ErrInvalidArgument(
				"x-amz-server-side-encryption-bucket-key-enabled must be true or false")
		}
		bucketKey = &b
	}
	return sse, keyID, bucketKey, nil
}

// customerEncryption returns the headers of r's SSE-C encryption, nil when
// r names no customer key. The algorithm must be AES256, else S3's
// InvalidEncryptionAlgorithmError, "The encryption request you specified
// is not valid. The valid value is AES256." (the S3 API Reference,
// Error). A key or key MD5 without the algorithm, and the algorithm with
// x-amz-server-side-encryption, are refused with 400 InvalidArgument, a
// choice. The key is checked against its MD5 (customerKeyMD5) and not
// kept: nothing is encrypted, and reads do not ask for it.
func customerEncryption(r *http.Request) (map[string]string, error) {
	alg := r.Header.Get(HeaderSSECAlgorithm)
	if alg == "" {
		if r.Header.Get(HeaderSSECKey) != "" || r.Header.Get(HeaderSSECKeyMD5) != "" {
			return nil, ErrInvalidArgument("Requests specifying Server Side Encryption with " +
				"Customer provided keys must provide a valid encryption algorithm.")
		}
		return nil, nil
	}
	if alg != SSEAES256 {
		return nil, NewError(http.StatusBadRequest, "InvalidEncryptionAlgorithmError",
			"The encryption request you specified is not valid. The valid value is AES256.")
	}
	if r.Header.Get(HeaderSSE) != "" {
		return nil, ErrInvalidArgument("Server Side Encryption with Customer provided keys " +
			"cannot be requested with x-amz-server-side-encryption.")
	}
	sum, err := customerKeyMD5(r.Header.Get(HeaderSSECKey), r.Header.Get(HeaderSSECKeyMD5))
	if err != nil {
		return nil, err
	}
	headers := map[string]string{HeaderSSECAlgorithm: alg}
	if sum != "" {
		headers[HeaderSSECKeyMD5] = sum
	}
	return headers, nil
}

// sseCKeyBytes is the length of an AES256 customer key: the key "must be
// appropriate for use with the algorithm specified" (PutObject API
// reference), and AES256 takes 256 bits.
const sseCKeyBytes = 32

// customerKeyMD5 returns the key MD5 an SSE-C write is answered with: the
// base64 MD5 of key, the base64 customer key, which the request's sent
// MD5, when it sends one, must equal. A missing key (the PutObject API
// reference: the request must provide it), a key that is not the base64
// of 256 bits, and an MD5 that differs are refused with 400
// InvalidArgument; the code and messages are choices.
func customerKeyMD5(key, sent string) (string, error) {
	if key == "" {
		return "", ErrInvalidArgument("Requests specifying Server Side Encryption with " +
			"Customer provided keys must provide an appropriate secret key.")
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != sseCKeyBytes {
		return "", ErrInvalidArgument("The secret key was invalid for the specified algorithm.")
	}
	digest := md5.Sum(raw) //nolint:gosec // S3's integrity check of the key is MD5
	sum := base64.StdEncoding.EncodeToString(digest[:])
	if sent != "" && sent != sum {
		return "", ErrInvalidArgument(
			"The calculated MD5 hash of the key did not match the hash that was provided.")
	}
	return sum, nil
}
