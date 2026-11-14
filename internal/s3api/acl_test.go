package s3api

import (
	"encoding/xml"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

const (
	ownerID  = "owner"
	bucketID = "bucket-owner"
)

// Canonical user IDs of the form grants take.
var (
	idA = strings.Repeat("a", 64)
	idB = strings.Repeat("b", 64)
)

func aclRequest(kv ...string) *http.Request {
	r := &http.Request{Header: http.Header{}}
	for i := 0; i+1 < len(kv); i += 2 {
		r.Header.Add(kv[i], kv[i+1])
	}
	return r
}

func describe(grants []Grant) string {
	out := make([]string, len(grants))
	for i, g := range grants {
		out[i] = g.Grantee.Type + ":" + g.Grantee.ID + g.Grantee.URI + "=" + g.Permission
	}
	return strings.Join(out, ",")
}

func TestHeaderGrants(t *testing.T) {
	owners := ACLOwners{Owner: ownerID, BucketOwner: bucketID}
	for _, c := range []struct {
		r      *http.Request
		want   string
		object bool
	}{
		{aclRequest("X-Amz-Acl", "private"), "CanonicalUser:owner=FULL_CONTROL", false},
		{aclRequest("X-Amz-Acl", "public-read"), "CanonicalUser:owner=FULL_CONTROL," +
			"Group:" + GroupAllUsers + "=READ", false},
		{aclRequest("X-Amz-Acl", "authenticated-read"), "CanonicalUser:owner=FULL_CONTROL," +
			"Group:" + GroupAuthenticatedUsers + "=READ", true},
		{aclRequest("X-Amz-Acl", "bucket-owner-full-control"), "CanonicalUser:owner=FULL_CONTROL," +
			"CanonicalUser:bucket-owner=FULL_CONTROL", true},
		{aclRequest("X-Amz-Acl", "bucket-owner-read"), "CanonicalUser:owner=FULL_CONTROL," +
			"CanonicalUser:bucket-owner=READ", true},
		// Ignored on a bucket, as the canned ACL table says.
		{
			aclRequest("X-Amz-Acl", "bucket-owner-full-control"), "CanonicalUser:owner=FULL_CONTROL",
			false,
		},
		{aclRequest("X-Amz-Acl", "log-delivery-write"), "CanonicalUser:owner=FULL_CONTROL," +
			"Group:" + GroupLogDelivery + "=WRITE,Group:" + GroupLogDelivery + "=READ_ACP", false},
		{
			aclRequest("X-Amz-Grant-Read", `id="`+idA+`", uri="`+GroupAllUsers+`"`,
				"X-Amz-Grant-Full-Control", `id="`+idB+`"`),
			"CanonicalUser:" + idB + "=FULL_CONTROL,CanonicalUser:" + idA + "=READ,Group:" +
				GroupAllUsers + "=READ",
			true,
		},
	} {
		got, set, err := HeaderGrants(c.r, owners, c.object)
		if err != nil || !set {
			t.Fatalf("%v: %v %v", c.r.Header, set, err)
		}
		if describe(got) != c.want {
			t.Errorf("%v: got %s, want %s", c.r.Header, describe(got), c.want)
		}
	}
	if _, set, err := HeaderGrants(aclRequest(), owners, true); set || err != nil {
		t.Errorf("no ACL headers: %v %v", set, err)
	}
}

func TestHeaderGrantsRefused(t *testing.T) {
	owners := ACLOwners{Owner: ownerID, BucketOwner: bucketID}

	for name, c := range map[string]struct {
		r    *http.Request
		code string
	}{
		"both kinds":  {aclRequest("X-Amz-Acl", "private", "X-Amz-Grant-Read", `id="`+idA+`"`), "InvalidRequest"},
		"a short id":  {aclRequest("X-Amz-Grant-Read", `id="abc"`), "InvalidArgument"},
		"an upper id": {aclRequest("X-Amz-Grant-Read", `id="`+strings.ToUpper(idA)+`"`), "InvalidArgument"},
		"a group uri": {aclRequest("X-Amz-Grant-Read", `uri="http://example.test/g"`), "InvalidArgument"},
		"unknown":     {aclRequest("X-Amz-Acl", "everyone"), "InvalidArgument"},
		"log on obj":  {aclRequest("X-Amz-Acl", "log-delivery-write"), "InvalidArgument"},
		"exec read":   {aclRequest("X-Amz-Acl", "aws-exec-read"), "NotImplemented"},
		"email":       {aclRequest("X-Amz-Grant-Read", `emailAddress="a@example.test"`), "UnresolvableGrantByEmailAddress"},
		"bad grantee": {aclRequest("X-Amz-Grant-Read", `name="a"`), "InvalidArgument"},
		"no value":    {aclRequest("X-Amz-Grant-Read", `id=`), "InvalidArgument"},
	} {
		_, _, err := HeaderGrants(c.r, owners, true)
		var s3Err *Error
		if !errors.As(err, &s3Err) || s3Err.Code != c.code {
			t.Errorf("%s: got %v, want %s", name, err, c.code)
		}
	}
}

func TestAccessControlPolicyRoundTrips(t *testing.T) {
	grants := []Grant{
		OwnerFullControl(idA),
		{Grantee: Grantee{Type: GranteeGroup, URI: GroupAllUsers}, Permission: PermRead},
	}
	b, err := xml.Marshal(NewAccessControlPolicy(idA, grants))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `xmlns:xsi="`+xsiNamespace+`"`) ||
		!strings.Contains(s, `xsi:type="CanonicalUser"`) {
		t.Errorf("grantee attributes missing: %s", s)
	}
	got, err := ParseAccessControlPolicy(b)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, grants) {
		t.Errorf("round trip: %s", describe(got))
	}

	// Another prefix for the namespace reads the same.
	other := `<AccessControlPolicy><AccessControlList><Grant>` +
		`<Grantee xmlns:x="` + xsiNamespace + `" x:type="CanonicalUser"><ID>` + idA + `</ID></Grantee>` +
		`<Permission>WRITE_ACP</Permission></Grant></AccessControlList></AccessControlPolicy>`
	got, err = ParseAccessControlPolicy([]byte(other))
	if err != nil || describe(got) != "CanonicalUser:"+idA+"=WRITE_ACP" {
		t.Errorf("another prefix: %s, %v", describe(got), err)
	}

	for name, doc := range map[string]string{
		"bad permission": `<AccessControlPolicy><AccessControlList><Grant><Grantee ` +
			`xmlns:xsi="` + xsiNamespace + `" xsi:type="CanonicalUser"><ID>i</ID></Grantee>` +
			`<Permission>ALL</Permission></Grant></AccessControlList></AccessControlPolicy>`,
		"no type": `<AccessControlPolicy><AccessControlList><Grant><Grantee><ID>i</ID>` +
			`</Grantee><Permission>READ</Permission></Grant></AccessControlList>` +
			`</AccessControlPolicy>`,
		"no ID": `<AccessControlPolicy><AccessControlList><Grant><Grantee ` +
			`xmlns:xsi="` + xsiNamespace + `" xsi:type="CanonicalUser"></Grantee>` +
			`<Permission>READ</Permission></Grant></AccessControlList></AccessControlPolicy>`,
		"not XML": `<AccessControlPolicy`,
	} {
		_, err = ParseAccessControlPolicy([]byte(doc))
		requireS3Error(t, err, http.StatusBadRequest, "MalformedACLError")
		t.Logf("%s: refused", name)
	}
}

func TestCheckACL(t *testing.T) {
	block := &PublicAccessBlockConfiguration{BlockPublicAcls: true}
	owner := []Grant{OwnerFullControl(bucketID)}
	public := append(slices.Clone(owner),
		Grant{Grantee: Grantee{Type: GranteeGroup, URI: GroupAllUsers}, Permission: PermRead})
	other := []Grant{OwnerFullControl("someone")}

	if err := CheckACL(OwnershipBucketOwnerEnforced, nil, owner, bucketID); err != nil {
		t.Errorf("owner full control, enforced: %v", err)
	}
	for _, g := range [][]Grant{public, other, nil} {
		err := CheckACL(OwnershipBucketOwnerEnforced, nil, g, bucketID)
		requireS3Error(t, err, http.StatusBadRequest, "AccessControlListNotSupported")
	}
	err := CheckACL(OwnershipObjectWriter, block, public, bucketID)
	requireS3Error(t, err, http.StatusForbidden, "AccessDenied")
	for _, c := range []struct {
		block  *PublicAccessBlockConfiguration
		grants []Grant
	}{{nil, public}, {block, other}, {&PublicAccessBlockConfiguration{}, public}} {
		if err = CheckACL(OwnershipBucketOwnerPreferred, c.block, c.grants, bucketID); err != nil {
			t.Errorf("%+v %s: %v", c.block, describe(c.grants), err)
		}
	}
	if got := WithoutPublicGrants(public); describe(got) != describe(owner) {
		t.Errorf("without public grants: %s", describe(got))
	}
	if len(public) != 2 {
		t.Error("WithoutPublicGrants changed its argument")
	}
}

// A document's grantees are checked as grant headers' are.
func TestAccessControlPolicyGrantees(t *testing.T) {
	doc := func(grantee string) []byte {
		return []byte(`<AccessControlPolicy><AccessControlList><Grant><Grantee xmlns:xsi="` +
			xsiNamespace + `" ` + grantee + `</Grantee><Permission>READ</Permission></Grant>` +
			`</AccessControlList></AccessControlPolicy>`)
	}
	for _, g := range []string{
		`xsi:type="CanonicalUser"><ID>abc</ID>`,
		`xsi:type="Group"><URI>http://example.test/g</URI>`,
	} {
		_, err := ParseAccessControlPolicy(doc(g))
		requireS3Error(t, err, http.StatusBadRequest, "InvalidArgument")
	}
	if _, err := ParseAccessControlPolicy(doc(`xsi:type="Group"><URI>` + GroupLogDelivery +
		`</URI>`)); err != nil {
		t.Errorf("a predefined group: %v", err)
	}
}
