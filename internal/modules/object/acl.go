package object

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"

	"github.com/gasmod/gas"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// maxACLDocument bounds a PutObjectAcl document; S3 documents no limit,
// and this one is a choice.
const maxACLDocument = 64 * 1024

// writeSettings is what a write needs from its bucket's configuration and
// its request beyond the object: the ACL to store with the object, nil for
// none, the encryption headers it is stored and answered with, and the
// tags of its x-amz-tagging header.
type writeSettings struct {
	encryption map[string]string
	acl        []byte
	tags       []contracts.Tag
}

// settings reads the ACL, encryption and, with readTags, tags of a
// PutObject, CopyObject or CreateMultipartUpload request r by caller to
// bucket, ctx being pinned to it. The ACL is checked as writeACL
// describes; the encryption is r's or the bucket's default
// (s3api.EncryptionHeaders); the tags are s3api.RequestTags', and none
// without readTags.
func (m *Module) settings(
	ctx context.Context,
	r *http.Request,
	caller *authz.Caller,
	bucket string,
	readTags bool,
) (*writeSettings, error) {
	acl, err := m.writeACL(ctx, r, caller, bucket)
	if err != nil {
		return nil, err
	}
	def, err := s3api.Encryption(ctx, m.storage, bucket)
	if err != nil {
		return nil, err //nolint:wrapcheck // an S3 or storage error, answered as is
	}
	enc, err := s3api.EncryptionHeaders(r, def)
	if err != nil {
		return nil, err //nolint:wrapcheck // an S3 error, answered as is
	}
	ws := &writeSettings{encryption: enc, acl: acl}
	if readTags {
		if ws.tags, err = s3api.RequestTags(r); err != nil {
			return nil, err //nolint:wrapcheck // an S3 error, answered as is
		}
	}
	return ws, nil
}

// setEncryptionHeaders sets the encryption headers of a write's response.
func setEncryptionHeaders(w http.ResponseWriter, headers map[string]string) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
}

// writeACL returns the ACL document to store with the object caller
// writes with r to bucket, nil for none, as S3 Object Ownership decides
// ("Controlling ownership of objects and disabling ACLs for your bucket",
// S3 User Guide):
//
//   - BucketOwnerEnforced: the bucket owner owns the object, and an ACL
//     header is refused with AccessControlListNotSupported unless it
//     grants the bucket owner FULL_CONTROL alone (s3api.CheckACL); nothing
//     is stored;
//   - BucketOwnerPreferred: the bucket owner owns the object written with
//     bucket-owner-full-control, the writer's account any other;
//   - ObjectWriter (and a bucket without Object Ownership): the writer's
//     account owns it.
//
// Under the last two, the ACL headers' ACL is checked against
// BlockPublicAcls and stored, and an object written without one by an
// account other than the bucket owner's is stored with the private ACL,
// to record its owner. A stored ACL is only answered by GetObjectAcl; it
// is never evaluated, and the bucket owner is evaluated as the owner of
// every object.
func (m *Module) writeACL(
	ctx context.Context,
	r *http.Request,
	caller *authz.Caller,
	bucket string,
) ([]byte, error) {
	ownership, err := s3api.Ownership(ctx, m.storage, bucket)
	if err != nil {
		return nil, err //nolint:wrapcheck // an S3 or storage error, answered as is
	}
	bucketOwner := s3api.CanonicalUserID(m.authz.Account())
	owner := objectOwner(ownership, r, s3api.CanonicalUserID(caller.AccountID()), bucketOwner)
	owners := s3api.ACLOwners{Owner: owner, BucketOwner: bucketOwner}
	grants, set, err := s3api.HeaderGrants(r, owners, true)
	if err != nil {
		return nil, err //nolint:wrapcheck // an S3 error, answered as is
	}
	if !set {
		if ownership == s3api.OwnershipBucketOwnerEnforced || owner == bucketOwner {
			return nil, nil
		}
		grants = []s3api.Grant{s3api.OwnerFullControl(owner)}
	}
	block, err := s3api.PublicAccessBlock(ctx, m.storage, bucket)
	if err != nil {
		return nil, err //nolint:wrapcheck // an S3 or storage error, answered as is
	}
	if err = s3api.CheckACL(ownership, block, grants, bucketOwner); err != nil {
		return nil, err //nolint:wrapcheck // an S3 error, answered as is
	}
	if ownership == s3api.OwnershipBucketOwnerEnforced {
		return nil, nil
	}
	return s3api.Encode(
		s3api.NewAccessControlPolicy(owner, grants),
	) //nolint:wrapcheck // s3api's own error
}

// objectOwner is the canonical ID of the owner of an object writer
// writes with r to a bucket of bucketOwner with Object Ownership
// ownership, as writeACL describes.
func objectOwner(ownership string, r *http.Request, writer, bucketOwner string) string {
	switch ownership {
	case s3api.OwnershipBucketOwnerEnforced:
		return bucketOwner
	case s3api.OwnershipBucketOwnerPreferred:
		if s3api.CannedACL(r) == "bucket-owner-full-control" {
			return bucketOwner
		}
	}
	return writer
}

// storedACL returns the owner and grants of the object at key: its stored
// ACL's, or the bucket owner with FULL_CONTROL for an object without one.
// It is storage.ErrObjectNotFound when there is no object.
func (m *Module) storedACL(ctx context.Context, bucket, key string) (string, []s3api.Grant, error) {
	bucketOwner := s3api.CanonicalUserID(m.authz.Account())
	doc, err := m.storage.GetObjectACL(ctx, bucket, key)
	if errors.Is(err, contracts.ErrNoSuchBucketConfig) {
		return bucketOwner, []s3api.Grant{s3api.OwnerFullControl(bucketOwner)}, nil
	}
	if err != nil {
		return "", nil, err //nolint:wrapcheck // the storage's own error, answered as is
	}
	var acl s3api.AccessControlPolicy
	if err = xml.Unmarshal(doc, &acl); err != nil {
		return "", nil, fmt.Errorf("object: the stored ACL of %q does not parse: %w", key, err)
	}
	if acl.Owner == nil {
		return "", nil, fmt.Errorf("object: the stored ACL of %q has no owner", key)
	}
	return acl.Owner.ID, acl.Grants, nil
}

// getObjectACL serves GetObjectAcl (GET /<bucket>/<key>?acl): the
// object's effective ACL. With ACLs disabled it is FULL_CONTROL for the
// bucket owner, who owns the object: "Requests to read ACLs always return
// a response that shows full control for the bucket owner."
// ("Troubleshooting", Object Ownership, S3 User Guide). Otherwise it is
// the stored ACL, without its public grants under IgnorePublicAcls, as
// GetBucketAcl answers. A missing object is 404 NoSuchKey, the only error
// the PutObjectAcl and GetObjectAcl API references list.
func (m *Module) getObjectACL(ctx gas.Context, t s3api.Target) error {
	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.GetObjectACL, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer g.Release()
	pinned := g.Context()
	ownership, err := s3api.Ownership(pinned, m.storage, t.Bucket)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	owner, grants, err := m.storedACL(pinned, t.Bucket, t.Key)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	if ownership == s3api.OwnershipBucketOwnerEnforced {
		owner = s3api.CanonicalUserID(m.authz.Account())
		grants = []s3api.Grant{s3api.OwnerFullControl(owner)}
	} else {
		block, bErr := s3api.PublicAccessBlock(pinned, m.storage, t.Bucket)
		if bErr != nil {
			return s3api.WriteError(ctx, t, bErr)
		}
		if block != nil && block.IgnorePublicAcls {
			grants = s3api.WithoutPublicGrants(grants)
		}
	}
	return ctx.XML(http.StatusOK, s3api.NewAccessControlPolicy(owner, grants))
}

// putObjectACL serves PutObjectAcl (PUT /<bucket>/<key>?acl), answering
// 200 with no body. The ACL is the request's headers', or else its
// document's (s3api.RequestGrants), a canned ACL expanded for the
// object's owner, and is checked with s3api.CheckACL. With ACLs disabled
// an accepted ACL changes nothing and is not stored; otherwise it is
// stored for the object as it now is, kept by the object's owner.
func (m *Module) putObjectACL(ctx gas.Context, t s3api.Target) error {
	r := ctx.Request()
	caller, err := m.authz.Authorize(r, authz.PutObjectACL, t)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	doc, err := s3api.ReadDocument(r, maxACLDocument, m.verifier.ReadBody)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	pinned, release, err := caller.Pin(ctx, t.Bucket)
	if err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	defer release()
	if err = m.setObjectACL(pinned, r, t, doc); err != nil {
		return s3api.WriteError(ctx, t, err)
	}
	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

// setObjectACL sets the ACL r, with the document doc, sets on the object
// t, ctx being pinned to its bucket, as putObjectACL describes.
func (m *Module) setObjectACL(
	ctx context.Context,
	r *http.Request,
	t s3api.Target,
	doc []byte,
) error {
	ownership, err := s3api.Ownership(ctx, m.storage, t.Bucket)
	if err != nil {
		return err //nolint:wrapcheck // an S3 or storage error, answered as is
	}
	owner, _, err := m.storedACL(ctx, t.Bucket, t.Key)
	if err != nil {
		return err
	}
	bucketOwner := s3api.CanonicalUserID(m.authz.Account())
	if ownership == s3api.OwnershipBucketOwnerEnforced {
		owner = bucketOwner
	}
	grants, err := s3api.RequestGrants(r, s3api.ACLOwners{Owner: owner, BucketOwner: bucketOwner},
		true, func() ([]byte, error) { return doc, nil })
	if err != nil {
		return err //nolint:wrapcheck // an S3 error, answered as is
	}
	block, err := s3api.PublicAccessBlock(ctx, m.storage, t.Bucket)
	if err != nil {
		return err //nolint:wrapcheck // an S3 or storage error, answered as is
	}
	if err = s3api.CheckACL(ownership, block, grants, bucketOwner); err != nil {
		return err //nolint:wrapcheck // an S3 error, answered as is
	}
	if ownership == s3api.OwnershipBucketOwnerEnforced {
		return nil
	}
	stored, err := s3api.Encode(s3api.NewAccessControlPolicy(owner, grants))
	if err != nil {
		return err //nolint:wrapcheck // s3api's own error
	}
	return m.storage.PutObjectACL(
		ctx,
		t.Bucket,
		t.Key,
		stored,
	) //nolint:wrapcheck // the storage's own error, answered as is
}
