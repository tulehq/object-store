package bucket

import (
	"net/http"

	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// getBucketVersioning serves GetBucketVersioning: the bucket's Status and
// MfaDelete as they were set, neither for a bucket never set (the
// GetBucketVersioning API reference's examples).
func (m *Module) getBucketVersioning(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.GetBucketVersioning, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer g.Release()
	cfg, err := m.storage.GetBucketVersioning(g.Context(), bucket)
	g.Release()
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	return ctx.XML(http.StatusOK, s3api.NewVersioningConfiguration(*cfg))
}

// putBucketVersioning serves PutBucketVersioning, answering 200 with no
// body: a Status of Enabled or Suspended is stored, also for a bucket never
// versioned before, which can be suspended; a document without one is 400
// IllegalVersioningConfigurationException, as a bucket can never return
// to unversioned; MfaDelete Disabled is stored and Enabled is 501
// NotImplemented, since MFA delete is not served. x-amz-mfa is not read.
// The document is read and the bucket pinned as putConfig does.
func (m *Module) putBucketVersioning(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}
	r := ctx.Request()

	caller, err := m.authz.Authorize(r, authz.PutBucketVersioning, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	doc, err := s3api.ReadDocument(r, maxConfigDocument, m.verifier.ReadBody)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	cfg, err := s3api.ParseVersioningConfiguration(doc)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	pinned, release, err := caller.Pin(ctx, bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer release()
	if err = m.storage.PutBucketVersioning(pinned, bucket, cfg); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

// listObjectVersions serves ListObjectVersions: one page of the bucket's
// versions and delete markers, interleaved newest first within each key,
// as storage lists them. max-keys defaults to 1000 and is held to it, and
// counts versions, markers and common prefixes alike; a max-keys of 0
// answers an empty page that is not truncated, which the API reference
// leaves unsaid, so a client paging on the markers does not loop on a page
// it cannot resume from. A version-id-marker without a key-marker is 400
// InvalidArgument. No Owner is answered.
func (m *Module) listObjectVersions(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.ListObjectVersions, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer g.Release()

	maxKeys, err := nonNegative(ctx.Query("max-keys"), "max-keys", defaultMaxKeys)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	encodingType, err := parseEncodingType(ctx)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	in := contracts.ListObjectVersionsInput{
		Prefix:          ctx.Query("prefix"),
		Delimiter:       ctx.Query("delimiter"),
		KeyMarker:       ctx.Query("key-marker"),
		VersionIDMarker: ctx.Query("version-id-marker"),
		MaxKeys:         min(maxKeys, defaultMaxKeys),
	}
	out, err := m.storage.ListObjectVersions(g.Context(), bucket, in)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}

	encode := keyEncoder(encodingType)
	res := s3api.NewListVersionsResult()
	res.Name = bucket
	res.Prefix = encode(in.Prefix)
	res.Delimiter = encode(in.Delimiter)
	res.KeyMarker = encode(in.KeyMarker)
	res.VersionIDMarker = in.VersionIDMarker
	res.NextKeyMarker = encode(out.NextKeyMarker)
	res.NextVersionIDMarker = out.NextVersionIDMarker
	res.EncodingType = encodingType
	res.MaxKeys = in.MaxKeys
	res.IsTruncated = out.IsTruncated
	for _, v := range out.Versions {
		lastModified := s3api.Timestamp(v.LastModified)
		if v.DeleteMarker {
			res.Entries = append(res.Entries,
				s3api.NewDeleteMarkerEntry(encode(v.Key), v.VersionID, lastModified, v.IsLatest))
			continue
		}
		res.Entries = append(res.Entries, s3api.NewVersionEntry(encode(v.Key), v.VersionID,
			lastModified, s3api.ETag(v.ETag), storageClass, v.Size, v.IsLatest))
	}
	for _, p := range out.CommonPrefixes {
		res.CommonPrefixes = append(res.CommonPrefixes, s3api.CommonPrefix{Prefix: encode(p)})
	}
	return ctx.XML(http.StatusOK, res)
}
