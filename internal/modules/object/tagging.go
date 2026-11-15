package object

import (
	"net/http"
	"strconv"

	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// maxTaggingDocument bounds a PutObjectTagging document. Ten tags of S3's
// longest keys and values take a few kilobytes; S3 documents no limit,
// and this one is a choice, the one the bucket configurations take.
const maxTaggingDocument = 64 * 1024

// putObjectTagging serves PutObjectTagging (PUT /<bucket>/<key>?tagging),
// answering 200 with no body: the document's tag set replaces the
// object's, and an empty one removes it ("If you send this request with
// an empty tag set, Amazon S3 deletes the existing tag set on the object",
// "Tagging your objects", S3 User Guide). The document is read and parsed
// before the request is evaluated, since its tags are the request's
// s3:RequestObjectTag and s3:RequestObjectTagKeys. A missing object is 404
// NoSuchKey, as for PutObjectAcl. With ?versionId it tags that version,
// and never creates one: "Some actions that modify the current object
// don't create a new version ... such as changing the tags on an object"
// (S3 User Guide, "How S3 Versioning works"). Like GetObjectTagging and
// DeleteObjectTagging it answers x-amz-version-id, the version tagged
// (their API references), in a bucket that is not Unversioned, and a
// delete marker as writeSubresourceError describes.
func (m *Module) putObjectTagging(ctx gas.Context, t s3api.Target) error {
	r := ctx.Request()
	caller, err := m.authz.Caller(r)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	doc, err := s3api.ReadDocument(r, maxTaggingDocument, m.verifier.ReadBody)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	tags, err := s3api.ParseObjectTagging(doc)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	caller.SetRequestTags(tags)
	if err = caller.Authorize(authz.PutObjectTagging, t); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	pinned, release, err := caller.Pin(ctx, t.Bucket)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer release()
	versionID, err := m.storage.PutObjectTagging(pinned, objectRef(t), tags,
		caller.ObjectCheck(t))
	if err != nil {
		return writeSubresourceError(ctx, t, err)
	}
	setVersionHeader(ctx.ResponseWriter(), versionID)
	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

// getObjectTagging serves GetObjectTagging (GET /<bucket>/<key>?tagging):
// the tag set of the object, or of the version ?versionId names, empty for
// one without tags, answered only for the version the request was
// evaluated for (authz.CheckObject).
func (m *Module) getObjectTagging(ctx gas.Context, t s3api.Target) error {
	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.GetObjectTagging, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()
	info, err := m.storage.StatObjectTagging(g.Context(), objectRef(t))
	if err != nil {
		return writeSubresourceError(ctx, t, err)
	}
	if err = g.CheckObject(t, info); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	setVersionHeader(ctx.ResponseWriter(), info.VersionID)
	return ctx.XML(http.StatusOK, s3api.NewTagging(info.Tags))
}

// deleteObjectTagging serves DeleteObjectTagging (DELETE
// /<bucket>/<key>?tagging), answering 204 No Content, also for an object
// without tags.
func (m *Module) deleteObjectTagging(ctx gas.Context, t s3api.Target) error {
	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.DeleteObjectTagging, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()
	versionID, err := m.storage.PutObjectTagging(g.Context(), objectRef(t), nil,
		g.ObjectCheck(t))
	if err != nil {
		return writeSubresourceError(ctx, t, err)
	}
	setVersionHeader(ctx.ResponseWriter(), versionID)
	return ctx.NoContent()
}

// setTagCount sets the x-amz-tagging-count header of a GetObject or
// HeadObject of t, an object with tags, for a caller allowed
// s3:GetObjectTagging on it: "The number of tags, if any, on the object,
// when you have the relevant permission to read object tags." (GetObject
// and HeadObject API references). An object without tags gets no header,
// as "if any" reads.
func setTagCount(
	w http.ResponseWriter,
	caller *authz.Caller,
	t s3api.Target,
	tags []contracts.Tag,
) error {
	if len(tags) == 0 {
		return nil
	}
	ok, err := caller.Allows(authz.GetObjectTagging, t)
	if err != nil {
		return err //nolint:wrapcheck // an S3 or evaluation error, answered as is
	}
	if ok {
		w.Header().Set(s3api.HeaderTaggingCount, strconv.Itoa(len(tags)))
	}
	return nil
}
