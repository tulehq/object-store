package s3api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// The grantee types and the group URIs an ACL names ("Access control list
// (ACL) overview", S3 User Guide).
const (
	GranteeCanonicalUser = "CanonicalUser"
	GranteeGroup         = "Group"
	GranteeEmail         = "AmazonCustomerByEmail"

	GroupAllUsers           = "http://acs.amazonaws.com/groups/global/AllUsers"
	GroupAuthenticatedUsers = "http://acs.amazonaws.com/groups/global/AuthenticatedUsers"
	GroupLogDelivery        = "http://acs.amazonaws.com/groups/s3/LogDelivery"
)

// The permissions an ACL grants.
const (
	PermFullControl = "FULL_CONTROL"
	PermRead        = "READ"
	PermWrite       = "WRITE"
	PermReadACP     = "READ_ACP"
	PermWriteACP    = "WRITE_ACP"
)

var permissions = []string{PermFullControl, PermRead, PermWrite, PermReadACP, PermWriteACP}

// xsiNamespace is the namespace of a Grantee's xsi:type attribute.
const xsiNamespace = "http://www.w3.org/2001/XMLSchema-instance"

// CanonicalUserID is the canonical user ID ACLs name account with. AWS
// assigns each account an opaque 64-character hex ID; here it is the hex
// SHA-256 of the account ID, so it is stable and has S3's form.
func CanonicalUserID(account string) string {
	sum := sha256.Sum256([]byte(account))
	return hex.EncodeToString(sum[:])
}

// AccessControlPolicy is the GetBucketAcl and GetObjectAcl response
// document, and the PutBucketAcl and PutObjectAcl request document.
type AccessControlPolicy struct {
	XMLName xml.Name `xml:"AccessControlPolicy"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Owner   *Owner   `xml:"Owner"`
	Grants  []Grant  `xml:"AccessControlList>Grant"`
}

// Grant is one grant of an ACL.
type Grant struct {
	Grantee    Grantee `xml:"Grantee"`
	Permission string  `xml:"Permission"`
}

// Grantee is whom a grant is to. Type is its xsi:type attribute.
type Grantee struct {
	Type         string `xml:"-"`
	ID           string `xml:"ID,omitempty"`
	URI          string `xml:"URI,omitempty"`
	EmailAddress string `xml:"EmailAddress,omitempty"`
}

// MarshalXML writes g with its xsi:type attribute, as S3 does.
func (g Grantee) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = append(start.Attr,
		xml.Attr{Name: xml.Name{Local: "xmlns:xsi"}, Value: xsiNamespace},
		xml.Attr{Name: xml.Name{Local: "xsi:type"}, Value: g.Type})
	type plain Grantee
	return e.EncodeElement(plain(g), start) //nolint:wrapcheck // encoding/xml's own error
}

// UnmarshalXML reads g and its xsi:type attribute, whatever prefix binds
// the namespace.
func (g *Grantee) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	type plain Grantee
	var p plain
	if err := d.DecodeElement(&p, &start); err != nil {
		return err //nolint:wrapcheck // encoding/xml's own error
	}
	for _, a := range start.Attr {
		if a.Name.Local == "type" {
			p.Type = a.Value
		}
	}
	*g = Grantee(p)
	return nil
}

// NewAccessControlPolicy returns the ACL document of a resource owned by
// ownerID with grants.
func NewAccessControlPolicy(ownerID string, grants []Grant) *AccessControlPolicy {
	return &AccessControlPolicy{Xmlns: xmlns, Owner: &Owner{ID: ownerID}, Grants: grants}
}

// OwnerFullControl is the grant of FULL_CONTROL to the canonical user id.
func OwnerFullControl(id string) Grant {
	return Grant{
		Grantee:    Grantee{Type: GranteeCanonicalUser, ID: id},
		Permission: PermFullControl,
	}
}

// ErrAccessControlListNotSupported is S3's 400 AccessControlListNotSupported
// for an ACL a bucket with ACLs disabled refuses: "PUT requests that contain
// other ACLs ... fail and return a 400 error with the error code
// AccessControlListNotSupported" ("Controlling ownership of objects and
// disabling ACLs for your bucket", S3 User Guide), with the message its
// troubleshooting page shows, "The bucket does not allow ACLs".
func ErrAccessControlListNotSupported() *Error {
	return NewError(http.StatusBadRequest, "AccessControlListNotSupported",
		"The bucket does not allow ACLs")
}

// ErrInvalidBucketACLWithObjectOwnership is S3's 400
// InvalidBucketAclWithObjectOwnership, for a CreateBucket or
// PutBucketOwnershipControls that would leave a bucket owner enforced
// bucket with an ACL granting others ("Troubleshooting", Object Ownership,
// S3 User Guide), with the message that page shows.
func ErrInvalidBucketACLWithObjectOwnership() *Error {
	return NewError(http.StatusBadRequest, "InvalidBucketAclWithObjectOwnership",
		"Bucket cannot have ACLs set with ObjectOwnership's BucketOwnerEnforced setting")
}

// grantHeaders are the request headers that grant an ACL permission, by the permission
// each grant header grants.
var grantHeaders = map[string]string{
	"X-Amz-Grant-Full-Control": PermFullControl,
	"X-Amz-Grant-Read":         PermRead,
	"X-Amz-Grant-Write":        PermWrite,
	"X-Amz-Grant-Read-Acp":     PermReadACP,
	"X-Amz-Grant-Write-Acp":    PermWriteACP,
}

// grantHeaderOrder lists grantHeaders in the order grants are built from
// them.
var grantHeaderOrder = []string{
	"X-Amz-Grant-Full-Control", "X-Amz-Grant-Read", "X-Amz-Grant-Write",
	"X-Amz-Grant-Read-Acp", "X-Amz-Grant-Write-Acp",
}

// HasACLHeaders reports whether r sets an ACL with x-amz-acl or an
// x-amz-grant-* header.
func HasACLHeaders(r *http.Request) bool {
	if len(r.Header.Values("X-Amz-Acl")) > 0 {
		return true
	}
	for h := range grantHeaders {
		if len(r.Header.Values(h)) > 0 {
			return true
		}
	}
	return false
}

// CannedACL returns r's x-amz-acl, "" when it has none.
func CannedACL(r *http.Request) string {
	return r.Header.Get("X-Amz-Acl")
}

// ACLOwners are the accounts a canned ACL is expanded for, by canonical
// user ID: the resource's owner and, for an object, its bucket's owner.
type ACLOwners struct {
	Owner       string
	BucketOwner string
}

// HeaderGrants returns the grants r's ACL headers set, for a resource
// owned as owners says, and whether r sets any. object tells an object's
// ACL from a bucket's, which take different canned ACLs.
//
// A canned ACL is expanded as the S3 User Guide's table of canned ACLs
// ("Access control list (ACL) overview") lists it; bucket-owner-read and
// bucket-owner-full-control on a bucket are ignored, as it says, and
// leave the owner FULL_CONTROL. aws-exec-read, which grants Amazon EC2 a
// grantee this server cannot name, is refused with NotImplemented. A grant
// header lists its grantees as id="...", uri="..." or emailAddress="...",
// comma-separated (the PutObjectAcl API reference), each checked with
// checkGrantee; an email grantee,
// which no account here has, is S3's UnresolvableGrantByEmailAddress. A
// request may not set both kinds: "If you use these ACL-specific headers,
// you cannot use x-amz-acl header" (that API reference); the error it gets
// is a choice.
func HeaderGrants(r *http.Request, owners ACLOwners, object bool) ([]Grant, bool, error) {
	canned := r.Header.Values("X-Amz-Acl")
	var grants []Grant
	for _, h := range grantHeaderOrder {
		for _, v := range r.Header.Values(h) {
			gs, err := parseGrantHeader(v, grantHeaders[h])
			if err != nil {
				return nil, true, err
			}
			grants = append(grants, gs...)
		}
	}
	switch {
	case len(canned) > 0 && grants != nil:
		return nil, true, ErrInvalidRequest(
			"Specifying both Canned ACLs and Header Grants is not allowed")
	case len(canned) > 0:
		gs, err := cannedGrants(canned[0], owners, object)
		return gs, true, err
	case grants != nil:
		return grants, true, nil
	}
	return nil, false, nil
}

// cannedGroupGrants are the grants to groups each canned ACL adds to the
// owner's FULL_CONTROL, by name; bucketOnly marks those a bucket takes
// and an object does not.
var cannedGroupGrants = map[string]struct {
	grants     []Grant
	bucketOnly bool
}{
	"private":     {},
	"public-read": {grants: []Grant{groupGrant(GroupAllUsers, PermRead)}},
	"public-read-write": {grants: []Grant{
		groupGrant(GroupAllUsers, PermRead), groupGrant(GroupAllUsers, PermWrite),
	}},
	"authenticated-read": {grants: []Grant{groupGrant(GroupAuthenticatedUsers, PermRead)}},
	"log-delivery-write": {bucketOnly: true, grants: []Grant{
		groupGrant(GroupLogDelivery, PermWrite), groupGrant(GroupLogDelivery, PermReadACP),
	}},
}

func groupGrant(uri, perm string) Grant {
	return Grant{Grantee: Grantee{Type: GranteeGroup, URI: uri}, Permission: perm}
}

// cannedGrants expands the canned ACL name.
func cannedGrants(name string, owners ACLOwners, object bool) ([]Grant, error) {
	owner := OwnerFullControl(owners.Owner)
	if c, ok := cannedGroupGrants[name]; ok && (!object || !c.bucketOnly) {
		return append([]Grant{owner}, c.grants...), nil
	}
	switch name {
	case "bucket-owner-read", "bucket-owner-full-control":
		if !object || owners.BucketOwner == owners.Owner {
			return []Grant{owner}, nil
		}
		perm := PermRead
		if name == "bucket-owner-full-control" {
			perm = PermFullControl
		}
		return []Grant{owner, {
			Grantee:    Grantee{Type: GranteeCanonicalUser, ID: owners.BucketOwner},
			Permission: perm,
		}}, nil
	case "aws-exec-read":
		return nil, ErrNotImplemented("The aws-exec-read canned ACL is not supported.")
	}
	return nil, ErrInvalidArgument("Invalid canned ACL: " + name)
}

// canonicalIDRe is the form of an AWS account's canonical user ID, "a long
// string of characters" whose example in "Access control list (ACL)
// overview" (S3 User Guide) is 64 lowercase hex digits. Requiring that form
// is a choice; S3 documents no rule.
var canonicalIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// groups are the predefined groups a grant may name by URI ("Amazon S3
// predefined groups", that page).
var groups = []string{GroupAllUsers, GroupAuthenticatedUsers, GroupLogDelivery}

// checkGrantee refuses a grantee that is not a canonical user ID of that
// form or a predefined group, with 400 InvalidArgument; S3 documents the
// grantee forms but not its error for others, so the code and messages are
// choices.
func checkGrantee(g Grantee) error {
	switch {
	case g.Type == GranteeCanonicalUser && !canonicalIDRe.MatchString(g.ID):
		return ErrInvalidArgument("Invalid id")
	case g.Type == GranteeGroup && !slices.Contains(groups, g.URI):
		return ErrInvalidArgument("Invalid group uri")
	}
	return nil
}

// parseGrantHeader parses one x-amz-grant-* value granting perm.
func parseGrantHeader(v, perm string) ([]Grant, error) {
	var grants []Grant
	for part := range strings.SplitSeq(v, ",") {
		kind, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if !ok || value == "" {
			return nil, ErrInvalidArgument("Invalid grant header value: " + v)
		}
		g := Grant{Permission: perm}
		switch strings.ToLower(strings.TrimSpace(kind)) {
		case "id":
			g.Grantee = Grantee{Type: GranteeCanonicalUser, ID: value}
		case "uri":
			g.Grantee = Grantee{Type: GranteeGroup, URI: value}
		case "emailaddress":
			return nil, errUnresolvableEmail()
		default:
			return nil, ErrInvalidArgument("Invalid grant header value: " + v)
		}
		if err := checkGrantee(g.Grantee); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, nil
}

// errUnresolvableEmail is S3's UnresolvableGrantByEmailAddress, "The email
// address you provided does not match any account on record." (the S3 API
// Reference, Error).
func errUnresolvableEmail() *Error {
	return NewError(http.StatusBadRequest, "UnresolvableGrantByEmailAddress",
		"The email address you provided does not match any account on record.")
}

// ParseAccessControlPolicy decodes a PutBucketAcl or PutObjectAcl document
// and returns its grants. A grant needs a known permission and a
// CanonicalUser with an ID or a Group with a URI, checked with
// checkGrantee; an email grantee is
// UnresolvableGrantByEmailAddress. Anything else is S3's
// MalformedACLError, "The XML you provided was not well-formed or did not
// validate against our published schema." (the S3 API Reference, Error).
// The document's Owner is not checked against the resource's owner.
func ParseAccessControlPolicy(doc []byte) ([]Grant, error) {
	malformed := NewError(http.StatusBadRequest, "MalformedACLError",
		"The XML you provided was not well-formed or did not validate against our "+
			"published schema.")
	var p AccessControlPolicy
	if err := xml.Unmarshal(doc, &p); err != nil {
		return nil, malformed
	}
	grants := make([]Grant, 0, len(p.Grants))
	for _, g := range p.Grants {
		grantee, err := documentGrantee(g, malformed)
		if err != nil {
			return nil, err
		}
		grants = append(grants, Grant{Grantee: grantee, Permission: g.Permission})
	}
	return grants, nil
}

// documentGrantee returns the grantee of g, a grant of an ACL document,
// with only the fields of its type, or the error it is refused with.
func documentGrantee(g Grant, malformed *Error) (Grantee, error) {
	if !slices.Contains(permissions, g.Permission) {
		return Grantee{}, malformed
	}
	var grantee Grantee
	switch g.Grantee.Type {
	case GranteeCanonicalUser:
		grantee = Grantee{Type: GranteeCanonicalUser, ID: g.Grantee.ID}
	case GranteeGroup:
		grantee = Grantee{Type: GranteeGroup, URI: g.Grantee.URI}
	case GranteeEmail:
		return Grantee{}, errUnresolvableEmail()
	default:
		return Grantee{}, malformed
	}
	if grantee.ID == "" && grantee.URI == "" {
		return Grantee{}, malformed
	}
	return grantee, checkGrantee(grantee)
}

// OnlyOwnerFullControl reports whether grants are exactly FULL_CONTROL for
// ownerID, the only ACL a bucket with ACLs disabled accepts: "your bucket
// accepts only PUT requests that do not specify an ACL or PUT requests
// with bucket owner full control ACLs, for example, the
// bucket-owner-full-control canned ACL or equivalent forms of this ACL
// expressed in XML" ("Controlling ownership of objects and disabling ACLs
// for your bucket", S3 User Guide). The private canned ACL grants the same
// when the bucket owner owns the resource, which it always does then.
func OnlyOwnerFullControl(grants []Grant, ownerID string) bool {
	if len(grants) == 0 {
		return false
	}
	for _, g := range grants {
		if g != OwnerFullControl(ownerID) {
			return false
		}
	}
	return true
}

// PublicGrants reports whether grants grant anything to the AllUsers or
// AuthenticatedUsers group, which is what makes an ACL public: "Amazon S3
// considers a bucket or object ACL public if it grants any permissions to
// members of the predefined AllUsers or AuthenticatedUsers groups."
// ("Blocking public access to your Amazon S3 storage", S3 User Guide).
func PublicGrants(grants []Grant) bool {
	return slices.ContainsFunc(grants, isPublicGrant)
}

func isPublicGrant(g Grant) bool {
	return g.Grantee.Type == GranteeGroup &&
		(g.Grantee.URI == GroupAllUsers || g.Grantee.URI == GroupAuthenticatedUsers)
}

// WithoutPublicGrants returns grants without the public ones, the
// effective ACL under IgnorePublicAcls: "Calls to GetBucketAcl and
// GetObjectAcl always return the effective permissions in place for the
// specified bucket or object" (that page).
func WithoutPublicGrants(grants []Grant) []Grant {
	return slices.DeleteFunc(slices.Clone(grants), isPublicGrant)
}

// GrantsOthers reports whether grants grant anything to anyone but ownerID.
func GrantsOthers(grants []Grant, ownerID string) bool {
	return slices.ContainsFunc(grants, func(g Grant) bool {
		return g.Grantee.Type != GranteeCanonicalUser || g.Grantee.ID != ownerID
	})
}

// RequestGrants returns the ACL a PutBucketAcl or PutObjectAcl request
// sets: its headers' (HeaderGrants), or else those of the document read
// returns (ParseAccessControlPolicy). A request that sets both is refused
// with InvalidRequest, a choice.
func RequestGrants(
	r *http.Request,
	owners ACLOwners,
	object bool,
	read func() ([]byte, error),
) ([]Grant, error) {
	grants, fromHeaders, err := HeaderGrants(r, owners, object)
	if err != nil {
		return nil, err
	}
	doc, err := read()
	if err != nil {
		return nil, err
	}
	if fromHeaders {
		if len(doc) > 0 {
			return nil, ErrInvalidRequest(
				"Specifying both an ACL document and ACL headers is not allowed")
		}
		return grants, nil
	}
	return ParseAccessControlPolicy(doc)
}
