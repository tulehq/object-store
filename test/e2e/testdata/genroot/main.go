//go:build ignore

// Command genroot writes a storage root written by an earlier release of
// object-store, which a TestDataDirFrom test loads. Run it from the
// repository root, once per fixture, by hand:
//
//	go run test/e2e/testdata/genroot/main.go                  # v0.10.1
//	go run test/e2e/testdata/genroot/main.go -tag v0.12.0
//
// It builds the release from its tag in a temporary git worktree, runs it
// on an empty root, writes the objects README.md describes for that tag
// with the SDK, stops the server and tars the root with fixture.json
// beside it.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	tag = "v0.10.1"
	mib = 1 << 20
)

// scenario is what genroot writes for a tag.
type scenario struct {
	// populate writes the root, through the servers it starts with run,
	// and returns its fixture.json, given the commit the tag names.
	populate func(run runFunc, commit string) (any, error)
	// output is the tarball, from the repository root.
	output string
	// keyID and secret are the server's key, which c signs with.
	keyID, secret string
	// clock builds the server as clockMain, whose storage clock runs
	// ahead of the system's by the offset run is given, in place of
	// ./cmd, which ignores it.
	clock bool
}

// runFunc runs the server on the root, with its storage clock offset
// ahead, calls fn with a client for it, and stops it.
type runFunc func(offset time.Duration, fn func(c *s3.Client) error) error

// scenarios are the fixtures genroot writes, by tag.
var scenarios = map[string]scenario{
	tag: {
		output: "test/e2e/testdata/v0.10.1-root.tar.gz", keyID: "test", secret: "test",
		populate: func(run runFunc, commit string) (any, error) {
			fx := &Fixture{Tag: tag, Commit: commit, Objects: map[string]Object{}}
			return fx, run(0, func(c *s3.Client) error { return populate(c, fx) })
		},
	},
	// The e2e notification stack's root key, so that the test serves the
	// root with the key that wrote it.
	tagV0_12_0: {
		output: "test/e2e/testdata/v0.12.0-root.tar.gz",
		keyID:  "AKIAIOSFODNN7EXAMPLE", secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		clock: true,
		populate: func(run runFunc, commit string) (any, error) {
			fx := &VersionedFixture{
				Tag: tagV0_12_0, Commit: commit, Versions: map[string]Version{},
				NoncurrentSince: map[string]time.Time{},
			}
			return fx, populateV0_12_0(run, fx)
		},
	},
}

// Fixture is the v0.10.1 fixture.json: what the test cannot know before
// the generator runs.
type Fixture struct {
	Objects map[string]Object `json:"objects"` // by "bucket/key"
	Upload  Upload            `json:"upload"`
	Commit  string            `json:"commit"`
	Tag     string            `json:"tag"`
}

// Object is one object written, as the test reads it back.
type Object struct {
	SHA256 string  `json:"sha256"`
	ETag   string  `json:"etag"`
	Parts  []int64 `json:"parts,omitempty"`
	Size   int64   `json:"size"`
}

// Upload is the multipart upload left in progress, with its one part.
type Upload struct {
	Bucket     string `json:"bucket"`
	Key        string `json:"key"`
	ID         string `json:"id"`
	PartETag   string `json:"partETag"`
	PartSHA256 string `json:"partSHA256"`
	PartSize   int64  `json:"partSize"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	name := flag.String("tag", tag, "the release whose root to write")
	out := flag.String("o", "", "the tarball to write, absolute or from the repository "+
		"root, instead of the fixture's own")
	flag.Parse()
	sc, ok := scenarios[*name]
	if !ok {
		return fmt.Errorf("no fixture for tag %q", *name)
	}
	if *out != "" {
		sc.output = *out
	}
	repo, err := output1("git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "genroot-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	src := filepath.Join(tmp, "src")
	if err = runIn(repo, "git", "worktree", "add", "--detach", src, *name); err != nil {
		return err
	}
	defer func() {
		if rErr := runIn(repo, "git", "worktree", "remove", "--force", src); rErr != nil {
			log.Printf("remove worktree %s: %v", src, rErr)
		}
	}()
	commit, err := output1("git", "-C", src, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	bin := filepath.Join(tmp, "tule-object-store")
	pkg := "./cmd"
	if sc.clock {
		if pkg, err = writeClockMain(src); err != nil {
			return err
		}
	}
	if err = runIn(src, "go", "build", "-o", bin, pkg); err != nil {
		return err
	}

	root := filepath.Join(tmp, "root")
	if err = os.Mkdir(root, 0o750); err != nil {
		return err
	}
	fx, err := sc.populate(func(offset time.Duration, fn func(c *s3.Client) error) error {
		return serve(bin, root, sc, offset, fn)
	}, commit)
	if err != nil {
		return err
	}
	dest := sc.output
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(repo, dest)
	}
	return writeTarball(dest, root, fx)
}

// clockOffsetEnv is the variable clockMain reads its clock offset from.
const clockOffsetEnv = "GENROOT_CLOCK_OFFSET"

// clockMain is the tag's ./cmd with its storage clock set, through the
// app's SetClock, to the system's moved ahead by GENROOT_CLOCK_OFFSET, so
// that the root holds times days apart. Waits keep the system clock.
const clockMain = `// Command genrootclock is ./cmd with its storage clock moved ahead.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"tule.dev/object-store/internal/app"
)

type offsetClock time.Duration

func (c offsetClock) Now() time.Time { return time.Now().Add(time.Duration(c)) }

func (offsetClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func main() {
	offset, err := time.ParseDuration(os.Getenv("` + clockOffsetEnv + `"))
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := app.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	cfg.SetClock(offsetClock(offset))
	a, err := app.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err = a.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
`

// writeClockMain writes clockMain into the checkout src and returns the
// package to build.
func writeClockMain(src string) (string, error) {
	dir := filepath.Join(src, "cmd", "genrootclock")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	return "./cmd/genrootclock", os.WriteFile(filepath.Join(dir, "main.go"), []byte(clockMain), 0o600)
}

// serve runs bin on root with sc's key, and its storage clock offset ahead
// when sc builds clockMain, calls fn with a client for it, and stops it.
func serve(
	bin, root string, sc scenario, offset time.Duration, fn func(c *s3.Client) error,
) error {
	port, err := freePort()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"TULE_GasEnv=testing",
		"TULE_Storage_RootDir="+root,
		"TULE_Auth_AccessKeyID="+sc.keyID,
		"TULE_Auth_SecretAccessKey="+sc.secret,
		"TULE_Server_Port="+strconv.Itoa(port),
	)
	if sc.clock {
		cmd.Env = append(cmd.Env, clockOffsetEnv+"="+offset.String())
	} else if offset != 0 {
		return fmt.Errorf("a clock offset of %v for a build without clockMain", offset)
	}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err = cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	url := "http://127.0.0.1:" + strconv.Itoa(port)
	fnErr := waitReady(url, exited)
	if fnErr == nil {
		fnErr = fn(s3.New(s3.Options{
			Region:       "us-east-1",
			BaseEndpoint: aws.String(url),
			UsePathStyle: true,
			Credentials:  credentials.NewStaticCredentialsProvider(sc.keyID, sc.secret, ""),
		}))
	}
	if sErr := cmd.Process.Signal(syscall.SIGTERM); sErr != nil {
		return errors.Join(fnErr, sErr)
	}
	select {
	case wErr := <-exited:
		if wErr != nil {
			return errors.Join(fnErr, fmt.Errorf("server exit: %w", wErr))
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		return errors.Join(fnErr, errors.New("the server did not stop in time"))
	}
	return fnErr
}

func waitReady(url string, exited chan error) error {
	client := &http.Client{Timeout: time.Second}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-exited:
			return fmt.Errorf("the server exited before it was ready: %v", err)
		default:
		}
		if res, err := client.Get(url + "/_ready"); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("the server was not ready in time")
}

// legacyPolicy is the bucket policy of "legacy", one that grants nothing
// public, so Block Public Access allows it.
const legacyPolicy = `{"Version":"2012-10-17","Statement":[{"Sid":"ReadLegacy",` +
	`"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000000:root"},` +
	`"Action":"s3:GetObject","Resource":"arn:aws:s3:::legacy/*"}]}`

// populate writes the objects README.md lists for v0.10.1.
func populate(c *s3.Client, fx *Fixture) error {
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("legacy")}); err != nil {
		return fmt.Errorf("create legacy: %w", err)
	}
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	puts := []*s3.PutObjectInput{
		{Key: aws.String("plain.txt"), Body: strings.NewReader("a plain legacy object\n")},
		{
			Key:                aws.String("headers.txt"),
			Body:               strings.NewReader("an object with metadata and system headers\n"),
			ContentType:        aws.String("text/plain; charset=utf-8"),
			CacheControl:       aws.String("max-age=60"),
			ContentDisposition: aws.String(`attachment; filename="headers.txt"`),
			ContentEncoding:    aws.String("identity"),
			ContentLanguage:    aws.String("en-US"),
			Expires:            aws.Time(expires),
			Metadata:           map[string]string{"color": "blue", "owner-team": "storage"},
		},
		{
			Key:     aws.String("tagged.txt"),
			Body:    strings.NewReader("an object tagged as it was written\n"),
			Tagging: aws.String("env=dev&team=storage"),
		},
		{
			Key:     aws.String("retagged.txt"),
			Body:    strings.NewReader("an object retagged after it was written\n"),
			Tagging: aws.String("stage=old"),
		},
	}
	for _, in := range puts {
		in.Bucket = aws.String("legacy")
		if err := putRecorded(ctx, c, in, fx); err != nil {
			return err
		}
	}
	if _, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String("legacy"), Key: aws.String("retagged.txt"),
		Tagging: &types.Tagging{TagSet: []types.Tag{
			{Key: aws.String("stage"), Value: aws.String("new")},
			{Key: aws.String("reviewed"), Value: aws.String("yes")},
		}},
	}); err != nil {
		return fmt.Errorf("retag: %w", err)
	}
	if err := multipart(ctx, c, fx); err != nil {
		return err
	}
	if _, err := c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String("legacy"), Policy: aws.String(legacyPolicy),
	}); err != nil {
		return fmt.Errorf("put policy: %w", err)
	}
	if err := publicObject(ctx, c, fx); err != nil {
		return err
	}
	return pendingUpload(ctx, c, fx)
}

// putRecorded puts in, buffering its body, and records it in fx.
func putRecorded(ctx context.Context, c *s3.Client, in *s3.PutObjectInput, fx *Fixture) error {
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return err
	}
	in.Body = bytes.NewReader(body)
	out, err := c.PutObject(ctx, in)
	if err != nil {
		return fmt.Errorf("put %s: %w", aws.ToString(in.Key), err)
	}
	fx.Objects[aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)] = Object{
		SHA256: sha(body), ETag: aws.ToString(out.ETag), Size: int64(len(body)),
	}
	return nil
}

// partBody is part n of multipart.bin, size bytes: 64 KiB blocks, each a
// line naming the part and block followed by filler, so that it compresses
// well and a misplaced block still reads differently.
func partBody(n, size int) []byte {
	const block = 64 << 10
	filler := bytes.Repeat([]byte("legacy multipart filler "), block/24+1)
	var b bytes.Buffer
	for i := 0; b.Len() < size; i++ {
		start := b.Len()
		fmt.Fprintf(&b, "multipart.bin part %d block %d\n", n, i)
		b.Write(filler[:block-(b.Len()-start)])
	}
	return b.Bytes()[:size]
}

// multipart writes legacy/multipart.bin as 3 parts, two of the minimum
// size and a short last one.
func multipart(ctx context.Context, c *s3.Client, fx *Fixture) error {
	bucket, key := aws.String("legacy"), aws.String("multipart.bin")
	up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: bucket, Key: key, ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		return fmt.Errorf("create multipart: %w", err)
	}
	sizes := []int64{5 * mib, 5 * mib, 4096}
	var all []byte
	parts := make([]types.CompletedPart, 0, len(sizes))
	for i, size := range sizes {
		body := partBody(i+1, int(size))
		all = append(all, body...)
		out, pErr := c.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: bucket, Key: key, UploadId: up.UploadId,
			PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(body),
		})
		if pErr != nil {
			return fmt.Errorf("upload part %d: %w", i+1, pErr)
		}
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.ETag})
	}
	done, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: key, UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return fmt.Errorf("complete multipart: %w", err)
	}
	fx.Objects["legacy/multipart.bin"] = Object{
		SHA256: sha(all), ETag: aws.ToString(done.ETag), Size: int64(len(all)), Parts: sizes,
	}
	return nil
}

// publicObject writes legacy-writer/public.txt with a public-read ACL, in
// a bucket of ObjectWriter ownership without Block Public Access.
func publicObject(ctx context.Context, c *s3.Client, fx *Fixture) error {
	bucket := aws.String("legacy-writer")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: bucket, ObjectOwnership: types.ObjectOwnershipObjectWriter,
	}); err != nil {
		return fmt.Errorf("create legacy-writer: %w", err)
	}
	if _, err := c.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{
		Bucket: bucket,
	}); err != nil {
		return fmt.Errorf("delete public access block: %w", err)
	}
	return putRecorded(ctx, c, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("public.txt"),
		Body: strings.NewReader("an object anyone may read\n"),
		ACL:  types.ObjectCannedACLPublicRead,
	}, fx)
}

// pendingUpload starts legacy/pending.bin and uploads its one part,
// leaving it in progress.
func pendingUpload(ctx context.Context, c *s3.Client, fx *Fixture) error {
	bucket, key := aws.String("legacy"), aws.String("pending.bin")
	up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: bucket, Key: key, Metadata: map[string]string{"origin": tag},
	})
	if err != nil {
		return fmt.Errorf("create pending upload: %w", err)
	}
	body := []byte("the one part of an upload v0.10.1 left in progress\n")
	part, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: bucket, Key: key, UploadId: up.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader(body),
	})
	if err != nil {
		return fmt.Errorf("upload pending part: %w", err)
	}
	fx.Upload = Upload{
		Bucket: "legacy", Key: "pending.bin", ID: aws.ToString(up.UploadId),
		PartETag: aws.ToString(part.ETag), PartSHA256: sha(body), PartSize: int64(len(body)),
	}
	return nil
}

// tagV0_12_0 is the last release before event notifications.
const tagV0_12_0 = "v0.12.0"

// VersionedFixture is the v0.12.0 fixture.json: what the test cannot know
// before the generator runs.
type VersionedFixture struct {
	// Written is a time after the last write, on the server's storage
	// clock.
	Written  time.Time          `json:"written"`
	Versions map[string]Version `json:"versions"` // by the label README.md gives
	// NoncurrentSince is when the versions labeled became noncurrent, as
	// the server recorded it: the LastModified of the successor they had
	// then, since deleted.
	NoncurrentSince map[string]time.Time `json:"noncurrentSince"`
	Upload          Upload               `json:"upload"`
	Commit          string               `json:"commit"`
	Tag             string               `json:"tag"`
}

// Version is a version or delete marker written: a marker has no body.
type Version struct {
	LastModified time.Time `json:"lastModified"`
	Bucket       string    `json:"bucket"`
	Key          string    `json:"key"`
	VersionID    string    `json:"versionId"`
	SHA256       string    `json:"sha256,omitempty"`
	ETag         string    `json:"etag,omitempty"`
	Size         int64     `json:"size"`
}

// lifecycleRulesV0_12_0 is the lifecycle configuration of "history": a
// rule that expires reports/ a day after they are written, one that
// expires the noncurrent versions of archive/ 4 days after they became
// noncurrent, and one, Disabled, that would expire everything.
func lifecycleRulesV0_12_0() []types.LifecycleRule {
	return []types.LifecycleRule{
		{
			ID: aws.String("expire-reports"), Status: types.ExpirationStatusEnabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("reports/")},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)},
			NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{
				NoncurrentDays: aws.Int32(30),
			},
		},
		{
			ID: aws.String("expire-archive"), Status: types.ExpirationStatusEnabled,
			Filter: &types.LifecycleRuleFilter{Prefix: aws.String("archive/")},
			NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{
				NoncurrentDays: aws.Int32(4),
			},
		},
		{
			ID: aws.String("disabled-all"), Status: types.ExpirationStatusDisabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("")},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)},
		},
	}
}

// The storage clock offsets of the three servers populateV0_12_0 runs.
const (
	secondRun = 48 * time.Hour
	thirdRun  = 96 * time.Hour
)

// populateV0_12_0 writes the versions README.md lists for v0.12.0, with
// three servers on the root, each with its storage clock two days ahead
// of the one before.
func populateV0_12_0(run runFunc, fx *VersionedFixture) error {
	ctx := context.Background()
	archive := aws.String("archive/log.txt")
	putArchive := func(c *s3.Client, n int) error {
		return putLabeled(ctx, c, fx, fmt.Sprintf("archive-v%d", n), &s3.PutObjectInput{
			Bucket: aws.String("history"), Key: archive,
			Body: strings.NewReader(fmt.Sprintf("archive/log.txt, version %d\n", n)),
		})
	}
	if err := run(0, func(c *s3.Client) error {
		if err := populateHistory(ctx, c, fx); err != nil {
			return err
		}
		if err := putArchive(c, 1); err != nil {
			return err
		}
		return populatePaused(ctx, c, fx)
	}); err != nil {
		return err
	}
	if err := run(secondRun, func(c *s3.Client) error {
		if err := putArchive(c, 2); err != nil {
			return err
		}
		if err := recordLastModified(ctx, c, "history", fx); err != nil {
			return err
		}
		fx.NoncurrentSince["archive-v1"] = fx.Versions["archive-v2"].LastModified
		return nil
	}); err != nil {
		return err
	}
	return run(thirdRun, func(c *s3.Client) error {
		if err := putArchive(c, 3); err != nil {
			return err
		}
		if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String("history"), Key: archive,
			VersionId: aws.String(fx.Versions["archive-v2"].VersionID),
		}); err != nil {
			return fmt.Errorf("delete archive/log.txt version 2: %w", err)
		}
		delete(fx.Versions, "archive-v2")
		if err := expiringReport(ctx, c, fx); err != nil {
			return err
		}
		for _, bucket := range []string{"history", "paused"} {
			if err := recordLastModified(ctx, c, bucket, fx); err != nil {
				return err
			}
		}
		fx.Written = time.Now().Add(thirdRun).UTC()
		return nil
	})
}

// populateHistory writes "history": legacy null versions, then, Enabled,
// versions over one of them, and versions of doc.txt with tags, an ACL
// and a delete marker.
func populateHistory(ctx context.Context, c *s3.Client, fx *VersionedFixture) error {
	bucket := aws.String("history")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: bucket, ObjectOwnership: types.ObjectOwnershipObjectWriter,
	}); err != nil {
		return fmt.Errorf("create history: %w", err)
	}
	if _, err := c.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{
		Bucket: bucket,
	}); err != nil {
		return fmt.Errorf("delete public access block: %w", err)
	}
	if err := putLabeled(ctx, c, fx, "legacy-null", &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("legacy.txt"),
		Body:    strings.NewReader("legacy.txt, written while history was unversioned\n"),
		Tagging: aws.String("origin=unversioned"),
	}); err != nil {
		return err
	}
	if err := putLabeled(ctx, c, fx, "untouched-null", &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("untouched.txt"),
		Body: strings.NewReader("untouched.txt, written while history was unversioned\n"),
	}); err != nil {
		return err
	}
	if err := versioning(ctx, c, "history", types.BucketVersioningStatusEnabled); err != nil {
		return err
	}
	puts := []struct{ label, key, body string }{
		{"legacy-v1", "legacy.txt", "legacy.txt, its first version\n"},
		{"doc-v1", "doc.txt", "doc.txt, version 1\n"},
		{"doc-v2", "doc.txt", "doc.txt, version 2\n"},
		{"doc-v3", "doc.txt", "doc.txt, version 3\n"},
	}
	for _, p := range puts {
		if err := putLabeled(ctx, c, fx, p.label, &s3.PutObjectInput{
			Bucket: bucket, Key: aws.String(p.key), Body: strings.NewReader(p.body),
		}); err != nil {
			return err
		}
	}
	doc := aws.String("doc.txt")
	if _, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: bucket, Key: doc, VersionId: aws.String(fx.Versions["doc-v2"].VersionID),
		Tagging: &types.Tagging{TagSet: []types.Tag{
			{Key: aws.String("stage"), Value: aws.String("review")},
		}},
	}); err != nil {
		return fmt.Errorf("tag doc.txt version 2: %w", err)
	}
	if _, err := c.PutObjectAcl(ctx, &s3.PutObjectAclInput{
		Bucket: bucket, Key: doc, VersionId: aws.String(fx.Versions["doc-v1"].VersionID),
		ACL: types.ObjectCannedACLPublicRead,
	}); err != nil {
		return fmt.Errorf("put the ACL of doc.txt version 1: %w", err)
	}
	del, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: doc})
	if err != nil {
		return fmt.Errorf("delete doc.txt: %w", err)
	}
	if !aws.ToBool(del.DeleteMarker) {
		return errors.New("deleting doc.txt added no delete marker")
	}
	fx.Versions["doc-marker"] = Version{
		Bucket: "history", Key: "doc.txt", VersionID: aws.ToString(del.VersionId),
	}
	return nil
}

// expiringReport writes history/reports/q3.txt, which the lifecycle
// configuration expires, and that configuration.
func expiringReport(ctx context.Context, c *s3.Client, fx *VersionedFixture) error {
	bucket := aws.String("history")
	if err := putLabeled(ctx, c, fx, "report-v1", &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("reports/q3.txt"),
		Body: strings.NewReader("reports/q3.txt, which lifecycle expires\n"),
	}); err != nil {
		return err
	}
	if _, err := c.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket:                 bucket,
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: lifecycleRulesV0_12_0()},
		TransitionDefaultMinimumObjectSize: types.
			TransitionDefaultMinimumObjectSizeVariesByStorageClass,
	}); err != nil {
		return fmt.Errorf("put the lifecycle configuration: %w", err)
	}
	return nil
}

// populatePaused writes "paused": a version of note.txt and of gone.txt
// while Enabled, then, Suspended, note.txt's null version and a delete of
// gone.txt, which makes its null version a delete marker, and big.bin, a
// multipart upload with one part, left in progress.
func populatePaused(ctx context.Context, c *s3.Client, fx *VersionedFixture) error {
	bucket := aws.String("paused")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		return fmt.Errorf("create paused: %w", err)
	}
	if err := versioning(ctx, c, "paused", types.BucketVersioningStatusEnabled); err != nil {
		return err
	}
	if err := putLabeled(ctx, c, fx, "note-v1", &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("note.txt"),
		Body: strings.NewReader("note.txt, written while paused was Enabled\n"),
	}); err != nil {
		return err
	}
	if err := putLabeled(ctx, c, fx, "gone-v1", &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("gone.txt"),
		Body: strings.NewReader("gone.txt, written while paused was Enabled\n"),
	}); err != nil {
		return err
	}
	if err := versioning(ctx, c, "paused", types.BucketVersioningStatusSuspended); err != nil {
		return err
	}
	if err := putLabeled(ctx, c, fx, "note-null", &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("note.txt"),
		Body: strings.NewReader("note.txt, written while paused was Suspended\n"),
	}); err != nil {
		return err
	}
	if id := fx.Versions["note-null"].VersionID; id != "null" {
		return fmt.Errorf("a Suspended write answered version %q", id)
	}
	del, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: aws.String("gone.txt")})
	if err != nil {
		return fmt.Errorf("delete gone.txt: %w", err)
	}
	if !aws.ToBool(del.DeleteMarker) || aws.ToString(del.VersionId) != "null" {
		return fmt.Errorf("a Suspended delete answered marker %v, version %q",
			aws.ToBool(del.DeleteMarker), aws.ToString(del.VersionId))
	}
	fx.Versions["gone-marker"] = Version{Bucket: "paused", Key: "gone.txt", VersionID: "null"}
	key := aws.String("big.bin")
	up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: bucket, Key: key, Metadata: map[string]string{"origin": tagV0_12_0},
	})
	if err != nil {
		return fmt.Errorf("create pending upload: %w", err)
	}
	body := []byte("the one part of an upload v0.12.0 left in progress\n")
	part, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: bucket, Key: key, UploadId: up.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader(body),
	})
	if err != nil {
		return fmt.Errorf("upload pending part: %w", err)
	}
	fx.Upload = Upload{
		Bucket: "paused", Key: "big.bin", ID: aws.ToString(up.UploadId),
		PartETag: aws.ToString(part.ETag), PartSHA256: sha(body), PartSize: int64(len(body)),
	}
	return nil
}

// versioning sets bucket's versioning status.
func versioning(
	ctx context.Context, c *s3.Client, bucket string, status types.BucketVersioningStatus,
) error {
	if _, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: status},
	}); err != nil {
		return fmt.Errorf("set the versioning of %s to %s: %w", bucket, status, err)
	}
	return nil
}

// putLabeled puts in, buffering its body, and records the version it
// wrote in fx as label: "null" when the response names none.
func putLabeled(
	ctx context.Context, c *s3.Client, fx *VersionedFixture, label string, in *s3.PutObjectInput,
) error {
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return err
	}
	in.Body = bytes.NewReader(body)
	out, err := c.PutObject(ctx, in)
	if err != nil {
		return fmt.Errorf("put %s: %w", label, err)
	}
	id := aws.ToString(out.VersionId)
	if id == "" {
		id = "null"
	}
	fx.Versions[label] = Version{
		Bucket: aws.ToString(in.Bucket), Key: aws.ToString(in.Key), VersionID: id,
		SHA256: sha(body), ETag: aws.ToString(out.ETag), Size: int64(len(body)),
	}
	return nil
}

// recordLastModified sets the LastModified of each of fx's versions in
// bucket from its listing, which must list exactly those.
func recordLastModified(ctx context.Context, c *s3.Client, bucket string, fx *VersionedFixture) error {
	out, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	if err != nil {
		return fmt.Errorf("list the versions of %s: %w", bucket, err)
	}
	listed := map[string]time.Time{}
	for _, v := range out.Versions {
		listed[aws.ToString(v.Key)+"?"+aws.ToString(v.VersionId)] = aws.ToTime(v.LastModified)
	}
	for _, m := range out.DeleteMarkers {
		listed[aws.ToString(m.Key)+"?"+aws.ToString(m.VersionId)] = aws.ToTime(m.LastModified)
	}
	labels := slices.Sorted(maps.Keys(fx.Versions))
	n := 0
	for _, label := range labels {
		v := fx.Versions[label]
		if v.Bucket != bucket {
			continue
		}
		at, ok := listed[v.Key+"?"+v.VersionID]
		if !ok {
			return fmt.Errorf("%s does not list %s, version %s", bucket, label, v.VersionID)
		}
		v.LastModified = at.UTC()
		fx.Versions[label] = v
		n++
	}
	if n != len(listed) {
		return fmt.Errorf("%s lists %d versions and markers, %d written", bucket, len(listed), n)
	}
	return nil
}

// writeTarball writes root, as "root/", and fx, as "fixture.json", to path
// as a gzipped tar.
func writeTarball(path, root string, fx any) error {
	meta, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	if err = tw.WriteHeader(&tar.Header{
		Name: "fixture.json", Mode: 0o644, Size: int64(len(meta)), ModTime: time.Now(),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	if _, err = tw.Write(meta); err != nil {
		return err
	}
	if err = filepath.WalkDir(root, func(p string, d fs.DirEntry, wErr error) error {
		if wErr != nil {
			return wErr
		}
		return addEntry(tw, root, p, d)
	}); err != nil {
		return err
	}
	if err = tw.Close(); err != nil {
		return err
	}
	if err = gz.Close(); err != nil {
		return err
	}
	if err = os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return err
	}
	log.Printf("wrote %s (%d bytes)", path, buf.Len())
	return nil
}

// addEntry adds p, under root, to tw as root/<p relative to root>.
func addEntry(tw *tar.Writer, root, p string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Errorf("%s: not a regular file or directory", p)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = filepath.ToSlash(filepath.Join("root", rel))
	if info.IsDir() {
		hdr.Name += "/"
	}
	hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
	if err = tw.WriteHeader(hdr); err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(tw, f)
	return err
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func runIn(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func output1(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
