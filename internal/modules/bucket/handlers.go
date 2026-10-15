package bucket

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

const (
	defaultMaxKeys = 1000
	storageClass   = "STANDARD"
)

func (m *Module) listBuckets(ctx gas.Context) error {
	buckets, err := m.storage.ListBuckets(ctx)
	if err != nil {
		return s3api.WriteError(ctx, s3api.Target{}, err)
	}

	out := make([]s3api.Bucket, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, s3api.Bucket{
			Name:         b.Name,
			CreationDate: s3api.Timestamp(b.CreationDate),
		})
	}

	return ctx.XML(http.StatusOK, s3api.NewListAllMyBucketsResult(out))
}

// getBucket serves the GET bucket operations: ListObjects, ListObjectsV2
// (list-type=2), GetBucketLocation, GetBucketVersioning and
// ListMultipartUploads.
func (m *Module) getBucket(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}
	r := ctx.Request()

	switch sub := s3api.Subresource(r); sub {
	case "location", "versioning":
		if _, err := m.storage.HeadBucket(ctx, bucket); err != nil {
			return s3api.WriteError(ctx, target, err)
		}
		if sub == "location" {
			// Every bucket here is in us-east-1, which S3 reports as an empty
			// constraint.
			return ctx.XML(http.StatusOK, s3api.NewLocationConstraint(""))
		}
		return ctx.XML(http.StatusOK, s3api.NewVersioningConfiguration())
	case "uploads":
		return m.listMultipartUploads(ctx)
	case "":
	default:
		return s3api.WriteError(ctx, target, s3api.CheckSubresource(r))
	}

	if ctx.Query("list-type") == "2" {
		return m.listObjectsV2(ctx)
	}
	return m.listObjectsV1(ctx)
}

// listObjectsV2 serves ListObjectsV2. Continuation tokens are the base64 of the
// last key or common prefix returned.
func (m *Module) listObjectsV2(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	in, res, err := parseListObjects(ctx)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}

	out, err := m.storage.ListObjects(ctx, bucket, in)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}

	encode := keyEncoder(res.EncodingType)

	res.Name = bucket
	res.Prefix = encode(in.Prefix)
	res.Delimiter = encode(in.Delimiter)
	res.StartAfter = encode(res.StartAfter)
	res.IsTruncated = out.IsTruncated
	res.KeyCount = len(out.Objects) + len(out.CommonPrefixes)
	if out.IsTruncated {
		res.NextContinuationToken = base64.URLEncoding.EncodeToString([]byte(out.NextStartAfter))
	}

	for _, o := range out.Objects {
		res.Contents = append(res.Contents, s3api.Object{
			Key:          encode(o.Key),
			LastModified: s3api.Timestamp(o.LastModified),
			ETag:         s3api.ETag(o.ETag),
			Size:         o.Size,
			StorageClass: storageClass,
		})
	}
	for _, p := range out.CommonPrefixes {
		res.CommonPrefixes = append(res.CommonPrefixes, s3api.CommonPrefix{Prefix: encode(p)})
	}

	return ctx.XML(http.StatusOK, res)
}

// listObjectsV1 serves ListObjects (version 1), which pages with a marker
// key instead of a continuation token.
func (m *Module) listObjectsV1(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	in, err := parseListInput(ctx)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	encodingType, err := parseEncodingType(ctx)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	in.StartAfter = ctx.Query("marker")

	out, err := m.storage.ListObjects(ctx, bucket, in)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}

	encode := keyEncoder(encodingType)
	res := s3api.NewListBucketResultV1()
	res.Name = bucket
	res.Prefix = encode(in.Prefix)
	res.Delimiter = encode(in.Delimiter)
	res.Marker = encode(in.StartAfter)
	res.EncodingType = encodingType
	res.MaxKeys = in.MaxKeys
	res.IsTruncated = out.IsTruncated
	// S3 only sends NextMarker when a delimiter is set; without one, clients
	// resume from the last key returned.
	if out.IsTruncated && in.Delimiter != "" {
		res.NextMarker = encode(out.NextStartAfter)
	}

	for _, o := range out.Objects {
		res.Contents = append(res.Contents, s3api.Object{
			Key:          encode(o.Key),
			LastModified: s3api.Timestamp(o.LastModified),
			ETag:         s3api.ETag(o.ETag),
			Size:         o.Size,
			StorageClass: storageClass,
		})
	}
	for _, p := range out.CommonPrefixes {
		res.CommonPrefixes = append(res.CommonPrefixes, s3api.CommonPrefix{Prefix: encode(p)})
	}

	return ctx.XML(http.StatusOK, res)
}

// keyEncoder returns the function that encodes keys in a listing response for
// the request's encoding-type.
func keyEncoder(encodingType string) func(string) string {
	if encodingType == "url" {
		return url.PathEscape
	}
	return func(s string) string { return s }
}

// parseListInput reads the prefix, delimiter and max-keys query parameters
// that both ListObjects versions share.
func parseListInput(ctx gas.Context) (contracts.ListObjectsInput, error) {
	in := contracts.ListObjectsInput{
		Prefix:    ctx.Query("prefix"),
		Delimiter: ctx.Query("delimiter"),
		MaxKeys:   defaultMaxKeys,
	}

	n, err := nonNegative(ctx.Query("max-keys"), "max-keys", defaultMaxKeys)
	if err != nil {
		return in, err
	}
	in.MaxKeys = min(n, defaultMaxKeys)

	return in, nil
}

// nonNegative parses the query parameter name, valued v, as a non-negative
// integer, returning def when it is absent.
func nonNegative(v, name string, def int) (int, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, s3api.ErrInvalidArgument(name + " must be a non-negative integer")
	}
	return n, nil
}

// parseEncodingType reads the encoding-type query parameter.
func parseEncodingType(ctx gas.Context) (string, error) {
	switch et := ctx.Query("encoding-type"); et {
	case "", "url":
		return et, nil
	default:
		return "", s3api.ErrInvalidArgument("Invalid Encoding Method specified in Request")
	}
}

// parseListObjects reads the ListObjectsV2 query parameters. It returns the
// storage input and a response document with the echoed request fields set.
func parseListObjects(
	ctx gas.Context,
) (contracts.ListObjectsInput, *s3api.ListBucketResult, error) {
	res := s3api.NewListBucketResult()
	in, err := parseListInput(ctx)
	if err != nil {
		return in, nil, err
	}
	res.MaxKeys = in.MaxKeys

	if res.EncodingType, err = parseEncodingType(ctx); err != nil {
		return in, nil, err
	}

	res.StartAfter = ctx.Query("start-after")
	in.StartAfter = res.StartAfter

	if token := ctx.Query("continuation-token"); token != "" {
		after, err := base64.URLEncoding.DecodeString(token)
		if err != nil {
			return in, nil, s3api.ErrInvalidArgument("The continuation token provided is incorrect")
		}
		res.ContinuationToken = token
		in.StartAfter = string(after)
	}

	return in, res, nil
}

func (m *Module) headBucket(ctx gas.Context) error {
	bucket := ctx.Param("bucket")

	if err := s3api.CheckSubresource(ctx.Request()); err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}

	if _, err := m.storage.HeadBucket(ctx, bucket); err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}

	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

func (m *Module) putBucket(ctx gas.Context) error {
	bucket := ctx.Param("bucket")

	if err := s3api.CheckSubresource(ctx.Request()); err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}

	if _, err := m.storage.CreateBucket(ctx, bucket); err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}

	ctx.SetHeader("Location", "/"+bucket)
	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

func (m *Module) deleteBucket(ctx gas.Context) error {
	bucket := ctx.Param("bucket")

	if err := s3api.CheckSubresource(ctx.Request()); err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}

	if err := m.storage.DeleteBucket(ctx, bucket); err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}

	return ctx.NoContent()
}
