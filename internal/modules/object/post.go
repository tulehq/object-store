package object

import (
	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/postpolicy"
	"tule.dev/object-store/internal/s3api"
)

// PostObject serves a POST Object form whose policy verified (up), which the
// bucket module routes here: it stores the form's file as PutObject stores
// a body, evaluated as s3:PutObject for the policy's signer with
// s3:authType POST. The request's headers are only those the form's fields
// set (see the app's serveForm), so the object gets its bucket's defaults,
// encryption and ACL, and the form's Content-Type. It is answered with
// up.Status and an empty body, with the headers a PutObject's response has
// ("POST Object", Response Headers).
func (m *Module) PostObject(ctx gas.Context, up *postpolicy.Upload) error {
	t := s3api.Target{Bucket: up.Bucket, Key: up.Key}
	r := ctx.Request()
	g, err := m.authz.AuthorizeAndPin(ctx, r, authz.PutObject, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()

	ws, err := m.settings(g.Context(), r, g.Caller(), t.Bucket, true)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	info, err := m.storage.PostObject(g.Context(), t.Bucket, t.Key, r.Body, ws.putInput(r))
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}

	if etag := s3api.ETag(info.ETag); etag != "" {
		ctx.SetHeader("ETag", etag)
	}
	setVersionHeader(ctx.ResponseWriter(), info.VersionID)
	setEncryptionHeaders(ctx.ResponseWriter(), ws.encryption)
	setExpiration(ctx.ResponseWriter(), ws.lifecycle, info)
	ctx.ResponseWriter().WriteHeader(up.Status)
	return nil
}
