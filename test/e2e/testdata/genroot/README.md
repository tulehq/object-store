# genroot

`main.go` writes a storage root written by an earlier release of
object-store, for a test that starts the current server on it:

| Tag | Tarball | Test | Why that release |
|---|---|---|---|
| `v0.10.1` | `../v0.10.1-root.tar.gz` | `TestDataDirFromV0_10_1` (`test/e2e/datadir_test.go`) | the last layout before bucket versioning |
| `v0.12.0` | `../v0.12.0-root.tar.gz` | `TestDataDirFromV0_12_0` (`test/e2e/datadir_v0_12_0_test.go`) | the last release before event notifications |

`TestDataDirFromV0_10_1` checks that a data directory of that version
loads unchanged and is versioned as S3 versions an existing bucket.
`TestDataDirFromV0_12_0` checks that a data directory of that version
loads unchanged, and that the events recorded of the data it holds are
correct.

The tarballs are committed. Run the generator again only to change what a
root holds, from the repository root:

```sh
go run test/e2e/testdata/genroot/main.go                  # v0.10.1
go run test/e2e/testdata/genroot/main.go -tag v0.12.0
```

`-o <path>` writes the tarball elsewhere, to compare a new run with the
committed one without replacing it.

It needs git and the tag. It:

1. checks out the tag in a temporary `git worktree` (removed afterwards)
   and builds `./cmd` there, for v0.12.0 with a `main` of its own beside
   it (`clockMain`), which is `./cmd` with its storage clock set through
   the app's `SetClock` to the system's moved ahead by
   `GENROOT_CLOCK_OFFSET`;
2. runs that build on an empty root, with the test/test key for v0.10.1
   and the e2e root key (`AKIAIOSFODNN7EXAMPLE`) for v0.12.0, the key
   the notification stack serves the root with;
3. writes, with the SDK, what the tag's list below gives;
4. stops the server and tars the root as `root/`, with `fixture.json`
   beside it.

## v0.10.1

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
  Public Access removed, `public.txt` with a public-read ACL.

`fixture.json` holds what the test cannot know beforehand: each object's
SHA-256, size, ETag and part sizes, and the in-progress upload's ID and
part.

## v0.12.0

The root is written by three servers in turn, the second with its
storage clock 2 days ahead and the third 4 days ahead, so that it holds
times days apart. Each version is listed with the label `fixture.json`
records it under.

First server:

- in bucket `history`, of ObjectWriter ownership and with Block Public
  Access removed:
  - while unversioned, `legacy.txt` (`legacy-null`), written with
    `x-amz-tagging` `origin=unversioned`, and `untouched.txt`
    (`untouched-null`): legacy null versions;
  - versioning Enabled;
  - `legacy.txt` again (`legacy-v1`), so its null version is noncurrent;
  - `doc.txt` three times (`doc-v1`, `doc-v2`, `doc-v3`); `doc-v2` tagged
    `stage=review` by PutObjectTagging and `doc-v1` given a public-read
    ACL by PutObjectAcl, each by `versionId`; then a DeleteObject of
    `doc.txt`, which adds a delete marker (`doc-marker`);
  - `archive/log.txt` (`archive-v1`);
- in bucket `paused`:
  - versioning Enabled, `note.txt` (`note-v1`) and `gone.txt`
    (`gone-v1`);
  - versioning Suspended, `note.txt` again, its null version
    (`note-null`), and a DeleteObject of `gone.txt`, which adds a null
    delete marker (`gone-marker`);
  - `big.bin`, a multipart upload with metadata `origin=v0.12.0` and one
    part, left in progress.

Second server, 2 days ahead: `archive/log.txt` again (`archive-v2`),
which v0.12.0 records as the time `archive-v1` became noncurrent.

Third server, 4 days ahead:

- `archive/log.txt` again (`archive-v3`), then a DeleteObject of
  `archive-v2` by `versionId`, so that `archive-v1`'s successor is
  `archive-v3`, 2 days after the time recorded;
- `reports/q3.txt` (`report-v1`);
- the lifecycle configuration of `history`, with the
  `TransitionDefaultMinimumObjectSize` `varies_by_storage_class`, of three
  rules: `expire-reports`, Enabled, which expires `reports/` a day after
  it is written and their noncurrent versions 30 days after;
  `expire-archive`, Enabled, which expires the noncurrent versions of
  `archive/` 4 days after they became noncurrent; and `disabled-all`,
  Disabled, which would expire every key a day after it is written.

`fixture.json` holds what the test cannot know beforehand: each version's
ID, SHA-256, size, ETag and LastModified (a marker has no body), the time
`archive-v1` became noncurrent (`noncurrentSince`), the in-progress
upload's ID and part, and `written`, a time after the last write on the
third server's clock, which the test's storage clock starts at so that
the lifecycle rules act only once the test moves the clock.

The bodies, headers, tags, ACLs, policy and lifecycle rules the tests
expect are the ones `main.go` writes, so a change to one needs the same
change in the test.
