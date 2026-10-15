package dirimpl

import (
	"cmp"
	"context"
	"crypto/md5" //nolint:gosec // S3 ETags are MD5 digests, not a security control
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gasmod/gas"
	"github.com/gasmod/gas/storage/dir"

	"tule.dev/object-store/internal/contracts"
)

// multipartDir is where in-progress multipart uploads are staged, one
// directory per upload ID. It is not a valid bucket name, so it can never
// collide with a bucket.
const multipartDir = ".multipart"

const manifestFile = "upload.json"

// uploadIDRe matches the IDs rand.Text produces. Anything else is rejected
// before it is used as a path component.
var uploadIDRe = regexp.MustCompile(`^[A-Z2-7]{26}$`)

// uploadManifest is what CreateMultipartUpload records for CompleteMultipartUpload.
type uploadManifest struct {
	Initiated time.Time                `json:"initiated"`
	Put       contracts.PutObjectInput `json:"put"`
	Bucket    string                   `json:"bucket"`
	Key       string                   `json:"key"`
}

func uploadDir(id string) string {
	return filepath.Join(multipartDir, id)
}

func partFile(id string, n int) string {
	return filepath.Join(uploadDir(id), fmt.Sprintf("%05d.part", n))
}

func partETagFile(id string, n int) string {
	return filepath.Join(uploadDir(id), fmt.Sprintf("%05d.etag", n))
}

// CreateMultipartUpload starts a multipart upload of bucket/key and returns
// its upload ID. in is applied to the object when the upload completes.
func (s *Service) CreateMultipartUpload(
	ctx context.Context,
	bucket, key string,
	in contracts.PutObjectInput,
) (string, error) {
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return "", err
	}
	if !filepath.IsLocal(key) {
		return "", fmt.Errorf("%s: create upload %q: %w", s.Name(), key, dir.ErrInvalidPath)
	}
	if !supportedKey(key) {
		return "", contracts.ErrUnsupportedKey
	}

	id := rand.Text()
	if err := s.root.MkdirAll(uploadDir(id), 0o755); err != nil {
		return "", fmt.Errorf("%s: create upload %q: %w", s.Name(), key, err)
	}

	manifest, err := json.Marshal(uploadManifest{
		Bucket:    bucket,
		Key:       key,
		Initiated: time.Now().UTC(),
		Put:       in,
	})
	if err != nil {
		return "", fmt.Errorf("%s: create upload %q: %w", s.Name(), key, err)
	}

	name := filepath.Join(uploadDir(id), manifestFile)
	if err = s.root.WriteFile(name, manifest, 0o644); err != nil {
		return "", fmt.Errorf("%s: create upload %q: %w", s.Name(), key, err)
	}

	return id, nil
}

// readManifest reads the manifest of upload id. It fails with fs.ErrNotExist
// when there is no such upload.
func (s *Service) readManifest(id string) (*uploadManifest, error) {
	b, err := s.root.ReadFile(filepath.Join(uploadDir(id), manifestFile))
	if err != nil {
		return nil, fmt.Errorf("%s: read upload %s: %w", s.Name(), id, err)
	}

	var m uploadManifest
	if err = json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: read upload %s: %w", s.Name(), id, err)
	}
	return &m, nil
}

// loadUpload returns the manifest of upload id, or contracts.ErrNoSuchUpload
// when there is no such upload for bucket/key.
func (s *Service) loadUpload(bucket, key, id string) (*uploadManifest, error) {
	if !uploadIDRe.MatchString(id) {
		return nil, contracts.ErrNoSuchUpload
	}

	m, err := s.readManifest(id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, contracts.ErrNoSuchUpload
	}
	if err != nil {
		return nil, err
	}
	if m.Bucket != bucket || m.Key != key {
		return nil, contracts.ErrNoSuchUpload
	}

	return m, nil
}

// UploadPart stores part partNumber of an upload, replacing any earlier
// upload of the same part, and returns its ETag.
func (s *Service) UploadPart(
	_ context.Context,
	bucket, key, uploadID string,
	partNumber int,
	body io.Reader,
) (string, error) {
	if _, err := s.loadUpload(bucket, key, uploadID); err != nil {
		return "", err
	}

	tmp := filepath.Join(uploadDir(uploadID), "tmp-"+rand.Text())
	etag, err := s.writePart(tmp, body)
	if err != nil {
		if rmErr := s.root.Remove(tmp); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
		return "", fmt.Errorf("%s: upload part %d of %s: %w", s.Name(), partNumber, uploadID, err)
	}

	if err = s.root.Rename(tmp, partFile(uploadID, partNumber)); err != nil {
		return "", fmt.Errorf("%s: upload part %d of %s: %w", s.Name(), partNumber, uploadID, err)
	}
	if err = s.root.WriteFile(partETagFile(uploadID, partNumber), []byte(etag), 0o644); err != nil {
		return "", fmt.Errorf("%s: upload part %d of %s: %w", s.Name(), partNumber, uploadID, err)
	}

	return etag, nil
}

// writePart streams body into name and returns the hex MD5 of what it wrote.
//
//nolint:wrapcheck // wrapped at the caller site
func (s *Service) writePart(name string, body io.Reader) (_ string, err error) {
	f, err := s.root.Create(name)
	if err != nil {
		return "", err
	}
	defer func() {
		if cErr := f.Close(); cErr != nil {
			err = errors.Join(err, cErr)
		}
	}()

	h := md5.New() //nolint:gosec // see import
	if _, err = io.Copy(f, io.TeeReader(body, h)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CompleteMultipartUpload assembles the listed parts, in order, into the
// object and removes the upload. The object's ETag follows S3: the MD5 of the
// concatenated binary part digests, suffixed with the part count. The object
// is only written if cond holds; otherwise the upload is kept.
func (s *Service) CompleteMultipartUpload(
	ctx context.Context,
	bucket, key, uploadID string,
	parts []contracts.CompletedPart,
	cond contracts.Precondition,
) (*contracts.ObjectInfo, error) {
	m, err := s.loadUpload(bucket, key, uploadID)
	if err != nil {
		return nil, err
	}

	unlock := s.locks.lock(bucket, key)
	defer unlock()

	if err = s.checkWritable(bucket, key); err != nil {
		return nil, err
	}
	if err = s.checkPrecondition(ctx, bucket, key, cond); err != nil {
		return nil, err
	}

	sizes, etag, err := s.checkParts(uploadID, parts)
	if err != nil {
		return nil, err
	}

	meta := joinMetadata(m.Put.Metadata, m.Put.Headers)
	meta[etagMetaKey] = etag
	joinParts(meta, sizes)

	opts := []gas.StorageOption{gas.InBucket(bucket), gas.WithMetadata(meta)}
	if m.Put.ContentType != "" {
		opts = append(opts, gas.WithContentType(m.Put.ContentType))
	}

	names := make([]string, len(parts))
	for i, p := range parts {
		names[i] = partFile(uploadID, p.PartNumber)
	}
	r := &partsReader{root: s.root, names: names}

	err = s.dir.Upload(ctx, key, r, opts...)
	if cErr := r.Close(); cErr != nil {
		err = errors.Join(err, cErr)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: complete upload %s: %w", s.Name(), uploadID, err)
	}

	if err = s.root.RemoveAll(uploadDir(uploadID)); err != nil {
		return nil, fmt.Errorf("%s: complete upload %s: %w", s.Name(), uploadID, err)
	}

	return s.StatObject(ctx, bucket, key)
}

// checkParts validates a completion's part list against the uploaded parts
// and returns the part sizes and the multipart ETag.
func (s *Service) checkParts(
	uploadID string,
	parts []contracts.CompletedPart,
) ([]int64, string, error) {
	for i := 1; i < len(parts); i++ {
		if parts[i].PartNumber <= parts[i-1].PartNumber {
			return nil, "", contracts.ErrInvalidPartOrder
		}
	}

	sizes := make([]int64, len(parts))
	digests := make([]byte, 0, len(parts)*md5.Size)

	for i, p := range parts {
		size, digest, err := s.checkPart(uploadID, p, i == len(parts)-1)
		if err != nil {
			return nil, "", err
		}
		sizes[i] = size
		digests = append(digests, digest...)
	}

	sum := md5.Sum(digests) //nolint:gosec // see import
	return sizes, hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(len(parts)), nil
}

// checkPart checks one listed part against the uploaded one and returns its
// size and binary MD5. Every part but the last must be at least MinPartSize.
func (s *Service) checkPart(
	uploadID string,
	p contracts.CompletedPart,
	last bool,
) (int64, []byte, error) {
	stored, err := s.root.ReadFile(partETagFile(uploadID, p.PartNumber))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil, contracts.ErrInvalidPart
		}
		return 0, nil, fmt.Errorf(
			"%s: read part %d of %s: %w",
			s.Name(),
			p.PartNumber,
			uploadID,
			err,
		)
	}
	if string(stored) != p.ETag {
		return 0, nil, contracts.ErrInvalidPart
	}

	digest, err := hex.DecodeString(p.ETag)
	if err != nil {
		return 0, nil, contracts.ErrInvalidPart
	}

	info, err := s.root.Stat(partFile(uploadID, p.PartNumber))
	if err != nil {
		return 0, nil, fmt.Errorf(
			"%s: stat part %d of %s: %w",
			s.Name(),
			p.PartNumber,
			uploadID,
			err,
		)
	}
	if !last && info.Size() < contracts.MinPartSize {
		return 0, nil, contracts.ErrEntityTooSmall
	}

	return info.Size(), digest, nil
}

// AbortMultipartUpload discards an upload and its parts.
func (s *Service) AbortMultipartUpload(_ context.Context, bucket, key, uploadID string) error {
	if _, err := s.loadUpload(bucket, key, uploadID); err != nil {
		return err
	}
	if err := s.root.RemoveAll(uploadDir(uploadID)); err != nil {
		return fmt.Errorf("%s: abort upload %s: %w", s.Name(), uploadID, err)
	}
	return nil
}

// ListParts returns the uploaded parts of an upload, in part number order.
func (s *Service) ListParts(
	_ context.Context,
	bucket, key, uploadID string,
) ([]contracts.PartInfo, error) {
	if _, err := s.loadUpload(bucket, key, uploadID); err != nil {
		return nil, err
	}

	entries, err := fs.ReadDir(s.root.FS(), uploadDir(uploadID))
	if err != nil {
		return nil, fmt.Errorf("%s: list parts of %s: %w", s.Name(), uploadID, err)
	}

	var parts []contracts.PartInfo
	for _, e := range entries {
		numStr, ok := strings.CutSuffix(e.Name(), ".part")
		if !ok {
			continue
		}
		n, nErr := strconv.Atoi(numStr)
		if nErr != nil {
			continue
		}

		info, iErr := e.Info()
		if iErr != nil {
			return nil, fmt.Errorf("%s: list parts of %s: %w", s.Name(), uploadID, iErr)
		}
		etag, eErr := s.root.ReadFile(partETagFile(uploadID, n))
		if eErr != nil {
			return nil, fmt.Errorf("%s: list parts of %s: %w", s.Name(), uploadID, eErr)
		}

		parts = append(parts, contracts.PartInfo{
			PartNumber:   n,
			ETag:         string(etag),
			Size:         info.Size(),
			LastModified: info.ModTime(),
		})
	}

	return parts, nil
}

// ListMultipartUploads returns the in-progress uploads into bucket, sorted by
// key and then by initiation time.
func (s *Service) ListMultipartUploads(
	ctx context.Context,
	bucket string,
) ([]contracts.UploadInfo, error) {
	if _, err := s.HeadBucket(ctx, bucket); err != nil {
		return nil, err
	}

	entries, err := fs.ReadDir(s.root.FS(), multipartDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: list uploads: %w", s.Name(), err)
	}

	var uploads []contracts.UploadInfo
	for _, e := range entries {
		if !e.IsDir() || !uploadIDRe.MatchString(e.Name()) {
			continue
		}
		m, rErr := s.readManifest(e.Name())
		if errors.Is(rErr, fs.ErrNotExist) {
			// Completed or aborted while listing.
			continue
		}
		if rErr != nil {
			return nil, rErr
		}
		if m.Bucket != bucket {
			continue
		}
		uploads = append(uploads, contracts.UploadInfo{
			Key:       m.Key,
			UploadID:  e.Name(),
			Initiated: m.Initiated,
		})
	}

	slices.SortFunc(uploads, func(a, b contracts.UploadInfo) int {
		return cmp.Or(
			strings.Compare(a.Key, b.Key),
			a.Initiated.Compare(b.Initiated),
			strings.Compare(a.UploadID, b.UploadID),
		)
	})
	return uploads, nil
}

// partsReader reads the named files back to back, opening each one only when
// the previous one is exhausted, so an upload of thousands of parts does not
// hold thousands of open files.
type partsReader struct {
	root  *os.Root
	cur   *os.File
	names []string
}

func (p *partsReader) Read(b []byte) (int, error) {
	for {
		if p.cur == nil {
			if len(p.names) == 0 {
				return 0, io.EOF
			}
			f, err := p.root.Open(p.names[0])
			if err != nil {
				return 0, fmt.Errorf("open part: %w", err)
			}
			p.cur, p.names = f, p.names[1:]
		}

		n, err := p.cur.Read(b)
		if errors.Is(err, io.EOF) {
			if cErr := p.Close(); cErr != nil {
				return n, cErr
			}
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err //nolint:wrapcheck // passes os.File read errors through unchanged
	}
}

// Close closes the part currently open, if any.
func (p *partsReader) Close() error {
	if p.cur == nil {
		return nil
	}
	err := p.cur.Close()
	p.cur = nil
	if err != nil {
		return fmt.Errorf("close part: %w", err)
	}
	return nil
}
