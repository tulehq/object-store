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
| `TULE_Auth_AccountID` | `000000000000` | The account the configured key and the buckets belong to: identity's management account (`TULE_Identity_AccountID`), 12 digits. Principals of other accounts are refused ([below](#cross-account-access)) |
| `TULE_Auth_IdentityEndpoint` | unset | The [identity](https://github.com/tulehq/identity) service's base URL, such as `http://localhost:9030`, to also accept the keys it issues and enforce their IAM policies ([below](#permissions)). Must be an absolute `http` or `https` URL; needs identity v0.2.0 or later |
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
is the account's root and may do everything.

With it, every request signed with a key identity issued, an IAM user's
access key or a role session's credentials, is evaluated against that
principal's identity-based policies, as S3 evaluates a request within one
account: its inline policies and the default versions of its attached
managed policies, or a session's role's, as identity reports them on each
request. An explicit `Deny` wins; otherwise the request needs an `Allow`.
The configured key, which is identity's root key too, is never evaluated,
and neither is identity's root principal of `TULE_Auth_AccountID`; a root
principal of any other account is refused like that account's other
principals. A presigned URL is evaluated as
the principal that signed it.

**Breaking change:** up to v0.5.0, any key identity issued could do
everything. Now an IAM user or role with no policy that allows S3 actions is
denied every request. Give such users and roles a policy, such as one
allowing `s3:*` on `*`, or `arn:aws:iam::aws:policy/AdministratorAccess`.

A request no policy allows is answered with `403 AccessDenied` and the
message S3 returns for a same-account request ("Troubleshoot access denied
(403 Forbidden) errors in Amazon S3", S3 User Guide):

```
User: arn:aws:iam::000000000000:user/dev is not authorized to perform: s3:GetObject on resource: "arn:aws:s3:::bkt/key" because no identity-based policy allows the s3:GetObject action
User: arn:aws:iam::000000000000:user/dev is not authorized to perform: s3:GetObject on resource: "arn:aws:s3:::bkt/key" with an explicit deny in an identity-based policy
```

`s3:ListAllMyBuckets`, which has no resource type, names no resource; S3's
message for that case is not documented. A HEAD request's 403 has no body,
as on S3. A key DeleteObjects may not delete gets its own entry, `AccessDenied`
with the message `Access Denied`, as the DeleteObjects API reference's
example shows it.

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

### Cross-account access

A principal of an account other than `TULE_Auth_AccountID`, such as a
session of `OrganizationAccountAccessRole` in a member account, is refused
every request, even one its identity policies allow:

```
User: arn:aws:sts::111111111111:assumed-role/OrganizationAccountAccessRole/s is not authorized to perform: s3:GetObject on resource: "arn:aws:s3:::bkt/key" because no resource-based policy allows the s3:GetObject action
```

On AWS a cross-account request needs a resource-based policy, such as a
bucket policy, granting it as well as an identity policy ("Cross-account
policy evaluation logic", IAM User Guide). This server evaluates no
resource-based policy, so nothing can grant it, and since buckets are not
isolated by account, allowing it would let any member account's
administrator reach every bucket. An explicit deny or a missing allow in the
principal's own policies is still reported as such. The refusal lifts when
resource-based policies are evaluated.

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

A request for something the server does not implement (`501
NotImplemented`) is refused before it is evaluated.

### Condition keys

Global keys: `aws:SourceIp` (the TCP peer's address; `X-Forwarded-For` is
not trusted for it), `aws:CurrentTime`, `aws:EpochTime`,
`aws:SecureTransport`, `aws:RequestedRegion` (always `us-east-1`, the
region every bucket is in; the region a request is signed for is not
checked, so it is not trusted for this), `aws:PrincipalAccount`, `aws:PrincipalType`, `aws:PrincipalArn`
and, for IAM users, `aws:username`. They also fill policy variables such as
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
`REST-HEADER`) and `s3:signatureversion` (every action).

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

- `aws:PrincipalArn` for a role session is the role's ARN without its path
  (`arn:aws:iam::<account>:role/<name>`): the session's ARN carries no path,
  and identity does not report it yet. A policy condition on a role ARN that
  includes a path does not match a session, so a Deny written that way
  (such as one on `arn:aws:iam::*:role/restricted/*`) does not apply to
  sessions yet.
- `aws:userid`, `aws:PrincipalTag/*`, `aws:ResourceTag/*`,
  `s3:ResourceAccount`, `s3:signatureAge`, `s3:TlsVersion` and the other S3
  keys not listed above are not supplied.
- An explicit deny's message leaves out S3's ", with policy ARN: <arn>",
  since identity reports policy documents without their ARNs.
- The actions the reference also lists for features this server does not
  implement are not checked. ACL, tagging and object lock headers on a write
  (`x-amz-acl`, `x-amz-grant-*`, `x-amz-tagging`, `x-amz-object-lock-*`)
  are accepted and ignored, so such a write needs only `s3:PutObject`, where
  S3 also needs `s3:PutObjectAcl`, `s3:PutObjectTagging` and the like.
- Resource-based policies (bucket policies, access point policies, ACLs),
  permissions boundaries, session policies and SCPs are not evaluated.
  Bucket policies and ACLs cannot be set (`501 NotImplemented`), so none is
  stored.
- There is no per-account ownership of buckets: every bucket belongs to
  `TULE_Auth_AccountID`, and other accounts are refused.
- CopyObject checks the destination before the source; the order S3 checks
  them in, which decides the action a doubly denied copy names, is not
  documented. DeleteObjects checks that the bucket exists before it
  evaluates any key, so a caller with no permission learns whether a bucket
  exists; S3's order is not documented either.

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
  GetBucketLocation, GetBucketVersioning (always unversioned).
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
  objects, as far as their identity policies allow. Keys of other accounts
  are refused. There is no per-account isolation, and no resource-based
  policies ([Permissions](#permissions)).
- **Presigned DeleteObjects and CompleteMultipartUpload URLs sign no body**,
  as on S3. Whoever holds such a URL chooses the document it sends, so the
  URL is a bearer grant over every key in the bucket for that operation:
  any keys to delete, or any of the upload's parts to complete.
- **Not implemented** (requests return `501 NotImplemented`): enabling
  versioning, tagging, ACLs, bucket policies, CORS and lifecycle configuration,
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
