package object

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"tule.dev/object-store/internal/s3api"
)

// The implicit ACL is the document setObjectACL stores for a canned
// private ACL on an object without one of its own, whose owner is the
// bucket owner, so the storage tells that it changes nothing.
func TestImplicitACLIsCannedPrivate(t *testing.T) {
	const account = "000000000000"
	owner := s3api.CanonicalUserID(account)
	r := httptest.NewRequest("PUT", "/bucket/k?acl", nil)
	r.Header.Set("x-amz-acl", "private")
	grants, err := s3api.RequestGrants(r, s3api.ACLOwners{Owner: owner, BucketOwner: owner},
		true, func() ([]byte, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	private, err := s3api.Encode(s3api.NewAccessControlPolicy(owner, grants))
	if err != nil {
		t.Fatal(err)
	}
	implicit, err := ImplicitACL(account)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(implicit, private) {
		t.Fatalf("implicit ACL\n%s\nprivate\n%s", implicit, private)
	}
}
