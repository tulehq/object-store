package bucket

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

// maxACLDocument bounds a PutBucketAcl document; S3 documents no limit,
// and this one is a choice.
const maxACLDocument = 64 * 1024

// putBucketOwnershipControls serves PutBucketOwnershipControls (PUT
// /<bucket>?ownershipControls), answering 200 with no body. Setting
// BucketOwnerEnforced on a bucket whose ACL grants anyone but its owner is
// refused with InvalidBucketAclWithObjectOwnership: "if your
// PutBucketOwnershipControls request sets Bucket owner enforced on a
// bucket that has a bucket ACL that grants permissions to others, the
// request fails." ("Troubleshooting", Object Ownership, S3 User Guide).
// A stored ACL is kept, so ACLs enabled again find it, as S3 restores
// "your preexisting bucket and object ACLs".
func (m *Module) putBucketOwnershipControls(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}
	r := ctx.Request()

	caller, err := m.authz.Authorize(r, authz.PutBucketOwnershipControls, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	doc, err := s3api.ReadDocument(r, maxConfigDocument, m.verifier.ReadBody)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	ownership, err := s3api.ParseOwnershipControls(doc)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	stored, err := s3api.Encode(s3api.NewOwnershipControls(ownership))
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	pinned, release, err := caller.Pin(ctx, bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer release()
	if ownership == s3api.OwnershipBucketOwnerEnforced {
		grants, gErr := m.bucketGrants(pinned, bucket)
		if gErr != nil {
			return s3api.WriteError(ctx, target, gErr)
		}
		if s3api.GrantsOthers(grants, m.ownerID()) {
			return s3api.WriteError(ctx, target, s3api.ErrInvalidBucketACLWithObjectOwnership())
		}
	}
	if err = m.storage.PutBucketConfig(pinned, bucket, contracts.ConfigOwnership,
		stored); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}

// getBucketOwnershipControls serves GetBucketOwnershipControls, or 404
// OwnershipControlsNotFoundError for a bucket without them: "A bucket
// doesn't have OwnershipControls settings" when it was created before
// they existed or they were deleted (GetBucketOwnershipControls API
// reference).
func (m *Module) getBucketOwnershipControls(ctx gas.Context) error {
	return m.getConfig(ctx, configKind{
		kind: contracts.ConfigOwnership, notFound: s3api.ErrOwnershipControlsNotFound,
		get: authz.GetBucketOwnershipControls,
	})
}

// deleteBucketOwnershipControls serves DeleteBucketOwnershipControls. The
// bucket then behaves as ObjectWriter (see s3api.Ownership).
func (m *Module) deleteBucketOwnershipControls(ctx gas.Context) error {
	return m.deleteConfig(ctx, configKind{
		kind: contracts.ConfigOwnership, del: authz.DeleteBucketOwnershipControls,
	})
}

// ownerID is the canonical user ID of the account that owns every bucket.
func (m *Module) ownerID() string {
	return s3api.CanonicalUserID(m.authz.Account())
}

// bucketGrants returns the grants of bucket's stored ACL, or FULL_CONTROL
// for its owner, the private ACL, when it has none.
func (m *Module) bucketGrants(ctx context.Context, bucket string) ([]s3api.Grant, error) {
	doc, err := m.storage.GetBucketConfig(ctx, bucket, contracts.ConfigACL)
	if errors.Is(err, contracts.ErrNoSuchBucketConfig) {
		return []s3api.Grant{s3api.OwnerFullControl(m.ownerID())}, nil
	}
	if err != nil {
		return nil, err //nolint:wrapcheck // the storage's own error, answered as is
	}
	var acl s3api.AccessControlPolicy
	if err = xml.Unmarshal(doc, &acl); err != nil {
		return nil, fmt.Errorf("bucket: the stored ACL of bucket %s does not parse: %w",
			bucket, err)
	}
	return acl.Grants, nil
}

// getBucketACL serves GetBucketAcl (GET /<bucket>?acl): the effective ACL,
// owned by the bucket owner. With ACLs disabled it is FULL_CONTROL for the
// owner alone: "requests to read ACLs are still supported and return the
// bucket-owner-full-control ACL with the owner being the account that
// created the bucket" (GetBucketAcl API reference). Otherwise it is the
// stored ACL, without its public grants under IgnorePublicAcls, since
// "Calls to GetBucketAcl and GetObjectAcl always return the effective
// permissions" ("Blocking public access to your Amazon S3 storage").
func (m *Module) getBucketACL(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}

	g, err := m.authz.AuthorizeAndPin(ctx, ctx.Request(), authz.GetBucketACL, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer g.Release()
	pinned := g.Context()
	ownership, err := s3api.Ownership(pinned, m.storage, bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	grants := []s3api.Grant{s3api.OwnerFullControl(m.ownerID())}
	if ownership != s3api.OwnershipBucketOwnerEnforced {
		if grants, err = m.bucketGrants(pinned, bucket); err != nil {
			return s3api.WriteError(ctx, target, err)
		}
		block, bErr := s3api.PublicAccessBlock(pinned, m.storage, bucket)
		if bErr != nil {
			return s3api.WriteError(ctx, target, bErr)
		}
		if block != nil && block.IgnorePublicAcls {
			grants = s3api.WithoutPublicGrants(grants)
		}
	}
	return ctx.XML(http.StatusOK, s3api.NewAccessControlPolicy(m.ownerID(), grants))
}

// putBucketACL serves PutBucketAcl (PUT /<bucket>?acl), answering 200 with
// no body. The ACL is the request's x-amz-acl or x-amz-grant-* headers, or
// else its AccessControlPolicy document; sending both is refused with
// InvalidRequest, a choice. It is checked with s3api.CheckACL: with ACLs
// disabled, "Requests to set ACLs or update ACLs fail and return the
// AccessControlListNotSupported error code" (PutBucketAcl API reference),
// unless it grants the owner FULL_CONTROL alone. An accepted ACL is
// stored, to be read back by GetBucketAcl; it is never evaluated.
func (m *Module) putBucketACL(ctx gas.Context) error {
	bucket := ctx.Param("bucket")
	target := s3api.Target{Bucket: bucket}
	r := ctx.Request()

	caller, err := m.authz.Authorize(r, authz.PutBucketACL, target)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	owner := m.ownerID()
	grants, err := s3api.RequestGrants(
		r,
		s3api.ACLOwners{Owner: owner, BucketOwner: owner},
		false,
		func() ([]byte, error) { return s3api.ReadDocument(r, maxACLDocument, m.verifier.ReadBody) },
	)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	stored, err := s3api.Encode(s3api.NewAccessControlPolicy(owner, grants))
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	pinned, release, err := caller.Pin(ctx, bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	defer release()
	ownership, err := s3api.Ownership(pinned, m.storage, bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	block, err := s3api.PublicAccessBlock(pinned, m.storage, bucket)
	if err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	if err = s3api.CheckACL(ownership, block, grants, owner); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	if err = m.storage.PutBucketConfig(pinned, bucket, contracts.ConfigACL, stored); err != nil {
		return s3api.WriteError(ctx, target, err)
	}
	ctx.ResponseWriter().WriteHeader(http.StatusOK)
	return nil
}
