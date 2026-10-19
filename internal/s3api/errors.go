package s3api

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/storage"
	"github.com/go-chi/chi/v5/middleware"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/httpx"
)

// Error is an S3 error response. It implements error so handlers and services
// can return one directly for conditions that have no storage-layer sentinel.
type Error struct {
	XMLName    xml.Name `xml:"Error"`
	Code       string   `xml:"Code"`
	Message    string   `xml:"Message"`
	BucketName string   `xml:"BucketName,omitempty"`
	Key        string   `xml:"Key,omitempty"`
	Resource   string   `xml:"Resource,omitempty"`
	RequestID  string   `xml:"RequestId,omitempty"`
	Status     int      `xml:"-"`
}

func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}

// NewError builds an S3 error response with the given status and code.
func NewError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// ErrInvalidArgument is a 400 InvalidArgument error.
func ErrInvalidArgument(msg string) *Error {
	return NewError(http.StatusBadRequest, "InvalidArgument", msg)
}

// ErrNotImplemented is a 501 NotImplemented error.
func ErrNotImplemented(msg string) *Error {
	return NewError(http.StatusNotImplemented, "NotImplemented", msg)
}

// ErrInvalidRequest is a 400 InvalidRequest error.
func ErrInvalidRequest(msg string) *Error {
	return NewError(http.StatusBadRequest, "InvalidRequest", msg)
}

// ErrMalformedXML is a 400 MalformedXML error.
func ErrMalformedXML() *Error {
	return NewError(
		http.StatusBadRequest,
		"MalformedXML",
		"The XML you provided was not well-formed or did not validate against our published schema.",
	)
}

// ErrInvalidPartNumber is a 416 InvalidPartNumber error, for a GET or HEAD
// of a part the object does not have.
func ErrInvalidPartNumber() *Error {
	return NewError(http.StatusRequestedRangeNotSatisfiable, "InvalidPartNumber",
		"The requested partnumber is not satisfiable")
}

// ErrIncompleteBody is a 400 IncompleteBody error, for bodies that end early
// or decode to the wrong length.
func ErrIncompleteBody(msg string) *Error {
	return NewError(http.StatusBadRequest, "IncompleteBody", msg)
}

// ErrPreconditionFailed is a 412 PreconditionFailed error, for a conditional
// request whose condition does not hold.
func ErrPreconditionFailed() *Error {
	return NewError(http.StatusPreconditionFailed, "PreconditionFailed",
		"At least one of the pre-conditions you specified did not hold")
}

// Target names the bucket and key an error is about. Key is empty for bucket
// operations.
type Target struct {
	Bucket string
	Key    string
}

// WriteError renders err as an S3 error response. Storage sentinels map to
// their S3 codes; anything unrecognized is logged and sent as InternalError.
func WriteError(ctx gas.Context, t Target, err error) error {
	return ctx.XML(errorFor(ctx.Request(), t, err))
}

// WriteHTTPError is WriteError for plain http.Handler middleware, which has no
// gas.Context.
func WriteHTTPError(w http.ResponseWriter, r *http.Request, t Target, err error) {
	status, s3Err := errorFor(r, t, err)
	body, mErr := xml.Marshal(s3Err)
	if mErr != nil {
		httpx.Logger(r.Context()).Error("failed to encode error response").
			Err("error", mErr).Send()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	//nolint:gosec // G705: body is an XML-encoded error document, escaped by encoding/xml
	if _, wErr := w.Write(append([]byte(xml.Header), body...)); wErr != nil {
		httpx.Logger(r.Context()).Warn("failed to write error response").
			Err("error", wErr).Send()
	}
}

func errorFor(r *http.Request, t Target, err error) (int, *Error) {
	s3Err := ToError(r.Context(), err)

	s3Err.BucketName = t.Bucket
	s3Err.Key = t.Key
	s3Err.Resource = r.URL.Path
	s3Err.RequestID = middleware.GetReqID(r.Context())

	return s3Err.Status, s3Err
}

// ToError maps err to an S3 error, logging it when it maps to InternalError.
// It returns a copy, safe for the caller to fill in.
func ToError(ctx context.Context, err error) *Error {
	s3Err := toS3Error(err)
	if s3Err.Status == http.StatusInternalServerError {
		httpx.Logger(ctx).Error("request failed").Err("error", err).Send()
	}
	return s3Err
}

// sentinelErrors maps storage-layer sentinels to the S3 errors they become.
var sentinelErrors = []struct {
	err   error
	s3Err *Error
}{
	{storage.ErrKeyNotFound, NewError(http.StatusNotFound, "NoSuchKey",
		"The specified key does not exist.")},
	{storage.ErrBucketNotFound, NewError(http.StatusNotFound, "NoSuchBucket",
		"The specified bucket does not exist.")},
	{contracts.ErrBucketExists, NewError(http.StatusConflict, "BucketAlreadyOwnedByYou",
		"Your previous request to create the named bucket succeeded and you already own it.")},
	{contracts.ErrBucketNotEmpty, NewError(http.StatusConflict, "BucketNotEmpty",
		"The bucket you tried to delete is not empty.")},
	{contracts.ErrInvalidBucketName, NewError(http.StatusBadRequest, "InvalidBucketName",
		"The specified bucket is not valid.")},
	{storage.ErrInvalidBucket, NewError(http.StatusBadRequest, "InvalidBucketName",
		"The specified bucket is not valid.")},
	{contracts.ErrNoSuchUpload, NewError(http.StatusNotFound, "NoSuchUpload",
		"The specified multipart upload does not exist. The upload ID might be invalid, "+
			"or the multipart upload might have been aborted or completed.")},
	{contracts.ErrInvalidPart, NewError(http.StatusBadRequest, "InvalidPart",
		"One or more of the specified parts could not be found. The part might not have "+
			"been uploaded, or the specified entity tag might not have matched the part's entity tag.")},
	{contracts.ErrInvalidPartOrder, NewError(http.StatusBadRequest, "InvalidPartOrder",
		"The list of parts was not in ascending order. Parts must be ordered by part number.")},
	{contracts.ErrEntityTooSmall, NewError(http.StatusBadRequest, "EntityTooSmall",
		"Your proposed upload is smaller than the minimum allowed object size.")},
	{storage.ErrInvalidKey, ErrInvalidArgument("The specified key is not valid. Keys must be " +
		"1 to 1024 bytes of UTF-8 and must not end in a \".\" path segment.")},
	{contracts.ErrPreconditionFailed, ErrPreconditionFailed()},
}

func toS3Error(err error) *Error {
	var s3Err *Error
	if errors.As(err, &s3Err) {
		cp := *s3Err
		return &cp
	}

	for _, se := range sentinelErrors {
		if errors.Is(err, se.err) {
			cp := *se.s3Err
			return &cp
		}
	}

	return NewError(http.StatusInternalServerError, "InternalError",
		"We encountered an internal error. Please try again.")
}
