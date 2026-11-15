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

	"tule.dev/object-store/internal/contracts"
)

const manifestFile = "upload.json"

// uploadIDRe matches the IDs rand.Text produces. Anything else is rejected
// before it is used as a path component.
var uploadIDRe = regexp.MustCompile(`^[A-Z2-7]{26}$`)

// uploadManifest is what CreateMultipartUpload records for CompleteMultipartUpload.
type uploadManifest struct {
	Initiated time.Time `json:"initiated"`
	Bucket    string    `json:"bucket"`
	Key       string    `json:"key"`
	// Generation is the bucket's generation when the upload started, so it
	// completes only into that bucket, not one created again under its
	// name. Empty in a manifest written before generations were recorded,
	// which stands for legacyGeneration.
	Generation string                   `json:"generation,omitempty"`
	Put        contracts.PutObjectInput `json:"put"`
}

// generation is the generation of the bucket the upload started in.
func (m *uploadManifest) generation() string {
	if m.Generation == "" {
		return legacyGeneration
	}
	return m.Generation
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
// its upload ID and the time it was initiated. in is applied to the object
// when the upload completes. ctx must be pinned to bucket.
func (s *Service) CreateMultipartUpload(
	ctx context.Context,
	bucket, key string,
	in contracts.PutObjectInput,
) (string, time.Time, error) {
	gen, err := requirePin(ctx, bucket)
	if err != nil {
		return "", time.Time{}, err
	}
	if _, err = s.HeadBucket(ctx, bucket); err != nil {
		return "", time.Time{}, err
	}
	if err = s.checkKey(key); err != nil {
		return "", time.Time{}, err
	}

	initiated := s.now().UTC()
	manifest, err := json.Marshal(uploadManifest{
		Bucket:     bucket,
		Key:        key,
		Generation: gen,
		Initiated:  initiated,
		Put:        in,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%s: create upload %q: %w", s.Name(), key, err)
	}
	id := rand.Text()
	if err = s.makeUpload(id, manifest); err != nil {
		return "", time.Time{}, fmt.Errorf("%s: create upload %q: %w", s.Name(), key, err)
	}
	return id, initiated, nil
}

// uploadTempPrefix starts the name of an upload directory being made,
// which uploadIDRe does not match.
const uploadTempPrefix = ".new-"

// makeUpload makes the directory of upload id with manifest as its
// manifest: under a temporary name, renamed into place once the manifest
// is in it, so an upload directory never lacks its manifest while it is
// made, and one without a manifest is garbage (see removeOrphanUploads).
//
//nolint:wrapcheck // wrapped at the caller site
func (s *Service) makeUpload(id string, manifest []byte) error {
	tmp := filepath.Join(multipartDir, uploadTempPrefix+rand.Text())
	if err := s.root.Mkdir(tmp, 0o755); err != nil {
		return err
	}
	err := s.root.WriteFile(filepath.Join(tmp, manifestFile), manifest, 0o644)
	if err == nil {
		err = s.root.Rename(tmp, uploadDir(id))
	}
	if err != nil {
		return errors.Join(err, s.root.RemoveAll(tmp))
	}
	return nil
}

// staleUploadTemp is how long ago an upload directory being made must
// have been made to be one a create that stopped left: making one takes a
// few file operations.
const staleUploadTemp = time.Hour

// removeOrphanUploads removes the upload directories without a manifest,
// which only an abort or completion that stopped after removing it leaves
// (see removeUpload and makeUpload), and the directories of uploads being
// made that are older than staleUploadTemp, which only a create that
// stopped leaves. Parts are written only after their upload's manifest is
// read, so a directory without one never becomes an upload again.
func (s *Service) removeOrphanUploads() error {
	entries, err := fs.ReadDir(s.root.FS(), multipartDir)
	if err != nil {
		return fmt.Errorf("%s: list uploads: %w", s.Name(), err)
	}
	for _, e := range entries {
		orphan, oErr := s.isOrphanUpload(e)
		if oErr != nil {
			return oErr
		}
		if !orphan {
			continue
		}
		if rErr := s.root.RemoveAll(filepath.Join(multipartDir, e.Name())); rErr != nil {
			return fmt.Errorf("%s: remove abandoned upload %s: %w", s.Name(), e.Name(), rErr)
		}
	}
	return nil
}

// isOrphanUpload reports whether e, an entry of multipartDir, is one
// removeOrphanUploads removes. An entry removed meanwhile is none.
func (s *Service) isOrphanUpload(e fs.DirEntry) (bool, error) {
	switch {
	case !e.IsDir():
		return false, nil
	case uploadIDRe.MatchString(e.Name()):
		_, err := s.root.Stat(filepath.Join(uploadDir(e.Name()), manifestFile))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("%s: stat upload %s: %w", s.Name(), e.Name(), err)
		}
		return err != nil, nil
	case strings.HasPrefix(e.Name(), uploadTempPrefix):
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("%s: stat upload %s: %w", s.Name(), e.Name(), err)
		}
		// File times are the system's, not the store's clock.
		return info.ModTime().Before(time.Now().Add(-staleUploadTemp)), nil
	}
	return false, nil
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
// when there is no such upload for bucket/key in the generation ctx pins
// bucket to.
func (s *Service) loadUpload(ctx context.Context, bucket, key, id string) (*uploadManifest, error) {
	gen, err := requirePin(ctx, bucket)
	if err != nil {
		return nil, err
	}
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
	if m.Bucket != bucket || m.Key != key || m.generation() != gen {
		return nil, contracts.ErrNoSuchUpload
	}

	return m, nil
}

// UploadPart stores part partNumber of an upload, replacing any earlier
// upload of the same part, and returns its ETag.
func (s *Service) UploadPart(
	ctx context.Context,
	bucket, key, uploadID string,
	partNumber int,
	body io.Reader,
) (string, map[string]string, error) {
	m, err := s.loadUpload(ctx, bucket, key, uploadID)
	if err != nil {
		return "", nil, err
	}
	etag, err := s.storePart(uploadID, partNumber, body)
	if err != nil {
		return "", nil, err
	}
	return etag, m.Put.Headers, nil
}

// storePart streams body into part partNumber of upload uploadID,
// replacing any earlier upload of the part, and returns its ETag.
func (s *Service) storePart(uploadID string, partNumber int, body io.Reader) (string, error) {
	tmp := filepath.Join(uploadDir(uploadID), "tmp-"+rand.Text())
	etag, err := s.writePart(tmp, body)
	if err != nil {
		if rmErr := s.root.Remove(tmp); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
		return "", fmt.Errorf(
			"%s: upload part %d of %s: %w",
			s.Name(),
			partNumber,
			uploadID,
			err,
		)
	}

	if err = s.root.Rename(tmp, partFile(uploadID, partNumber)); err != nil {
		return "", fmt.Errorf(
			"%s: upload part %d of %s: %w",
			s.Name(),
			partNumber,
			uploadID,
			err,
		)
	}
	if err = s.root.WriteFile(partETagFile(uploadID, partNumber), []byte(etag), 0o644); err != nil {
		return "", fmt.Errorf(
			"%s: upload part %d of %s: %w",
			s.Name(),
			partNumber,
			uploadID,
			err,
		)
	}
	return etag, nil
}

// UploadPartCopy stores the bytes of in's source object in in.Range, or
// all of them, as part in.PartNumber of an upload, replacing any earlier
// upload of the part, if in.SourceCheck passes and in.SourceCondition
// holds of the source as it is read. The bytes are streamed from the
// source as it was opened, so a source written over meanwhile is copied
// whole as it was. A range must be within the source, which must be larger
// than MinRangeCopySource, and the copy at most MaxCopyPartSize. It returns
// the part with the source's version ID. ctx must be pinned to both
// buckets.
func (s *Service) UploadPartCopy(
	ctx context.Context,
	in contracts.UploadPartCopyInput,
) (_ *contracts.UploadPartCopyOutput, err error) {
	m, err := s.loadUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return nil, err
	}
	src, err := s.GetObject(ctx, contracts.ObjectRef{
		Bucket: in.SrcBucket, Key: in.SrcKey, Version: in.SrcVersion,
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		if cErr := src.Body.Close(); cErr != nil {
			err = errors.Join(err, fmt.Errorf("%s: copy %q: %w", s.Name(), in.SrcKey, cErr))
		}
	}()
	if err = checkSource(in.SourceCheck, in.SourceCondition, &src.ObjectInfo); err != nil {
		return nil, err
	}
	first, length, err := copySpan(in.Range, src.Size)
	if err != nil {
		return nil, err
	}
	// The object body seeks within the object bytes (see objectBody).
	rs, ok := src.Body.(io.Seeker)
	if !ok {
		return nil, fmt.Errorf("%s: copy %q: body %T is not seekable",
			s.Name(), in.SrcKey, src.Body)
	}
	if _, err = rs.Seek(first, io.SeekStart); err != nil {
		return nil, fmt.Errorf("%s: copy %q: %w", s.Name(), in.SrcKey, err)
	}
	part, err := s.copyPart(in, io.LimitReader(src.Body, length), length)
	if err != nil {
		return nil, err
	}
	return &contracts.UploadPartCopyOutput{
		Part: *part, Headers: m.Put.Headers, SourceVersionID: src.VersionID,
	}, nil
}

// copyPart stores the length bytes body yields as part in.PartNumber of
// upload in.UploadID, failing if body ends early.
func (s *Service) copyPart(
	in contracts.UploadPartCopyInput,
	body io.Reader,
	length int64,
) (*contracts.PartInfo, error) {
	cr := &countingReader{r: body}
	etag, err := s.storePart(in.UploadID, in.PartNumber, cr)
	if err != nil {
		return nil, err
	}
	if cr.n != length {
		return nil, fmt.Errorf("%s: copy %q: read %d of %d bytes: %w",
			s.Name(), in.SrcKey, cr.n, length, io.ErrUnexpectedEOF)
	}
	stat, err := s.root.Stat(partFile(in.UploadID, in.PartNumber))
	if err != nil {
		return nil, fmt.Errorf("%s: stat part %d of %s: %w",
			s.Name(), in.PartNumber, in.UploadID, err)
	}
	return &contracts.PartInfo{
		PartNumber: in.PartNumber, ETag: etag, Size: length, LastModified: stat.ModTime(),
	}, nil
}

// copySpan returns the offset and length of the bytes of a source of size
// bytes that rng selects, all of them when it is nil:
// contracts.ErrRangeCopySourceTooSmall for a range of a source no larger
// than MinRangeCopySource, contracts.ErrInvalidCopyRange for one that does
// not end within the source, and contracts.ErrEntityTooLarge for more than
// MaxCopyPartSize bytes.
func copySpan(rng *contracts.CopyRange, size int64) (int64, int64, error) {
	first, length := int64(0), size
	if rng != nil {
		if size <= contracts.MinRangeCopySource {
			return 0, 0, contracts.ErrRangeCopySourceTooSmall
		}
		if rng.First < 0 || rng.Last < rng.First || rng.Last >= size {
			return 0, 0, contracts.ErrInvalidCopyRange
		}
		first, length = rng.First, rng.Last-rng.First+1
	}
	if length > contracts.MaxCopyPartSize {
		return 0, 0, contracts.ErrEntityTooLarge
	}
	return first, length, nil
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err //nolint:wrapcheck // io.Reader contract: EOF must pass through unwrapped
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
// object, written as PutObject writes in the bucket's versioning state at
// completion, and removes the upload. The object's ETag follows S3: the
// MD5 of the concatenated binary part digests, suffixed with the part
// count. The object is only written if cond holds; otherwise the upload is
// kept.
func (s *Service) CompleteMultipartUpload(
	ctx context.Context,
	bucket, key, uploadID string,
	parts []contracts.CompletedPart,
	cond contracts.Precondition,
) (*contracts.ObjectInfo, error) {
	m, err := s.loadUpload(ctx, bucket, key, uploadID)
	if err != nil {
		return nil, err
	}

	return s.writeObject(ctx, bucket, key, &objectWrite{
		open: func() (io.Reader, *trailer, func() error, error) {
			sizes, etag, pErr := s.checkParts(uploadID, parts)
			if pErr == nil {
				pErr = s.at(stagePartsChecked)
			}
			if pErr != nil {
				return nil, nil, nil, pErr
			}
			meta := joinMetadata(m.Put.Metadata, m.Put.Headers)
			meta[etagMetaKey] = etag
			joinParts(meta, sizes)

			names := make([]string, len(parts))
			for i, p := range parts {
				names[i] = partFile(uploadID, p.PartNumber)
			}
			r := &partsReader{root: s.root, names: names}
			return r, &trailer{Meta: meta, ContentType: m.Put.ContentType, Tags: m.Put.Tags},
				func() error {
					if cErr := r.Close(); cErr != nil {
						return fmt.Errorf("%s: complete upload %s: %w", s.Name(), uploadID, cErr)
					}
					return nil
				}, nil
		},
		// Two completions of one upload may write at once; only one
		// commits.
		check: func() error {
			_, lErr := s.loadUpload(ctx, bucket, key, uploadID)
			return lErr
		},
		// An abort that removed the upload while its parts were read
		// fails the completion as one after it would.
		failed: func(err error) error {
			_, lErr := s.loadUpload(ctx, bucket, key, uploadID)
			if errors.Is(lErr, contracts.ErrNoSuchUpload) {
				return lErr
			}
			return errors.Join(err, lErr)
		},
		done: func() error {
			if _, rErr := s.removeUpload(uploadID); rErr != nil {
				return fmt.Errorf("%s: complete upload %s: %w", s.Name(), uploadID, rErr)
			}
			return nil
		},
		acl:  m.Put.ACL,
		cond: cond,
	})
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

// AbortMultipartUpload discards an upload and its parts (see
// removeUpload).
func (s *Service) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	if _, err := s.loadUpload(ctx, bucket, key, uploadID); err != nil {
		return err
	}
	if _, err := s.removeUpload(uploadID); err != nil {
		return fmt.Errorf("%s: abort upload %s: %w", s.Name(), uploadID, err)
	}
	return nil
}

// removeUpload removes upload id: its manifest first, which is the commit,
// since an upload without one does not exist, then its directory, whose
// removal takes its files in no particular order. So the upload goes at
// once, and a crash leaves no upload that is listed with parts missing,
// only, at worst, a directory without a manifest, which no operation sees.
// It reports whether the manifest is gone, so the upload is, even when
// removing the directory then failed.
//
//nolint:wrapcheck // wrapped at the caller site
func (s *Service) removeUpload(id string) (bool, error) {
	if err := s.removeIfExists(filepath.Join(uploadDir(id), manifestFile)); err != nil {
		return false, err
	}
	return true, s.root.RemoveAll(uploadDir(id))
}

// ListParts returns the time an upload was initiated and its uploaded
// parts, in part number order.
func (s *Service) ListParts(
	ctx context.Context,
	bucket, key, uploadID string,
) (*contracts.UploadParts, error) {
	m, err := s.loadUpload(ctx, bucket, key, uploadID)
	if err != nil {
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

	return &contracts.UploadParts{Initiated: m.Initiated, Parts: parts}, nil
}

// removeUploads removes the uploads into bucket of generation gen, which
// cannot complete or be listed once the bucket is deleted, and the upload
// directories of any bucket left without a manifest (see
// removeOrphanUploads). The caller holds the bucket's write lock, so none
// of the uploads is in use.
func (s *Service) removeUploads(bucket, gen string) error {
	if err := s.removeOrphanUploads(); err != nil {
		return err
	}
	entries, err := fs.ReadDir(s.root.FS(), multipartDir)
	if err != nil {
		return fmt.Errorf("%s: list uploads: %w", s.Name(), err)
	}
	for _, e := range entries {
		if !e.IsDir() || !uploadIDRe.MatchString(e.Name()) {
			continue
		}
		_, ok, uErr := s.uploadOf(e.Name(), bucket, gen)
		if uErr != nil {
			return uErr
		}
		if !ok {
			continue
		}
		if rErr := s.root.RemoveAll(uploadDir(e.Name())); rErr != nil {
			return fmt.Errorf("%s: remove upload %s: %w", s.Name(), e.Name(), rErr)
		}
	}
	return nil
}

// uploadOf returns upload id when it is an upload into bucket of
// generation gen. An upload completed or aborted meanwhile is none.
func (s *Service) uploadOf(id, bucket, gen string) (contracts.UploadInfo, bool, error) {
	m, err := s.readManifest(id)
	if errors.Is(err, fs.ErrNotExist) {
		return contracts.UploadInfo{}, false, nil
	}
	if err != nil {
		return contracts.UploadInfo{}, false, err
	}
	if m.Bucket != bucket || m.generation() != gen {
		return contracts.UploadInfo{}, false, nil
	}
	return contracts.UploadInfo{Key: m.Key, UploadID: id, Initiated: m.Initiated}, true, nil
}

// ListMultipartUploads returns the in-progress uploads into bucket, sorted by
// key and then by initiation time.
func (s *Service) ListMultipartUploads(
	ctx context.Context,
	bucket string,
) ([]contracts.UploadInfo, error) {
	gen, err := requirePin(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if _, err = s.HeadBucket(ctx, bucket); err != nil {
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
		u, ok, rErr := s.uploadOf(e.Name(), bucket, gen)
		if rErr != nil {
			return nil, rErr
		}
		if ok {
			uploads = append(uploads, u)
		}
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
