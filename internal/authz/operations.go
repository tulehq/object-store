package authz

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
// answered on its own. The actions the reference lists for features this
// server does not implement (ACLs, tagging, object lock, versions) are not
// checked. Requests for their subresources (?acl, ?tagging, ?versionId and
// the like) are refused with NotImplemented before they are evaluated, but
// the ACL, tagging and object lock headers of a write (x-amz-acl,
// x-amz-grant-*, x-amz-tagging, x-amz-object-lock-*) are accepted and
// ignored, so a write carrying them needs only s3:PutObject here, where S3
// would also need s3:PutObjectAcl, s3:PutObjectTagging and the like.
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
