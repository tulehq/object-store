//go:build ignore

// Command genroot writes test/e2e/testdata/v0.10.1-root.tar.gz, a storage
// root written by object-store v0.10.1, which TestDataDirFromV0_10_1 loads.
// Run it from the repository root, once, by hand:
//
//	go run test/e2e/testdata/genroot/main.go
//
// It builds v0.10.1 from its tag in a temporary git worktree, runs it on an
// empty root, writes the objects described in README.md with the SDK,
// stops the server and tars the root with fixture.json beside it.
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
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	tag    = "v0.10.1"
	output = "test/e2e/testdata/v0.10.1-root.tar.gz"
	mib    = 1 << 20
)

// Fixture is fixture.json: what the test cannot know before the generator
// runs.
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
	if err = runIn(repo, "git", "worktree", "add", "--detach", src, tag); err != nil {
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
	if err = runIn(src, "go", "build", "-o", bin, "./cmd"); err != nil {
		return err
	}

	root := filepath.Join(tmp, "root")
	if err = os.Mkdir(root, 0o750); err != nil {
		return err
	}
	fx := Fixture{Tag: tag, Commit: commit, Objects: map[string]Object{}}
	if err = serve(bin, root, func(c *s3.Client) error { return populate(c, &fx) }); err != nil {
		return err
	}
	return writeTarball(filepath.Join(repo, output), root, &fx)
}

// serve runs bin on root, calls fn with a client for it, and stops it.
func serve(bin, root string, fn func(c *s3.Client) error) error {
	port, err := freePort()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"TULE_GasEnv=testing",
		"TULE_Storage_RootDir="+root,
		"TULE_Auth_AccessKeyID=test",
		"TULE_Auth_SecretAccessKey=test",
		"TULE_Server_Port="+strconv.Itoa(port),
	)
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
			Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
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

// populate writes the objects README.md lists.
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

// writeTarball writes root, as "root/", and fx, as "fixture.json", to path
// as a gzipped tar.
func writeTarball(path, root string, fx *Fixture) error {
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
