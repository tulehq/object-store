package bucket

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// errBucketAlreadyExists is S3's 409 BucketAlreadyExists (the S3 API
// Reference, Error codes).
var errBucketAlreadyExists = s3api.NewError(http.StatusConflict, "BucketAlreadyExists",
	"The requested bucket name is not available. The bucket namespace is shared by all "+
		"users of the system. Please select a different name and try again.")

const (
	defaultMaxKeys = 1000
	storageClass   = "STANDARD"
)

// listBuckets serves ListBuckets, which lists the buckets of the caller's
// own account, as on S3: every bucket for a principal of the server's
// account, which owns them all, and none for a principal of another.
func (m *Module) listBuckets(ctx gas.Context) error {
	caller, err := m.authz.Caller(ctx.Request())
	if err != nil {
		return s3api.WriteError(ctx, s3api.Target{}, err)
	}
	if err = caller.Authorize(authz.ListBuckets, s3api.Target{}); err != nil {
		return s3api.WriteError(ctx, s3api.Target{}, err)
	}

	var buckets []contracts.BucketInfo
	if caller.InAccount() {
		if buckets, err = m.storage.ListBuckets(ctx); err != nil {
			return s3api.WriteError(ctx, s3api.Target{}, err)
		}
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
// (list-type=2), GetBucketLocation, GetBucketVersioning,
// ListMultipartUploads, GetBucketPolicy, GetBucketPolicyStatus,
// GetBucketAcl, GetBucketOwnershipControls and the configuration reads of
// configKinds.
func (m *Module) getBucket(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}
	r := ctx.Request()

	if sub := s3api.Subresource(r); sub != "" {
		if serve, ok := m.getSubresource(sub); ok {
			// Any other subresource with sub, such as ?versionId, is one
			// not served with it.
			if err := s3api.CheckSubresource(r, sub); err != nil {
				return s3api.WriteError(ctx, target, err)
			}
			return serve(ctx)
		}
		return s3api.WriteError(ctx, target, s3api.CheckSubresource(r))
	}

	op, list := authz.ListObjects, m.listObjectsV1
	if ctx.Query("list-type") == "2" {
		op, list = authz.ListObjectsV2, m.listObjectsV2
	}
	g, err := m.authz.AuthorizeAndPin(ctx, r, op, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer g.Release()
	return list(ctx, g)
}

// getSubresource returns the handler of the GET bucket operation sub
// names, false for a subresource not served.
func (m *Module) getSubresource(sub string) (func(gas.Context) error, bool) {
	if ck, ok := configKinds[sub]; ok {
		return func(ctx gas.Context) error { return m.getConfig(ctx, ck) }, true
	}
	switch sub {
	case "location", "versioning":
		return func(ctx gas.Context) error { return m.getBucketConfig(ctx, sub) }, true
	case "uploads":
		return m.listMultipartUploads, true
	case "policy":
		return m.getBucketPolicy, true
	case "policyStatus":
		return m.getBucketPolicyStatus, true
	case "acl":
		return m.getBucketACL, true
	case "ownershipControls":
		return m.getBucketOwnershipControls, true
	}
	return nil, false
}

// getBucketConfig serves GetBucketLocation (sub "location") and
// GetBucketVersioning (sub "versioning").
func (m *Module) getBucketConfig(ctx gas.Context, sub string) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	op := authz.GetBucketVersioning
	if sub == "location" {
		op = authz.GetBucketLocation
	}
	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), op, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	g.Release()
	if sub == "location" {
		// Every bucket here is in us-east-1, which S3 reports as an empty
		// constraint.
		return ctx.XML(http.StatusOK, s3api.NewLocationConstraint(""))
	}
	return ctx.XML(http.StatusOK, s3api.NewVersioningConfiguration())
}

// listObjectsV2 serves ListObjectsV2. Continuation tokens are the base64 of the
// last key or common prefix returned. g pins the bucket.
func (m *Module) listObjectsV2(ctx gas.Context, g *authz.Grant) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	in, res, err := parseListObjects(ctx)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}

	out, err := m.storage.ListObjects(g.Context(), bucket, in)
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
// key instead of a continuation token. g pins the bucket.
func (m *Module) listObjectsV1(ctx gas.Context, g *authz.Grant) error {
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

	out, err := m.storage.ListObjects(g.Context(), bucket, in)
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
	// Pinning the bucket checks that it exists in the generation
	// evaluated.
	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.HeadBucket,
		s3api.Target{Bucket: bucket})
	if err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}
	g.Release()

	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

// putBucket serves CreateBucket, PutBucketPolicy (?policy), PutBucketAcl
// (?acl), PutBucketOwnershipControls (?ownershipControls) and the
// configuration writes of configKinds.
//
// CreateBucket of an existing bucket answers a principal of the owner's
// account with BucketAlreadyOwnedByYou, and any other with
// BucketAlreadyExists, as S3 answers a name another account owns. A new
// bucket gets DefaultConfigs.
func (m *Module) putBucket(ctx gas.Context) error {
	bucket := ctx.Param("bucket")

	sub := s3api.Subresource(ctx.Request())
	if serve, ok := m.putSubresource(sub); ok {
		if err := s3api.CheckSubresource(ctx.Request(), sub); err != nil {
			return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
		}
		return serve(ctx)
	}
	if err := s3api.CheckSubresource(ctx.Request()); err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}
	caller, err := m.authz.Authorize(ctx.Request(), authz.CreateBucket,
		s3api.Target{Bucket: bucket})
	if err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}
	configs, err := m.newBucketConfigs(ctx.Request())
	if err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}

	if _, err = m.storage.CreateBucket(ctx, bucket, configs); err != nil {
		if errors.Is(err, contracts.ErrBucketExists) && !caller.InAccount() {
			err = errBucketAlreadyExists
		}
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}

	ctx.SetHeader("Location", "/"+bucket)
	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

// putSubresource returns the handler of the PUT bucket operation sub
// names, false for none.
func (m *Module) putSubresource(sub string) (func(gas.Context) error, bool) {
	if ck, ok := configKinds[sub]; ok {
		return func(ctx gas.Context) error { return m.putConfig(ctx, ck) }, true
	}
	switch sub {
	case "policy":
		return m.putBucketPolicy, true
	case "acl":
		return m.putBucketACL, true
	case "ownershipControls":
		return m.putBucketOwnershipControls, true
	}
	return nil, false
}

// deleteBucket serves DeleteBucket, DeleteBucketPolicy (?policy),
// DeleteBucketOwnershipControls (?ownershipControls) and the
// configuration deletes of configKinds.
func (m *Module) deleteBucket(ctx gas.Context) error {
	bucket := ctx.Param("bucket")

	sub := s3api.Subresource(ctx.Request())
	if err := s3api.CheckSubresource(ctx.Request(), sub); err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}
	if ck, ok := configKinds[sub]; ok {
		return m.deleteConfig(ctx, ck)
	}
	switch sub {
	case "policy":
		return m.deleteBucketPolicy(ctx)
	case "ownershipControls":
		return m.deleteBucketOwnershipControls(ctx)
	}
	if err := s3api.CheckSubresource(ctx.Request()); err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}
	caller, err := m.authz.Authorize(ctx.Request(), authz.DeleteBucket,
		s3api.Target{Bucket: bucket})
	if err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}
	// The bucket deleted is the one evaluated: one deleted and created
	// again since is NoSuchBucket.
	ref, err := caller.Ref(bucket)
	if err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}
	if err = m.storage.DeleteBucket(ctx, ref); err != nil {
		return s3api.WriteError(ctx, s3api.Target{Bucket: bucket}, err)
	}

	return ctx.NoContent()
}

// newBucketConfigs returns the configuration a CreateBucket request's
// bucket is created with (DefaultConfigs): the Object Ownership of its
// x-amz-object-ownership header, BucketOwnerEnforced without one, and the
// ACL of its x-amz-acl or x-amz-grant-* headers, if any.
//
// An ACL that grants anyone but the owner is refused under
// BucketOwnerEnforced with InvalidBucketAclWithObjectOwnership: "if your
// CreateBucket request sets Bucket owner enforced and specifies a bucket
// ACL that provides access to an external AWS account, your request fails
// with a 400 error and returns the InvalidBucketAclWithObjectOwnership
// error code" ("Troubleshooting", Object Ownership, S3 User Guide). A
// public ACL is refused under the other settings, since the new bucket
// blocks public ACLs: "If you try to create a bucket with a public ACL,
// the request will fail." (CreateBucket API reference). The 400 and its
// message are what S3 is reported to answer; the code
// InvalidBucketAclWithBlockPublicAccess is a choice. An ownership value S3
// does not know is refused with InvalidArgument, also a choice.
func (m *Module) newBucketConfigs(r *http.Request) (map[contracts.BucketConfig][]byte, error) {
	ownership := s3api.OwnershipBucketOwnerEnforced
	if vs := r.Header.Values("X-Amz-Object-Ownership"); len(vs) > 0 {
		if !s3api.ValidOwnership(vs[0]) {
			return nil, s3api.ErrInvalidArgument("Invalid x-amz-object-ownership header: " +
				vs[0])
		}
		ownership = vs[0]
	}
	owner := m.ownerID()
	grants, set, err := s3api.HeaderGrants(r, s3api.ACLOwners{Owner: owner, BucketOwner: owner},
		false)
	if err != nil {
		return nil, err //nolint:wrapcheck // an S3 error, answered as is
	}
	var acl []byte
	if set {
		switch {
		case ownership == s3api.OwnershipBucketOwnerEnforced && s3api.GrantsOthers(grants, owner):
			return nil, s3api.ErrInvalidBucketACLWithObjectOwnership()
		case s3api.PublicGrants(grants):
			return nil, s3api.NewError(http.StatusBadRequest,
				"InvalidBucketAclWithBlockPublicAccess",
				"Bucket cannot have public ACLs set with BlockPublicAccess enabled")
		}
		if acl, err = s3api.Encode(s3api.NewAccessControlPolicy(owner, grants)); err != nil {
			return nil, err //nolint:wrapcheck // s3api's own error
		}
	}
	return DefaultConfigs(ownership, acl)
}
