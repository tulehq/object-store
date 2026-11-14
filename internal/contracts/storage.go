// Package contracts defines the storage interface the HTTP modules depend on,
// with its input and output types and sentinel errors.
package contracts

import (
	"context"
	"errors"
	"io"
	"time"
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

	// ErrOperationAborted is returned by DeleteBucket when operations in
	// progress on the bucket did not end in time.
	ErrOperationAborted = errors.New("storage: a conflicting operation is in progress")

	// ErrNoSuchBucketPolicy is returned by GetBucketPolicy for a bucket that
	// has no policy.
	ErrNoSuchBucketPolicy = errors.New("storage: the bucket has no policy")

	// ErrNoSuchBucketConfig is returned by GetBucketConfig for a bucket
	// that has no configuration of the kind asked for, and by GetObjectACL
	// for an object that has no ACL of its own.
	ErrNoSuchBucketConfig = errors.New("storage: the bucket has no such configuration")

	// ErrInvalidCopyRange is returned by UploadPartCopy for a CopyRange
	// that is not within its source object.
	ErrInvalidCopyRange = errors.New("storage: the copy range is not within the source")

	// ErrRangeCopySourceTooSmall is returned by UploadPartCopy for a
	// CopyRange of a source object no larger than MinRangeCopySource.
	ErrRangeCopySourceTooSmall = errors.New("storage: the source is too small for a range copy")

	// ErrEntityTooLarge is returned by UploadPartCopy for a copy larger
	// than MaxCopyPartSize.
	ErrEntityTooLarge = errors.New("storage: part too large")
)

// BucketConfig names a kind of bucket configuration, each kept as one
// document per bucket, as S3 keeps a bucket's subresources.
type BucketConfig string

// The bucket configurations the storage keeps.
const (
	ConfigTagging           BucketConfig = "tagging"
	ConfigCORS              BucketConfig = "cors"
	ConfigPublicAccessBlock BucketConfig = "publicAccessBlock"
	ConfigOwnership         BucketConfig = "ownershipControls"
	ConfigEncryption        BucketConfig = "encryption"
	ConfigACL               BucketConfig = "acl"
)

// BucketConfigs lists every BucketConfig.
var BucketConfigs = []BucketConfig{
	ConfigTagging, ConfigCORS, ConfigPublicAccessBlock, ConfigOwnership, ConfigEncryption,
	ConfigACL,
}

// MinPartSize is the smallest size S3 allows for every multipart part but the
// last. It is enforced so uploads that work here also work against S3.
const MinPartSize = 5 << 20

// MaxPartNumber is the highest part number S3 allows.
const MaxPartNumber = 10000

// MaxCopyPartSize is the largest part UploadPartCopy copies, S3's 5 GiB
// maximum part size ("Amazon S3 multipart upload limits", S3 User Guide).
const MaxCopyPartSize = 5 << 30

// MinRangeCopySource is the size a source object must exceed for
// UploadPartCopy to copy a range of it: "You can copy a range only if the
// source object is greater than 5 MB." (UploadPartCopy API reference).
const MinRangeCopySource = 5 << 20

// StorageProvider is the object store the S3 API is served from: the bucket,
// object, listing, conditional write and multipart operations S3 needs.
//
// The operations on a bucket's objects, uploads, listing and policy
// (everything but ListBuckets, HeadBucket, CreateBucket, DeleteBucket and
// GetBucketPolicy) require a context Pin returned for that bucket, and
// fail without one.
type StorageProvider interface {
	ListBuckets(ctx context.Context) ([]BucketInfo, error)
	ListObjects(ctx context.Context, bucket string, in ListObjectsInput) (*ListObjectsOutput, error)
	HeadBucket(ctx context.Context, bucket string) (*BucketInfo, error)
	// CreateBucket creates bucket with configs as its initial
	// configuration, stored before the bucket exists, so it never exists
	// without them.
	CreateBucket(
		ctx context.Context,
		bucket string,
		configs map[BucketConfig][]byte,
	) (*BucketInfo, error)
	DeleteBucket(ctx context.Context, ref BucketRef) error
	StatObject(ctx context.Context, bucket, key string) (*ObjectInfo, error)
	GetObject(ctx context.Context, bucket, key string) (*Object, error)
	DeleteObject(ctx context.Context, bucket, key string) error
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
	// UploadPart stores a part of an upload and returns its ETag and the
	// system headers the upload's object will be stored with, which a
	// part's response reports.
	UploadPart(
		ctx context.Context,
		bucket, key, uploadID string,
		partNumber int,
		body io.Reader,
	) (etag string, headers map[string]string, err error)
	CompleteMultipartUpload(
		ctx context.Context,
		bucket, key, uploadID string,
		parts []CompletedPart,
		cond Precondition,
	) (*ObjectInfo, error)
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error
	ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error)
	ListMultipartUploads(ctx context.Context, bucket string) ([]UploadInfo, error)

	// Pin locks each bucket refs names against deletion and creation and
	// checks that it is still of the generation named, failing with
	// storage.ErrBucketNotFound when it is not. It returns a context
	// pinned to the buckets, which every operation on a bucket's objects,
	// uploads and policy requires, and the function that releases them.
	Pin(ctx context.Context, refs ...BucketRef) (context.Context, func(), error)

	// GetBucketPolicy returns bucket's generation and its policy document
	// as it was stored: storage.ErrBucketNotFound when there is no such
	// bucket, and ErrNoSuchBucketPolicy, with the generation, when it has
	// no policy. Both are read together, so the policy is the one stored
	// for that generation.
	GetBucketPolicy(ctx context.Context, bucket string) (string, []byte, error)
	// PutBucketPolicy stores doc as bucket's policy, replacing any it had.
	PutBucketPolicy(ctx context.Context, bucket string, doc []byte) error
	// DeleteBucketPolicy removes bucket's policy. A bucket without one is
	// not an error.
	DeleteBucketPolicy(ctx context.Context, bucket string) error

	// GetBucketConfig returns bucket's configuration of kind as it was
	// stored: storage.ErrBucketNotFound when there is no such bucket, and
	// ErrNoSuchBucketConfig when it has none of that kind. ctx need not be
	// pinned; one pinned to bucket reads it under the pin.
	GetBucketConfig(ctx context.Context, bucket string, kind BucketConfig) ([]byte, error)
	// PutBucketConfig stores doc as bucket's configuration of kind,
	// replacing any it had.
	PutBucketConfig(ctx context.Context, bucket string, kind BucketConfig, doc []byte) error
	// DeleteBucketConfig removes bucket's configuration of kind. A bucket
	// without one is not an error.
	DeleteBucketConfig(ctx context.Context, bucket string, kind BucketConfig) error

	// GetObjectACL returns the ACL document stored for the object at key
	// as it now is, if check passes for that object:
	// storage.ErrObjectNotFound when there is no object, and
	// ErrNoSuchBucketConfig when the object has no ACL of its own, which
	// includes one written over, or deleted and written again, since its
	// ACL was stored.
	GetObjectACL(ctx context.Context, bucket, key string, check ObjectCheck) ([]byte, error)
	// PutObjectACL stores doc as the ACL of the object at key as it now
	// is, if check passes for that object: storage.ErrObjectNotFound when
	// there is none.
	PutObjectACL(ctx context.Context, bucket, key string, doc []byte, check ObjectCheck) error

	// GetObjectTagging returns the tags of the object at key as it now is:
	// the ones it was written with, or those PutObjectTagging stored for it
	// since, which an object written over, or deleted and written again,
	// does not keep: storage.ErrObjectNotFound when there is no object.
	// ctx need not be pinned; one pinned to bucket reads them under the
	// pin.
	GetObjectTagging(ctx context.Context, bucket, key string) ([]Tag, error)
	// StatObjectTagging returns the info of the object at key as it now
	// is with its Tags set, as GetObjectTagging returns them, both read
	// from one open of the object, so the tags are that object's:
	// storage.ErrObjectNotFound when there is no object. ctx need not be
	// pinned; one pinned to bucket reads them under the pin.
	StatObjectTagging(ctx context.Context, bucket, key string) (*ObjectInfo, error)
	// PutObjectTagging stores tags as the tags of the object at key as it
	// now is, replacing any it had, if check passes for that object; no
	// tags removes them: storage.ErrObjectNotFound when there is no
	// object.
	PutObjectTagging(
		ctx context.Context,
		bucket, key string,
		tags []Tag,
		check ObjectCheck,
	) error

	// UploadPartCopy stores as part in.PartNumber of an upload the bytes of
	// its source object in in.Range, or all of them, if in.Condition holds
	// of the source, and returns the part as stored and the system headers
	// the upload's object will be stored with, as UploadPart does. ctx must
	// be pinned to both buckets.
	UploadPartCopy(
		ctx context.Context,
		in UploadPartCopyInput,
	) (*PartInfo, map[string]string, error)
}

// BucketRef names a bucket of one generation. A bucket deleted and created
// again under the same name has a new Generation, so a BucketRef read from
// the old one does not name it. An empty Generation names no bucket.
type BucketRef struct {
	Name       string
	Generation string
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
// assembled by a multipart upload, and is empty for other objects. Tags
// are the object's tags, as GetObjectTagging returns them; only GetObject
// and StatObjectTagging read them, and every other operation leaves them
// nil.
type ObjectInfo struct {
	LastModified time.Time
	Metadata     map[string]string
	Headers      map[string]string
	Key          string
	ContentType  string
	ETag         string
	PartSizes    []int64
	Tags         []Tag
	Size         int64
}

// Tag is one tag of an object's tag set.
type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Object is an object opened for reading. The caller must close Body.
type Object struct {
	Body io.ReadCloser
	ObjectInfo
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
// Content-Disposition and the like), keyed by canonical header name. ACL,
// when set, is stored as the object's ACL (see GetObjectACL) once it is
// written, replacing any the object had, and an object written without one
// has none; it is not part of what a copy of the object copies. Tags are
// stored as the object's tags in the same way, and are copied only as
// CopyObjectInput's CopyTags says.
type PutObjectInput struct {
	Metadata    map[string]string
	Headers     map[string]string
	ContentType string
	ACL         []byte `json:",omitempty"`
	Tags        []Tag  `json:",omitempty"`
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
// gets Replace's instead. Headers, when set, are set on the copy over
// either, such as the encryption headers a copy gets from its own request;
// an empty value removes the header.
// ACL is the copy's ACL, as PutObjectInput's. Tags are the copy's tags,
// unless CopyTags is set, which gives it its source's instead, as S3's
// COPY tagging directive does. Precondition applies to the destination,
// and SourceCondition to the source as it is read. SourceCheck, when set,
// is called with the source's info, Tags included, as it was opened for
// the copy, and an error it returns fails the copy before anything is
// written: it binds the copy to the source its authorization was
// evaluated for.
type CopyObjectInput struct {
	Replace         *PutObjectInput
	Headers         map[string]string
	SourceCondition SourceCondition
	ACL             []byte
	Tags            []Tag
	SrcBucket       string
	SrcKey          string
	DstBucket       string
	DstKey          string
	SourceCheck     ObjectCheck
	Precondition    Precondition
	CopyTags        bool
}

// ObjectCheck checks an object an operation opened, its info with its
// Tags, before the operation acts on it; an error it returns fails the
// operation with that error. A nil ObjectCheck checks nothing.
type ObjectCheck func(*ObjectInfo) error

// SourceCondition guards a copy's source, following S3's
// x-amz-copy-source-if-* headers and RFC 7232's order of evaluation: a set
// IfMatch, an unquoted ETag or "*", replaces IfUnmodifiedSince, and a set
// IfNoneMatch replaces IfModifiedSince. The zero value always holds.
type SourceCondition struct {
	IfModifiedSince   *time.Time
	IfUnmodifiedSince *time.Time
	IfMatch           string
	IfNoneMatch       string
}

// UploadPartCopyInput copies SrcBucket/SrcKey, or its Range, into part
// PartNumber of upload UploadID of Bucket/Key. SourceCheck is as
// CopyObjectInput's.
type UploadPartCopyInput struct {
	Range           *CopyRange
	SourceCheck     ObjectCheck
	SourceCondition SourceCondition
	Bucket          string
	Key             string
	UploadID        string
	SrcBucket       string
	SrcKey          string
	PartNumber      int
}

// CopyRange is the bytes First to Last of a source object, both included
// and counted from 0.
type CopyRange struct {
	First int64
	Last  int64
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
