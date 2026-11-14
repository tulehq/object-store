package authz

import (
	"net/http"
	"strings"

	"tule.dev/object-store/internal/s3api"
)

// Operation is an S3 API operation this server routes, by its name in the
// S3 API reference.
type Operation string

// The routed operations. A request the handlers answer with NotImplemented,
// such as a subresource they do not serve, is no operation here and is not
// evaluated.
const (
	ListBuckets             Operation = "ListBuckets"
	CreateBucket            Operation = "CreateBucket"
	HeadBucket              Operation = "HeadBucket"
	DeleteBucket            Operation = "DeleteBucket"
	GetBucketLocation       Operation = "GetBucketLocation"
	GetBucketVersioning     Operation = "GetBucketVersioning"
	ListObjects             Operation = "ListObjects"
	ListObjectsV2           Operation = "ListObjectsV2"
	ListMultipartUploads    Operation = "ListMultipartUploads"
	DeleteObjects           Operation = "DeleteObjects"
	GetObject               Operation = "GetObject"
	HeadObject              Operation = "HeadObject"
	PutObject               Operation = "PutObject"
	CopyObject              Operation = "CopyObject"
	DeleteObject            Operation = "DeleteObject"
	CreateMultipartUpload   Operation = "CreateMultipartUpload"
	UploadPart              Operation = "UploadPart"
	CompleteMultipartUpload Operation = "CompleteMultipartUpload"
	AbortMultipartUpload    Operation = "AbortMultipartUpload"
	ListParts               Operation = "ListParts"
	PutBucketPolicy         Operation = "PutBucketPolicy"
	GetBucketPolicy         Operation = "GetBucketPolicy"
	DeleteBucketPolicy      Operation = "DeleteBucketPolicy"

	PutBucketTagging              Operation = "PutBucketTagging"
	GetBucketTagging              Operation = "GetBucketTagging"
	DeleteBucketTagging           Operation = "DeleteBucketTagging"
	PutBucketCors                 Operation = "PutBucketCors"
	GetBucketCors                 Operation = "GetBucketCors"
	DeleteBucketCors              Operation = "DeleteBucketCors"
	PutPublicAccessBlock          Operation = "PutPublicAccessBlock"
	GetPublicAccessBlock          Operation = "GetPublicAccessBlock"
	DeletePublicAccessBlock       Operation = "DeletePublicAccessBlock"
	GetBucketPolicyStatus         Operation = "GetBucketPolicyStatus"
	PutBucketOwnershipControls    Operation = "PutBucketOwnershipControls"
	GetBucketOwnershipControls    Operation = "GetBucketOwnershipControls"
	DeleteBucketOwnershipControls Operation = "DeleteBucketOwnershipControls"
	PutBucketACL                  Operation = "PutBucketAcl"
	GetBucketACL                  Operation = "GetBucketAcl"
	PutObjectACL                  Operation = "PutObjectAcl"
	GetObjectACL                  Operation = "GetObjectAcl"
	PutBucketEncryption           Operation = "PutBucketEncryption"
	GetBucketEncryption           Operation = "GetBucketEncryption"
	DeleteBucketEncryption        Operation = "DeleteBucketEncryption"
)

// resourceKind is the S3 resource type an action is evaluated on, in the
// reference's ARN formats.
type resourceKind int

const (
	// anyResource is for an action the reference lists no resource type
	// for, evaluated on "*".
	anyResource resourceKind = iota
	// bucketResource is arn:aws:s3:::<bucket>.
	bucketResource
	// objectResource is arn:aws:s3:::<bucket>/<key>.
	objectResource
)

// check is one evaluation an operation needs: an action on one resource.
type check struct {
	action string
	kind   resourceKind
	// implied marks a check the operation implies rather than requests,
	// such as s3:ListBucket for a read that found no object, which takes
	// none of the request's action-specific keys: a GetObject's ?prefix is
	// no listing's prefix.
	implied bool
	// ownerOnly marks an action only principals of the bucket owner's
	// account may perform, whatever the policies allow: the bucket policy
	// operations, which answer another account's allowed principal with
	// 405 MethodNotAllowed.
	ownerOnly bool
}

// operations maps every routed operation to the evaluations it needs, in
// the order they are made: the IAM actions and resource types AWS's Service
// Authorization Reference lists for it (the "Operations" and "Actions" of
// https://servicereference.us-east-1.amazonaws.com/v1/s3/s3.json, and the
// S3 page "Actions, resources, and condition keys for Amazon S3"). Several
// operations are evaluated as an action of another name: HeadBucket and the
// ListObjects versions as s3:ListBucket, HeadObject as s3:GetObject,
// ListMultipartUploads as s3:ListBucketMultipartUploads, ListParts as
// s3:ListMultipartUploadParts, and the multipart writes as s3:PutObject.
//
// An operation on several resources has one check per resource, made in
// turn: CopyObject needs s3:PutObject on the destination and s3:GetObject on
// the source. DeleteObjects is checked once per key it names, each key
// answered on its own. The Delete operations of the bucket configurations
// are evaluated as the configuration's Put action, as the reference lists
// them (DeleteBucketTagging as s3:PutBucketTagging, DeleteBucketCors as
// s3:PutBucketCORS and so on), and PutBucketEncryption, its Get and its
// Delete as s3:PutEncryptionConfiguration and
// s3:GetEncryptionConfiguration.
//
// A write that sets an ACL or an Object Ownership needs more actions,
// which headerChecks adds. The actions the reference lists for features
// this server does not implement (object tagging, object lock, versions)
// are not checked. Requests for their subresources (?tagging on an object,
// ?versionId and the like) are refused with NotImplemented before they are
// evaluated, but the tagging and object lock headers of a write
// (x-amz-tagging, x-amz-object-lock-*) are accepted and ignored, so a
// write carrying them needs only s3:PutObject here, where S3 would also
// need s3:PutObjectTagging and the like.
//
// operations_test.go holds every entry to the reference, and the app's
// tests drive every operation through its handler.
var operations = map[Operation][]check{
	ListBuckets:          {{action: "s3:ListAllMyBuckets", kind: anyResource}},
	CreateBucket:         {{action: "s3:CreateBucket", kind: bucketResource}},
	HeadBucket:           {{action: "s3:ListBucket", kind: bucketResource}},
	DeleteBucket:         {{action: "s3:DeleteBucket", kind: bucketResource}},
	GetBucketLocation:    {{action: "s3:GetBucketLocation", kind: bucketResource}},
	GetBucketVersioning:  {{action: "s3:GetBucketVersioning", kind: bucketResource}},
	ListObjects:          {{action: "s3:ListBucket", kind: bucketResource}},
	ListObjectsV2:        {{action: "s3:ListBucket", kind: bucketResource}},
	ListMultipartUploads: {{action: "s3:ListBucketMultipartUploads", kind: bucketResource}},
	DeleteObjects:        {{action: "s3:DeleteObject", kind: objectResource}},
	GetObject:            {{action: "s3:GetObject", kind: objectResource}},
	HeadObject:           {{action: "s3:GetObject", kind: objectResource}},
	PutObject:            {{action: "s3:PutObject", kind: objectResource}},
	CopyObject: {
		{action: "s3:PutObject", kind: objectResource},
		{action: "s3:GetObject", kind: objectResource},
	},
	DeleteObject:            {{action: "s3:DeleteObject", kind: objectResource}},
	CreateMultipartUpload:   {{action: "s3:PutObject", kind: objectResource}},
	UploadPart:              {{action: "s3:PutObject", kind: objectResource}},
	CompleteMultipartUpload: {{action: "s3:PutObject", kind: objectResource}},
	AbortMultipartUpload:    {{action: "s3:AbortMultipartUpload", kind: objectResource}},
	ListParts:               {{action: "s3:ListMultipartUploadParts", kind: objectResource}},
	PutBucketPolicy: {
		{action: "s3:PutBucketPolicy", kind: bucketResource, ownerOnly: true},
	},
	GetBucketPolicy: {
		{action: "s3:GetBucketPolicy", kind: bucketResource, ownerOnly: true},
	},
	DeleteBucketPolicy: {
		{action: "s3:DeleteBucketPolicy", kind: bucketResource, ownerOnly: true},
	},
	PutBucketTagging:    {{action: "s3:PutBucketTagging", kind: bucketResource}},
	GetBucketTagging:    {{action: "s3:GetBucketTagging", kind: bucketResource}},
	DeleteBucketTagging: {{action: "s3:PutBucketTagging", kind: bucketResource}},
	PutBucketCors:       {{action: "s3:PutBucketCORS", kind: bucketResource}},
	GetBucketCors:       {{action: "s3:GetBucketCORS", kind: bucketResource}},
	DeleteBucketCors:    {{action: "s3:PutBucketCORS", kind: bucketResource}},
	PutPublicAccessBlock: {
		{action: "s3:PutBucketPublicAccessBlock", kind: bucketResource},
	},
	GetPublicAccessBlock: {
		{action: "s3:GetBucketPublicAccessBlock", kind: bucketResource},
	},
	DeletePublicAccessBlock: {
		{action: "s3:PutBucketPublicAccessBlock", kind: bucketResource},
	},
	GetBucketPolicyStatus: {{action: "s3:GetBucketPolicyStatus", kind: bucketResource}},
	PutBucketOwnershipControls: {
		{action: "s3:PutBucketOwnershipControls", kind: bucketResource},
	},
	GetBucketOwnershipControls: {
		{action: "s3:GetBucketOwnershipControls", kind: bucketResource},
	},
	DeleteBucketOwnershipControls: {
		{action: "s3:PutBucketOwnershipControls", kind: bucketResource},
	},
	PutBucketACL: {{action: "s3:PutBucketAcl", kind: bucketResource}},
	GetBucketACL: {{action: "s3:GetBucketAcl", kind: bucketResource}},
	PutObjectACL: {{action: "s3:PutObjectAcl", kind: objectResource}},
	GetObjectACL: {{action: "s3:GetObjectAcl", kind: objectResource}},
	PutBucketEncryption: {
		{action: "s3:PutEncryptionConfiguration", kind: bucketResource},
	},
	GetBucketEncryption: {
		{action: "s3:GetEncryptionConfiguration", kind: bucketResource},
	},
	DeleteBucketEncryption: {
		{action: "s3:PutEncryptionConfiguration", kind: bucketResource},
	},
}

// headerChecks are the checks a write needs on top of its entry for the
// ACL and Object Ownership headers r carries, each on the operation's first
// target:
//
//   - CreateBucket with an ACL other than private needs s3:PutBucketAcl:
//     "if you specify an access control list (ACL) and set it to
//     public-read, public-read-write, authenticated-read, or if you
//     explicitly specify any other custom ACLs, both s3:CreateBucket and
//     s3:PutBucketAcl permissions are required" (CreateBucket API
//     reference, Permissions);
//   - CreateBucket with x-amz-object-ownership needs
//     s3:PutBucketOwnershipControls ("If your CreateBucket request includes
//     the x-amz-object-ownership header, then the
//     s3:PutBucketOwnershipControls permission is required.");
//   - PutObject, CopyObject and CreateMultipartUpload with an ACL that
//     changes the object's need s3:PutObjectAcl ("To successfully change
//     the objects ACL of your PutObject request, you must have the
//     s3:PutObjectAcl", PutObject API reference; the reference also lists
//     s3:PutObjectAcl for CopyObject and CreateMultipartUpload, whose API
//     references say nothing of it). The canned private and
//     bucket-owner-full-control need only the write's own action: they are
//     the ACLs a bucket with ACLs disabled accepts, and the S3 User Guide's
//     "Grant cross-account permissions to upload objects while ensuring
//     that the bucket owner has full control" ("Examples of Amazon S3
//     bucket policies") grants only s3:PutObject, conditioned on
//     s3:x-amz-acl being bucket-owner-full-control. A grant header always
//     needs s3:PutObjectAcl, even one that names the owner alone; that is
//     a choice.
func headerChecks(op Operation, r *http.Request) []check {
	switch op {
	case CreateBucket:
		var checks []check
		if s3api.HasACLHeaders(r) && !onlyPrivate(r) {
			checks = append(checks, check{action: "s3:PutBucketAcl", kind: bucketResource})
		}
		if len(r.Header.Values("X-Amz-Object-Ownership")) > 0 {
			checks = append(checks,
				check{action: "s3:PutBucketOwnershipControls", kind: bucketResource})
		}
		return checks
	case PutObject, CopyObject, CreateMultipartUpload:
		if s3api.HasACLHeaders(r) && !ownerCannedACL(r) {
			return []check{{action: "s3:PutObjectAcl", kind: objectResource}}
		}
	}
	return nil
}

// onlyPrivate reports whether r's only ACL header is x-amz-acl: private.
func onlyPrivate(r *http.Request) bool {
	acl := r.Header.Values("X-Amz-Acl")
	return len(acl) == 1 && acl[0] == "private" && !hasGrantHeaders(r)
}

// ownerCannedACL reports whether r's only ACL header is x-amz-acl: private
// or bucket-owner-full-control.
func ownerCannedACL(r *http.Request) bool {
	acl := r.Header.Values("X-Amz-Acl")
	return len(acl) == 1 && (acl[0] == "private" || acl[0] == "bucket-owner-full-control") &&
		!hasGrantHeaders(r)
}

func hasGrantHeaders(r *http.Request) bool {
	for name := range r.Header {
		if strings.HasPrefix(name, "X-Amz-Grant-") {
			return true
		}
	}
	return false
}

// HeaderActions returns the actions headerChecks can add to op, for tests
// that hold them to the reference.
func HeaderActions(op Operation) []string {
	r := &http.Request{Header: http.Header{}}
	r.Header.Set("X-Amz-Acl", "public-read")
	r.Header.Set("X-Amz-Object-Ownership", "ObjectWriter")
	checks := headerChecks(op, r)
	actions := make([]string, len(checks))
	for i, c := range checks {
		actions[i] = c.action
	}
	return actions
}

// Operations returns every routed operation, for tests that drive each one.
func Operations() []Operation {
	ops := make([]Operation, 0, len(operations))
	for op := range operations {
		ops = append(ops, op)
	}
	return ops
}

// Actions returns the actions op is evaluated as, in order, for tests that
// check a denial names the right one.
func Actions(op Operation) []string {
	checks := operations[op]
	actions := make([]string, len(checks))
	for i, c := range checks {
		actions[i] = c.action
	}
	return actions
}
