package authz

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// blocked is a PolicySource whose buckets have the Block Public Access
// document block, or none when it is empty.
type blocked struct {
	buckets
	err   error
	block string
}

func (b blocked) GetBucketConfig(
	_ context.Context,
	_ string,
	kind contracts.BucketConfig,
) ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	if kind != contracts.ConfigPublicAccessBlock || b.block == "" {
		return nil, contracts.ErrNoSuchBucketConfig
	}
	return []byte(b.block), nil
}

const restrictOn = `<PublicAccessBlockConfiguration><RestrictPublicBuckets>true` +
	`</RestrictPublicBuckets></PublicAccessBlockConfiguration>`

// A public bucket policy under RestrictPublicBuckets grants nothing to
// another account's principal, a non-public one still does, and the
// owner's account keeps the public policy's grants.
func TestRestrictPublicBuckets(t *testing.T) {
	toAccount := `{"Effect":"Allow","Principal":{"AWS":"111111111111"},` +
		`"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`
	toAll := `{"Effect":"Allow","Principal":"*","Action":"s3:GetObject",` +
		`"Resource":"arn:aws:s3:::b/*"}`
	public := `{"Version":"2012-10-17","Statement":[` + toAccount + `,` + toAll + `]}`
	private := `{"Version":"2012-10-17","Statement":[` + toAccount + `]}`
	other := request(session(allow("s3:GetObject", "*")), "/b/dir/k.txt")

	a := New("000000000000", blocked{buckets: buckets{"b": public}, block: restrictOn})
	requireDenied(t, a.check(other, GetObject, obj), `User: `+sessionARN+` is not authorized `+
		`to perform: s3:GetObject on resource: "arn:aws:s3:::b/dir/k.txt" because no `+
		`resource-based policy allows the s3:GetObject action`)
	if err := a.check(request(user(), "/b/dir/k.txt"), GetObject, obj); err != nil {
		t.Errorf("the owner's account, granted by the public statement: %v", err)
	}

	a = New("000000000000", blocked{buckets: buckets{"b": private}, block: restrictOn})
	if err := a.check(other, GetObject, obj); err != nil {
		t.Errorf("a non-public policy under RestrictPublicBuckets: %v", err)
	}
	a = New("000000000000", blocked{buckets: buckets{"b": public}})
	if err := a.check(other, GetObject, obj); err != nil {
		t.Errorf("a public policy without RestrictPublicBuckets: %v", err)
	}

	// A setting that cannot be read fails the request; it is not off.
	failed := errors.New("disk on fire")
	a = New("000000000000", blocked{buckets: buckets{"b": public}, err: failed})
	err := a.check(other, GetObject, obj)
	var s3Err *s3api.Error
	if !errors.Is(err, failed) || errors.As(err, &s3Err) {
		t.Errorf("an unreadable setting: %v", err)
	}
}

// A write with ACL or Object Ownership headers is evaluated for the
// actions they add, after its own.
func TestHeadersAddChecks(t *testing.T) {
	a := newAuthorizer(nil)
	put := func(header, value string, docs ...string) error {
		r := request(user(docs...), "/b/dir/k.txt")
		r.Method = http.MethodPut
		r.Header.Set(header, value)
		return a.check(r, PutObject, obj)
	}
	if err := put(
		"X-Amz-Acl",
		"bucket-owner-full-control",
		allow("s3:PutObject", "*"),
	); err != nil {
		t.Errorf("bucket-owner-full-control with s3:PutObject alone: %v", err)
	}
	requireDenied(t, put("X-Amz-Acl", "public-read", allow("s3:PutObject", "*")),
		`User: `+userARN+` is not authorized to perform: s3:PutObjectAcl on resource: `+
			`"arn:aws:s3:::b/dir/k.txt" because no identity-based policy allows the `+
			`s3:PutObjectAcl action`)
	if err := put("X-Amz-Grant-Read", `id="abc"`,
		allow("s3:PutObject", "*"), allow("s3:PutObjectAcl", "*")); err != nil {
		t.Errorf("both allowed: %v", err)
	}

	create := func(header, value string, docs ...string) error {
		r := request(user(docs...), "/n")
		r.Header.Set(header, value)
		return a.check(r, CreateBucket, s3api.Target{Bucket: "n"})
	}
	if err := create("X-Amz-Acl", "private", allow("s3:CreateBucket", "*")); err != nil {
		t.Errorf("CreateBucket private: %v", err)
	}
	requireDenied(t, create("X-Amz-Acl", "public-read", allow("s3:CreateBucket", "*")),
		`User: `+userARN+` is not authorized to perform: s3:PutBucketAcl on resource: `+
			`"arn:aws:s3:::n" because no identity-based policy allows the s3:PutBucketAcl action`)
	requireDenied(t, create("X-Amz-Object-Ownership", "ObjectWriter",
		allow("s3:CreateBucket", "*")), `User: `+userARN+` is not authorized to perform: `+
		`s3:PutBucketOwnershipControls on resource: "arn:aws:s3:::n" because no `+
		`identity-based policy allows the s3:PutBucketOwnershipControls action`)
}

// CopyObject and CreateMultipartUpload are evaluated for an ACL as
// PutObject is: a non-exempt ACL needs s3:PutObjectAcl on the destination.
func TestCopyAndUploadACLsNeedPutObjectACL(t *testing.T) {
	a := newAuthorizer(nil)
	src := s3api.Target{Bucket: "src", Key: "a"}
	writes := allow("s3:*Object", "*")
	for name, check := range map[string]func(*http.Request) error{
		"CopyObject": func(r *http.Request) error { return a.check(r, CopyObject, obj, src) },
		"CreateMultipartUpload": func(r *http.Request) error {
			return a.check(r, CreateMultipartUpload, obj)
		},
	} {
		for _, h := range [][2]string{
			{"X-Amz-Acl", "authenticated-read"}, {"X-Amz-Grant-Read", `id="abc"`},
		} {
			r := request(user(writes), "/b/dir/k.txt")
			r.Header.Set(h[0], h[1])
			requireDenied(t, check(r), `User: `+userARN+` is not authorized to perform: `+
				`s3:PutObjectAcl on resource: "arn:aws:s3:::b/dir/k.txt" because no `+
				`identity-based policy allows the s3:PutObjectAcl action`)
			r = request(user(writes, allow("s3:PutObjectAcl", "*")), "/b/dir/k.txt")
			r.Header.Set(h[0], h[1])
			if err := check(r); err != nil {
				t.Errorf("%s %v with s3:PutObjectAcl: %v", name, h, err)
			}
		}
		r := request(user(writes), "/b/dir/k.txt")
		r.Header.Set("X-Amz-Acl", "bucket-owner-full-control")
		if err := check(r); err != nil {
			t.Errorf("%s bucket-owner-full-control: %v", name, err)
		}
	}
}
