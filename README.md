# object-store

An S3-compatible object store that keeps objects on the local filesystem. It is
built to stand in for S3 in local development and end-to-end tests: point an
AWS SDK at it and the usual bucket, object, multipart and presigned-URL calls
work.

It is not a production object store. There is one storage root on one machine,
no replication, and listings read the bucket's whole key index on every call.

## Running it

With Docker:

```sh
docker run --rm -p 9000:9000 -v s3data:/data \
  -e TULE_Auth_AccessKeyID=test -e TULE_Auth_SecretAccessKey=test \
  -e TULE_Storage_Buckets=uploads,assets \
  ghcr.io/tulehq/object-store:latest
```

The image is based on Alpine, so `docker exec -it <container> sh` gives you a
shell. It runs as the unprivileged `nonroot` user, and its Docker health check
polls `/_ready`.

From source (Go 1.27):

```sh
make build
TULE_Auth_AccessKeyID=test TULE_Auth_SecretAccessKey=test ./build/tule-object-store
```

It listens on port 9000.

## Configuration

Settings come from environment variables prefixed with `TULE_`, and from a
`.env` file in the working directory (without the prefix).

| Variable | Default | Purpose |
|---|---|---|
| `TULE_Server_Port` | `9000` | Listen port |
| `TULE_Storage_RootDir` | `./data` (`/data` in the image) | Where buckets and objects are stored |
| `TULE_Storage_Buckets` | unset | Comma-separated buckets to create at startup, if missing |
| `TULE_HSTS_MaxAge` | unset | `Strict-Transport-Security` max age; the header is only sent when set |
| `TULE_Server_Domain` | `localhost` | Enables virtual-hosted-style requests to `<bucket>.<domain>` |
| `TULE_Auth_AccessKeyID` | required | The access key clients sign requests with |
| `TULE_Auth_SecretAccessKey` | required | Its secret |
| `TULE_Auth_AccountID` | `000000000000` | The account the configured key and the buckets belong to: identity's management account (`TULE_Identity_AccountID`), 12 digits. Principals of other accounts need a bucket policy granting them ([below](#cross-account-access)) |
| `TULE_Auth_IdentityEndpoint` | unset | The [identity](https://github.com/tulehq/identity) service's base URL, such as `http://localhost:9030`, to also accept the keys it issues and enforce their IAM policies and bucket policies ([below](#permissions)). Must be an absolute `http` or `https` URL; needs identity v0.4.0 or later |
| `TULE_GasEnv` | | `development` logs at trace level, `testing` disables logging |

The server refuses to start without credentials, and every request must be
signed with them (SigV4) or carry a presigned URL made with them. Any web page
you visit can send requests to a server on localhost; signatures are what stop
it from using this one.

Those two settings are all it needs: it runs on its own, with no IAM,
organizations or permissions. To also accept the access keys and STS
session credentials of the identity service, and enforce their IAM
policies, set `TULE_Auth_IdentityEndpoint`. The configured key must then be identity's root
key, since the server authenticates to identity's signing-key endpoint with
it. The root key is still checked locally first. Any other key is looked up
in identity on every request, so a key made inactive or deleted there is
refused on the next one with `403 InvalidAccessKeyId`. A lookup that fails,
such as identity being unreachable or not answering within 10 seconds, fails
the request with `500 InternalError`; it is never treated as an unknown key
or as a valid one.

## Permissions

Without `TULE_Auth_IdentityEndpoint` nothing is evaluated: the configured key
is the account's root and may do everything. Bucket policies can still be
set and read, but no request is evaluated against them.

With it, every request signed with a key identity issued, an IAM user's
access key or a role session's credentials, is evaluated against that
principal's identity-based policies and the bucket policy of the bucket it
acts on, together, as S3 evaluates them (tule.dev/policy's `Authorize`;
"Policy evaluation logic" and "Cross-account policy evaluation logic", IAM
User Guide). The identity-based policies are its inline policies and the
default versions of its attached managed policies, or a session's role's,
as identity reports them on each request. The configured key, which is
identity's root key too, is never evaluated, and neither is identity's root
principal of `TULE_Auth_AccountID`; a root principal of any other account is
evaluated like that account's other principals. A presigned URL is
evaluated as the principal that signed it.

**Breaking change in v0.6.0:** up to v0.5.0, any key identity issued could
do everything. An IAM user or role with no policy that allows S3 actions,
in an identity policy or a bucket policy, is denied every request. Give
such users and roles a policy, such as one allowing `s3:*` on `*`, or
`arn:aws:iam::aws:policy/AdministratorAccess`.

Every bucket belongs to `TULE_Auth_AccountID`. For a principal of that
account:

- an explicit `Deny` in either policy wins;
- otherwise an `Allow` in either allows: a bucket policy statement that
  names the principal itself (its user ARN, its role's ARN, path included,
  for a session, its session ARN, or `"*"`) is enough on its own, without
  any identity policy;
- a bucket policy statement that names only the account (`"000000000000"`
  or `arn:aws:iam::000000000000:root`) delegates to the account, so the
  principal also needs an identity `Allow`.

A request no policy allows is answered with `403 AccessDenied` and the
message S3 returns ("Troubleshoot access denied (403 Forbidden) errors in
Amazon S3", S3 User Guide), ending with the reason tule.dev/policy gives:

```
User: arn:aws:iam::000000000000:user/dev is not authorized to perform: s3:GetObject on resource: "arn:aws:s3:::bkt/key" because no identity-based policy allows the s3:GetObject action
User: arn:aws:iam::000000000000:user/dev is not authorized to perform: s3:GetObject on resource: "arn:aws:s3:::bkt/key" with an explicit deny in an identity-based policy
User: arn:aws:iam::000000000000:user/dev is not authorized to perform: s3:GetObject on resource: "arn:aws:s3:::bkt/key" with an explicit deny in a resource-based policy
User: arn:aws:iam::111111111111:user/partner is not authorized to perform: s3:GetObject on resource: "arn:aws:s3:::bkt/key" because no resource-based policy allows the s3:GetObject action
```

When both kinds deny, the identity-based deny is named. S3's example of
the "no resource-based policy" message names no resource; it is named here
as in the others. `s3:ListAllMyBuckets`, which has no resource type, names
no resource; S3's message for that case is not documented. A HEAD
request's 403 has no body, as on S3. A key DeleteObjects may not delete
gets its own entry, `AccessDenied` with the message `Access Denied`, as the
DeleteObjects API reference's example shows it.

Each operation reads the bucket's policy once, together with the bucket's
generation, a token that is new each time a bucket is created, so a bucket
deleted and created again under the same name is a different bucket. The
operation then acts only on that generation: it holds the bucket from the
moment it acts until it is done, and a bucket deleted, or deleted and
created again, since it was evaluated is answered with `404 NoSuchBucket`.
A grant in a deleted bucket's policy therefore never reaches the bucket
created again under its name, and an upload started in the old bucket does
not complete into, or list in, the new one. DeleteBucket waits for the
operations that hold the bucket, such as an upload in progress. A
PutBucketPolicy or CompleteMultipartUpload document is read before the
bucket is held, so a slow body holds nothing. A bucket policy that changes
while a request is in flight may be evaluated in its previous version, as a
tag value may on AWS. A stored policy that cannot be read or no longer
parses fails every evaluated request on its bucket with `500
InternalError`; it is never treated as absent. A new bucket has no policy,
and a policy left by an interrupted delete is removed when the bucket is
created again.

A GetObject, HeadObject or CopyObject whose object does not exist answers
`404 NoSuchKey` only to a caller allowed `s3:ListBucket` on the bucket, and
`403 AccessDenied` to any other, naming `s3:ListBucket`, so a key's
existence is not revealed to a caller who could not list it (the GetObject
and HeadObject API references, Permissions). The request's own `?prefix`
plays no part in that check.

A bucket name longer than 63 characters is refused with `400
InvalidBucketName`, and a key longer than 1024 bytes, in the path, a
DeleteObjects entry or `x-amz-copy-source`, with `400 KeyTooLongError`
("Your key is too long."), for every caller and before any evaluation: the
cost of evaluating a resource grows with its length.

An `x-amz-expected-bucket-owner` header (or CopyObject's
`x-amz-source-expected-bucket-owner`) other than `TULE_Auth_AccountID` is
refused with `403 AccessDenied` ("Access Denied"), for every caller and
before evaluation, DeleteObjects included, which fails as a whole rather
than per key: "If the account ID that you provide does not match the
actual owner of the bucket, the request fails with the HTTP status code
`403 Forbidden`" (the S3 API Reference). Every value of a repeated header
is checked.

### Cross-account access

A principal of another account, such as a session of
`OrganizationAccountAccessRole` in a member account or a user it created,
needs both an `Allow` in its own identity policies and an `Allow` in the
bucket's policy, of any kind (its ARN, its account, or `"*"`), and no
`Deny` in either. Another account's root needs only the bucket policy's
`Allow`. ListBuckets has no bucket policy and lists the caller's own
account's buckets, as on S3: another account's principal allowed
`s3:ListAllMyBuckets` gets an empty list. CreateBucket needs a bucket
policy, which a bucket that does not exist yet has none of, so other
accounts cannot create buckets here; one that names an existing bucket, if
its policy allows it, is answered with `409 BucketAlreadyExists`, as S3
answers a name another account owns, while the owner's account gets
`409 BucketAlreadyOwnedByYou`.

PutBucketPolicy, GetBucketPolicy and DeleteBucketPolicy answer another
account's principal that both policies allow with `405 MethodNotAllowed`,
as S3 does ("If you have the correct permissions, but you're not using an
identity that belongs to the bucket owner's account, Amazon S3 returns a
405 Method Not Allowed error."; their API references, Permissions).

Objects are owned by the bucket owner whoever writes them, as with S3's
default Object Ownership setting, `BucketOwnerEnforced`, so a bucket policy
applies to every object in its bucket.

### Bucket policies

`PUT /<bucket>?policy` (PutBucketPolicy), `GET /<bucket>?policy`
(GetBucketPolicy) and `DELETE /<bucket>?policy` (DeleteBucketPolicy), with
S3's request and response shapes:

- **PutBucketPolicy** takes the policy document as the body and answers
  `204 No Content`, as the API reference's example does (its response
  syntax says 200). It is evaluated before the body is read. The policy is
  refused with `400`:
  - over 20 KB (20480 bytes as sent; "Bucket policies are limited to 20 KB
    in size.", S3 User Guide): `MalformedPolicy`, "Policy exceeds the
    maximum allowed document size." S3 documents the limit but not its
    error; this one is a choice;
  - empty: `MissingRequestBodyError`, "Request body is empty.";
  - not a valid resource-based policy: `MalformedPolicy` with
    tule.dev/policy's message, such as `Missing required field Principal.`
    or, for a principal IAM does not accept (a group, a partial wildcard, a
    malformed ARN or account ID), `Invalid principal in policy`;
  - a `Resource` or `NotResource` that is not the bucket
    (`arn:aws:s3:::<bucket>`) or an object in it
    (`arn:aws:s3:::<bucket>/...`): `MalformedPolicy`, "Policy has invalid
    resource", S3's message as users report it. Refusing `"*"` and
    wildcards in the bucket name too is a choice;
  - an `Action` or `NotAction` that is not `"*"` or an `s3:` action:
    `MalformedPolicy`, "Policy has invalid action" (a choice; S3 does not
    document it).

  A `Sid` may be any string unique in the policy: S3 documents no rule of
  its own for it. The policy is validated before the bucket is looked up,
  so a bad policy for a missing bucket is `400`, a good one `404
  NoSuchBucket`. `Content-MD5`, the checksum headers and
  `x-amz-confirm-remove-self-bucket-access` are accepted and ignored.
- **GetBucketPolicy** answers `200` with the document exactly as stored, as
  `application/json`, or `404 NoSuchBucketPolicy`.
- **DeleteBucketPolicy** answers `204 No Content`, also when the bucket had
  no policy (S3 does not document that case).

Each takes `x-amz-expected-bucket-owner`. They are evaluated as
`s3:PutBucketPolicy`, `s3:GetBucketPolicy` and `s3:DeleteBucketPolicy` on
the bucket, and the bucket's own policy can grant them within the account.
The configured root key is never evaluated, so it can always change a
policy that denies everyone else. Deleting a bucket deletes its policy.

### Actions and resources

Each operation is evaluated as the actions and resource types AWS's
[Service Authorization Reference for S3](https://docs.aws.amazon.com/service-authorization/latest/reference/list_amazons3.html)
lists for it, on `arn:aws:s3:::<bucket>` or `arn:aws:s3:::<bucket>/<key>`.
The table is `internal/authz/operations.go`; a test checks every entry
against the reference's JSON, and another drives every routed operation
through its handler.

| Operation | Action | Resource |
|---|---|---|
| ListBuckets | `s3:ListAllMyBuckets` | `*` |
| CreateBucket | `s3:CreateBucket` | bucket |
| HeadBucket | `s3:ListBucket` | bucket |
| DeleteBucket | `s3:DeleteBucket` | bucket |
| GetBucketLocation | `s3:GetBucketLocation` | bucket |
| GetBucketVersioning | `s3:GetBucketVersioning` | bucket |
| ListObjects, ListObjectsV2 | `s3:ListBucket` | bucket |
| ListMultipartUploads | `s3:ListBucketMultipartUploads` | bucket |
| GetObject, HeadObject | `s3:GetObject` | object |
| PutObject | `s3:PutObject` | object |
| CopyObject | `s3:PutObject`, then `s3:GetObject` | destination, then source object |
| DeleteObject | `s3:DeleteObject` | object |
| DeleteObjects | `s3:DeleteObject` | each key; a denied key gets its own `AccessDenied` entry in the result |
| CreateMultipartUpload, UploadPart, CompleteMultipartUpload | `s3:PutObject` | object |
| AbortMultipartUpload | `s3:AbortMultipartUpload` | object |
| ListParts | `s3:ListMultipartUploadParts` | object |
| PutBucketPolicy, GetBucketPolicy, DeleteBucketPolicy | `s3:PutBucketPolicy`, `s3:GetBucketPolicy`, `s3:DeleteBucketPolicy` | bucket; another account's allowed principal gets `405 MethodNotAllowed` |

A request for something the server does not implement (`501
NotImplemented`) is refused before it is evaluated.

### Condition keys

Global keys: `aws:SourceIp` (the TCP peer's address; `X-Forwarded-For` is
not trusted for it), `aws:CurrentTime`, `aws:EpochTime`,
`aws:SecureTransport`, `aws:RequestedRegion` (always `us-east-1`, the
region every bucket is in; the region a request is signed for is not
checked, so it is not trusted for this), and the principal's own keys as
identity reports them: `aws:PrincipalArn` (a user's ARN with its path, a
session's role's ARN with the role's path, absent for a session whose role
was deleted, which is refused), `aws:userid`, `aws:PrincipalType`,
`aws:PrincipalAccount`, for IAM users `aws:username`, and
`aws:PrincipalTag/<key>` for each tag of an IAM user or of a session's role
(identity issues no session tags). They also fill policy variables such as
`${aws:username}`.

S3 keys, each for the actions the reference lists it for and only when the
request carries a value: `s3:prefix`, `s3:delimiter` and `s3:max-keys`
(`s3:ListBucket`); `s3:x-amz-acl` (`s3:CreateBucket`, `s3:PutObject`);
`s3:x-amz-server-side-encryption`, `s3:x-amz-storage-class`,
`s3:x-amz-copy-source`, `s3:x-amz-metadata-directive`, `s3:if-match` and
`s3:if-none-match` (`s3:PutObject`); `s3:ObjectCreationOperation`
(`s3:PutObject`): `true` for PutObject, CopyObject and
CompleteMultipartUpload, `false` for CreateMultipartUpload and UploadPart,
as "Enforce conditional writes" (S3 User Guide) uses it; and
`s3:x-amz-content-sha256`, `s3:authType` (`REST-QUERY-STRING` for a
presigned URL, by the same test the signature check makes, else
`REST-HEADER`), `s3:signatureversion` and `s3:ResourceAccount` (every
action; the bucket owner, `TULE_Auth_AccountID`, or, for ListBuckets, the
caller's own account). The reference lists `s3:ResourceAccount`,
`s3:TlsVersion`, `s3:authType`, `s3:signatureAge`, `s3:signatureversion`
and `s3:x-amz-content-sha256` for the bucket policy actions;
`s3:TlsVersion` and `s3:signatureAge` are not supplied.

`s3:x-amz-copy-source` is the source CopyObject reads, `<bucket>/<key>`
decoded, not the header as sent, so a percent-encoded or slash-led header
cannot step around a condition on it. Resources are likewise the bucket and
key the operation acts on, decoded.

Every key has one value, read from the same parse of the request the
operation acts on: a repeated query parameter is its first value for both,
and DeleteObjects' keys are evaluated and deleted from one decoded
document. A request whose value for a key is longer than 4096 bytes is
refused with `400 InvalidArgument`, which bounds the cost of evaluating it.

### Gaps

- AWS converts a user or role ARN in a stored bucket policy to the
  principal's unique ID, so a principal deleted and created again under the
  same name does not inherit the grant. Here policies match by ARN, so a
  recreated principal does inherit it, until identity can resolve unique IDs
  for the services. A session whose role was deleted is refused.
- `aws:PrincipalOrgID` and `aws:PrincipalOrgPaths` are not supplied
  (identity does not carry the organization). A positive condition on them
  never holds, and a negated one (`StringNotEquals` and the like) always
  does, so a `Deny` written with a negated operator on them, such as one
  refusing principals outside the organization, denies every evaluated
  principal.
- Service principals (`{"Service": ...}`), `CanonicalUser` and
  federated-user principals are accepted in a bucket policy but match no
  caller.
- S3 Block Public Access is out of scope: there is no PublicAccessBlock
  API, and no public policy is refused or restricted. GetBucketPolicyStatus
  is not implemented (`501 NotImplemented`).
- ACLs are not supported, so no ACL grant is evaluated.
- On S3 a bucket policy `Deny` also applies to the bucket owner's root user,
  except for the bucket policy operations; here that root is never
  evaluated.
- PutBucketPolicy does not check that a named user or role exists (S3
  refuses a principal it cannot resolve with `Invalid principal in
  policy`), nor that each action applies to the resources its statement
  names.
- `aws:ResourceTag/*`, `s3:signatureAge`, `s3:TlsVersion` and the other S3
  keys not listed above are not supplied.
- An explicit deny's message leaves out S3's ", with policy ARN: <arn>",
  since identity reports policy documents without their ARNs.
- The actions the reference also lists for features this server does not
  implement are not checked. ACL, tagging and object lock headers on a write
  (`x-amz-acl`, `x-amz-grant-*`, `x-amz-tagging`, `x-amz-object-lock-*`)
  are accepted and ignored, so such a write needs only `s3:PutObject`, where
  S3 also needs `s3:PutObjectAcl`, `s3:PutObjectTagging` and the like.
- Access point policies, permissions boundaries, session policies, SCPs and
  RCPs are not evaluated.
- There is no per-account ownership of buckets: every bucket belongs to
  `TULE_Auth_AccountID`.
- CopyObject checks the destination before the source; the order S3 checks
  them in, which decides the action a doubly denied copy names, is not
  documented. DeleteObjects checks that the bucket exists before it
  evaluates any key, so a caller with no permission learns whether a bucket
  exists; S3's order is not documented either.

### Changes in this version

- **Breaking:** `TULE_Auth_IdentityEndpoint` needs identity v0.4.0 or later,
  whose signing-key endpoint reports the principal attributes (version 3);
  an older identity fails every request it verifies with `500`.
- **Behavior:** a principal of another account is no longer refused every
  request: it is allowed what its identity policies and the bucket policy
  both allow, and ListBuckets lists its own account's buckets, none.
  `aws:PrincipalArn` for a role session now carries the role's path, so a
  condition on a path-scoped role ARN applies to sessions.
- **Behavior:** `x-amz-expected-bucket-owner` and
  `x-amz-source-expected-bucket-owner` were ignored; a value other than
  `TULE_Auth_AccountID` is now refused with `403 AccessDenied`.
- **Behavior:** a principal whose `Root` flag and `Account` type disagree,
  which identity never reports, fails with `500 InternalError`, in the
  server's own account too.
- **Behavior:** buckets have generations. A request whose bucket was
  deleted, or deleted and created again, after it was authorized gets `404
  NoSuchBucket`; DeleteBucket waits for operations in progress on the
  bucket; a multipart upload does not survive its bucket being deleted and
  created again. Buckets created by earlier versions keep working, under
  one fixed generation.

## Health checks

`GET /_health` (liveness) and `GET /_ready` (readiness) answer without a
signature. They respond `200` when every service reports healthy or ready and
`503` otherwise, with a JSON object of each service's status. Readiness fails
when the storage root cannot be read.

Startup buckets are created before the server listens, so once either probe
answers, they exist.

## Connecting a client

Use any region; buckets report `us-east-1`. Path-style addressing always
works. Virtual-hosted style works when the client can resolve
`<bucket>.<TULE_Server_Domain>` to the server.

Go (AWS SDK v2):

```go
client := s3.New(s3.Options{
	Region:       "us-east-1",
	BaseEndpoint: aws.String("http://localhost:9000"),
	UsePathStyle: true,
	Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
})
```

JavaScript (AWS SDK v3):

```js
const client = new S3Client({
  region: "us-east-1",
  endpoint: "http://localhost:9000",
  forcePathStyle: true,
  credentials: { accessKeyId: "test", secretAccessKey: "test" },
});
```

Browsers can upload to and download from presigned URLs directly: CORS allows
any origin, and response headers such as `ETag` are exposed.

## Supported operations

- **Buckets:** ListBuckets, CreateBucket, HeadBucket, DeleteBucket,
  GetBucketLocation, GetBucketVersioning (always unversioned),
  PutBucketPolicy, GetBucketPolicy, DeleteBucketPolicy.
- **Listing:** ListObjectsV2 and ListObjects (v1), with prefix, delimiter,
  paging and `encoding-type=url`.
- **Objects:** PutObject (including aws-chunked streaming uploads), GetObject
  (ranges, `partNumber`, `response-*` overrides, conditional GETs), HeadObject,
  CopyObject (`COPY` and `REPLACE` metadata directives), DeleteObject,
  DeleteObjects.
- **Multipart uploads:** CreateMultipartUpload, UploadPart,
  CompleteMultipartUpload, AbortMultipartUpload, ListParts,
  ListMultipartUploads. ETags and the 5 MiB minimum part size follow S3.
- **Conditional writes:** `If-None-Match: *` and `If-Match` on PutObject,
  CopyObject and CompleteMultipartUpload.
- **Presigned URLs** for GET and PUT.
- **Metadata:** `x-amz-meta-*` user metadata and the `Cache-Control`,
  `Content-Disposition`, `Content-Encoding`, `Content-Language` and `Expires`
  headers.

## Differences from S3

- **Keys.** Keys are opaque strings, as on S3: `a` and `a/b` coexist, and
  folder markers and `.` or `..` segments are ordinary keys. A key must be 1
  to 1024 bytes of valid UTF-8 and must not end in a `.` segment (`a/.`).
  A longer key is rejected with `400 KeyTooLongError`, as on S3; other
  invalid keys with `400 InvalidArgument`.
- **Storage layout.** Objects are stored by the gas `storage/dir` backend
  under hashed names, so the storage root is not browsable as files.
  Directories written by earlier versions are not readable by this one, and
  the server refuses to start on one. To keep their objects, serve the old
  volume with the previous image and a fresh one with this image, then copy
  each bucket through a local directory (`aws s3 sync` takes one endpoint per
  run):

  ```sh
  docker run -d --name old -p 9000:9000 -v s3data:/data \
    -e TULE_Auth_AccessKeyID=test -e TULE_Auth_SecretAccessKey=test \
    ghcr.io/tulehq/object-store:0.3.1
  docker run -d --name new -p 9001:9000 -v s3data-new:/data \
    -e TULE_Auth_AccessKeyID=test -e TULE_Auth_SecretAccessKey=test \
    ghcr.io/tulehq/object-store:latest
  export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1
  for b in $(aws s3 ls --endpoint-url http://localhost:9000 | awk '{print $3}'); do
    aws s3 sync "s3://$b" "copy/$b" --endpoint-url http://localhost:9000
    aws s3 mb "s3://$b" --endpoint-url http://localhost:9001
    aws s3 sync "copy/$b" "s3://$b" --endpoint-url http://localhost:9001
  done
  ```

  The copies keep their keys and contents. User metadata, system headers and
  modification times are not copied, and content types are guessed again from
  the file names.
- **Signature verification is request-level.** Header and presigned-URL
  signatures, expiry, clock skew and hex payload hashes are checked; the
  per-chunk signatures of streaming uploads are not, so a DeleteObjects or
  CompleteMultipartUpload document sent with signed chunks
  (`STREAMING-AWS4-*`) is refused with `400 InvalidRequest`. A body is
  decoded as aws-chunked only under a `STREAMING-*` `x-amz-content-sha256`;
  `Content-Encoding: aws-chunked` with any other value is refused with `400
  InvalidRequest`. As on S3, a request is refused when it carries an
  `x-amz-*` header it did not sign (`403 AccessDenied`), when its
  `SignedHeaders` are not lowercase, sorted and unique, when it is signed for
  a service other than `s3`, when its `x-amz-content-sha256` is not a hex
  SHA-256, `UNSIGNED-PAYLOAD` or `STREAMING-*` (`400 InvalidArgument`), and
  when a presigned URL is dated more than 15 minutes ahead (`403
  AccessDenied`, "Request is not yet valid"). The signature must cover
  `Host`. Unlike S3:
  - the credential's region is not checked, so clients may sign for any
    region;
  - the configured key is a long-term one, so a request carrying a session
    token on it is refused with `400 InvalidToken`. Without
    `TULE_Auth_IdentityEndpoint`, a session token on any other key ID gets
    `403 InvalidAccessKeyId`, as that key is unknown. With it, identity's
    session credentials are accepted, and a token that is not the session's
    is refused with `400 InvalidToken` and an expired one with `400
    ExpiredToken`;
  - a DeleteObjects or CompleteMultipartUpload document over 2 MiB is
    refused with `400 MaxMessageLengthExceeded`. One signed with a hex
    `x-amz-content-sha256` is verified before it is acted on; one sent as
    `UNSIGNED-PAYLOAD` or `STREAMING-UNSIGNED-PAYLOAD-TRAILER` is read as
    sent, as on S3.
- **Every key shares all the data.** The root key and every key identity
  issued in `TULE_Auth_AccountID` see and change the same buckets and
  objects, as far as their policies allow. Keys of other accounts reach them
  only through bucket policies. There is no per-account isolation
  ([Permissions](#permissions)).
- **Presigned DeleteObjects and CompleteMultipartUpload URLs sign no body**,
  as on S3. Whoever holds such a URL chooses the document it sends, so the
  URL is a bearer grant over every key in the bucket for that operation:
  any keys to delete, or any of the upload's parts to complete.
- **Not implemented** (requests return `501 NotImplemented`): enabling
  versioning, tagging, ACLs, bucket policy status, public access blocks,
  CORS and lifecycle configuration,
  object lock, UploadPartCopy, and browser POST-policy uploads.
- **Accepted but ignored:** server-side encryption headers,
  `x-amz-copy-source-if-*` conditions, and additional checksums
  (`x-amz-checksum-*`, neither verified nor returned).
- HeadObject ignores conditional request headers.

## Development

```sh
make test       # unit tests
make test-e2e   # end-to-end tests: the app in-process, driven by the AWS SDK
make lint
make image      # build ghcr.io/tulehq/object-store:dev
make smoke      # build the image, run it, and check it serves signed requests
make push VERSION=0.1.0   # build for amd64 and arm64 and push
```

The end-to-end tests include cross-service tests that build and run the
[identity](https://github.com/tulehq/identity) and
[accounts](https://github.com/tulehq/accounts) services from their source,
by default from checkouts beside this one (`../identity` and `../accounts`).
`TULE_E2E_IDENTITY_DIR` and `TULE_E2E_ACCOUNTS_DIR` point them at checkouts
elsewhere. A missing checkout fails the tests; they are never skipped.

## License

[MIT](LICENSE)
