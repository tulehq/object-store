// Package contracts defines the storage interface the HTTP modules depend on,
// with its input and output types and sentinel errors.
package contracts

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/gasmod/gas"
)

var (
	// ErrBucketExists is returned by CreateBucket when the bucket already exists.
	ErrBucketExists = errors.New("storage: bucket already exists")

	// ErrBucketNotEmpty is returned by DeleteBucket when the bucket still holds objects.
	ErrBucketNotEmpty = errors.New("storage: bucket not empty")

	// ErrInvalidBucketName is returned when a bucket name breaks the S3 naming rules.
	ErrInvalidBucketName = errors.New("storage: invalid bucket name")

	// ErrNoSuchUpload is returned for a multipart upload ID that does not exist,
	// was completed or aborted, or belongs to another bucket or key.
	ErrNoSuchUpload = errors.New("storage: no such multipart upload")

	// ErrInvalidPart is returned when a completion names a part that was not
	// uploaded, or with an ETag that does not match the uploaded part.
	ErrInvalidPart = errors.New("storage: invalid part")

	// ErrInvalidPartOrder is returned when a completion lists parts out of
	// ascending part-number order.
	ErrInvalidPartOrder = errors.New("storage: invalid part order")

	// ErrEntityTooSmall is returned when a part other than the last is smaller
	// than MinPartSize.
	ErrEntityTooSmall = errors.New("storage: part too small")

	// ErrPreconditionFailed is returned when a write's Precondition does not
	// hold.
	ErrPreconditionFailed = errors.New("storage: precondition failed")

	// ErrKeyConflict is returned when a key cannot be stored because an object
	// already uses it as a prefix, or a prefix of it is already an object.
	// Keys map to file paths, so "a" and "a/b" cannot both exist.
	ErrKeyConflict = errors.New("storage: key conflicts with an existing object or prefix")

	// ErrUnsupportedKey is returned for keys that do not map onto a file path
	// one-to-one: a trailing slash, or an empty, "." or ".." segment.
	ErrUnsupportedKey = errors.New("storage: unsupported key")
)

// MinPartSize is the smallest size S3 allows for every multipart part but the
// last. It is enforced so uploads that work here also work against S3.
const MinPartSize = 5 << 20

// MaxPartNumber is the highest part number S3 allows.
const MaxPartNumber = 10000

// StorageProvider is the object store the S3 API is served from: the gas
// object storage operations plus the bucket, listing, conditional write and
// multipart operations S3 needs.
type StorageProvider interface {
	gas.StorageProvider

	ListBuckets(ctx context.Context) ([]BucketInfo, error)
	ListObjects(ctx context.Context, bucket string, in ListObjectsInput) (*ListObjectsOutput, error)
	HeadBucket(ctx context.Context, bucket string) (*BucketInfo, error)
	CreateBucket(ctx context.Context, bucket string) (*BucketInfo, error)
	DeleteBucket(ctx context.Context, bucket string) error
	StatObject(ctx context.Context, bucket, key string) (*ObjectInfo, error)
	PutObject(
		ctx context.Context,
		bucket, key string,
		body io.Reader,
		in PutObjectInput,
		cond Precondition,
	) (*ObjectInfo, error)
	CopyObject(ctx context.Context, in CopyObjectInput) (*ObjectInfo, error)

	CreateMultipartUpload(
		ctx context.Context,
		bucket, key string,
		in PutObjectInput,
	) (string, error)
	UploadPart(
		ctx context.Context,
		bucket, key, uploadID string,
		partNumber int,
		body io.Reader,
	) (etag string, err error)
	CompleteMultipartUpload(
		ctx context.Context,
		bucket, key, uploadID string,
		parts []CompletedPart,
		cond Precondition,
	) (*ObjectInfo, error)
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error
	ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error)
	ListMultipartUploads(ctx context.Context, bucket string) ([]UploadInfo, error)
}

// BucketInfo describes a bucket.
type BucketInfo struct {
	CreationDate time.Time
	Name         string
}

// ObjectInfo describes a stored object. ETag is the hex MD5 of the object body,
// unquoted; it is empty for objects stored before ETags were recorded.
// Metadata is the user metadata and Headers the stored system headers, as
// described on PutObjectInput. PartSizes lists the part sizes of an object
// assembled by a multipart upload, and is empty for other objects.
type ObjectInfo struct {
	LastModified time.Time
	Metadata     map[string]string
	Headers      map[string]string
	Key          string
	ContentType  string
	ETag         string
	PartSizes    []int64
	Size         int64
}

// ListObjectsInput selects the keys a ListObjects call returns, following
// ListObjectsV2 semantics: keys are listed in lexicographic order, starting
// after StartAfter, and keys sharing Prefix up to the next Delimiter are
// rolled up into a single common prefix.
type ListObjectsInput struct {
	Prefix     string
	Delimiter  string
	StartAfter string
	MaxKeys    int
}

// ListObjectsOutput is one page of a listing. NextStartAfter is the last key or
// common prefix returned, and is only meaningful when IsTruncated is set.
type ListObjectsOutput struct {
	NextStartAfter string
	Objects        []ObjectInfo
	CommonPrefixes []string
	IsTruncated    bool
}

// PutObjectInput is what is stored alongside an object body. Metadata holds
// user metadata keyed by lowercase name, without the x-amz-meta- prefix.
// Headers holds system headers replayed on reads (Cache-Control,
// Content-Disposition and the like), keyed by canonical header name.
type PutObjectInput struct {
	Metadata    map[string]string
	Headers     map[string]string
	ContentType string
}

// Precondition guards a write, following S3 conditional writes. IfNoneMatch
// requires that no object exists at the key. IfMatch requires an existing
// object whose ETag (unquoted) equals it, or any existing object when it is
// "*". The zero value always holds.
type Precondition struct {
	IfMatch     string
	IfNoneMatch bool
}

// CopyObjectInput copies SrcBucket/SrcKey to DstBucket/DstKey. A nil Replace
// keeps the source's content type, metadata and headers; otherwise the copy
// gets Replace's instead. Precondition applies to the destination.
type CopyObjectInput struct {
	Replace      *PutObjectInput
	SrcBucket    string
	SrcKey       string
	DstBucket    string
	DstKey       string
	Precondition Precondition
}

// CompletedPart names one uploaded part in a CompleteMultipartUpload request.
// ETag is the hex MD5 the part upload returned, unquoted.
type CompletedPart struct {
	ETag       string
	PartNumber int
}

// PartInfo describes one uploaded part of an in-progress multipart upload.
// ETag is the hex MD5 of the part, unquoted.
type PartInfo struct {
	LastModified time.Time
	ETag         string
	PartNumber   int
	Size         int64
}

// UploadInfo describes an in-progress multipart upload.
type UploadInfo struct {
	Initiated time.Time
	Key       string
	UploadID  string
}
