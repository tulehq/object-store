# genroot

`main.go` writes `../v0.10.1-root.tar.gz`, a storage root written by
object-store v0.10.1, the last layout before bucket versioning.
`TestDataDirFromV0_10_1` (`test/e2e/datadir_test.go`) extracts it and
starts the server on it, to check that a data directory of that version
loads unchanged and is versioned as S3 versions an existing bucket.

The tarball is committed. Run the generator again only to change what the
root holds, from the repository root:

```sh
go run test/e2e/testdata/genroot/main.go
```

It needs git and the `v0.10.1` tag. It:

1. checks out v0.10.1 in a temporary `git worktree` (removed afterwards)
   and builds `./cmd` there;
2. runs that build on an empty root, with the test/test key;
3. writes, with the SDK:
   - in bucket `legacy`:
     - `plain.txt`, a plain object;
     - `headers.txt`, with user metadata and the system headers
       Content-Type, Cache-Control, Content-Disposition, Content-Encoding,
       Content-Language and Expires;
     - `multipart.bin`, a 3-part multipart object, two 5 MiB parts and a
       4 KiB last part, of compressible content;
     - `tagged.txt`, written with `x-amz-tagging`;
     - `retagged.txt`, written with `x-amz-tagging` and retagged by
       PutObjectTagging;
     - a bucket policy;
     - `pending.bin`, a multipart upload with one part, left in progress;
   - in bucket `legacy-writer`, of ObjectWriter ownership and with Block
     Public Access removed, `public.txt` with a public-read ACL;
4. stops the server and tars the root as `root/`, with `fixture.json`
   beside it.

`fixture.json` holds what the test cannot know beforehand: each object's
SHA-256, size, ETag and part sizes, and the in-progress upload's ID and
part. The bodies, headers, tags, ACL and policy the test expects are the
ones `main.go` writes, so a change to one needs the same change in
`datadir_test.go`.
