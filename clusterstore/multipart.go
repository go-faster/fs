package clusterstore

import (
	"context"
	"crypto/md5" //nolint:gosec // MD5 is required for S3 ETag compatibility.
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// Multipart state rides the same replicated machinery as objects, in
// synthetic per-bucket namespaces that can never collide with a real bucket
// (the "\x00" separators are impossible in S3 bucket names, and namespaces
// are hashed anyway):
//
//   - the upload record — an empty object keyed uploadID + "\x00" + objectKey
//     whose sidecar carries the metadata/tags/ACL to apply at completion —
//     lives in uploadsBucket;
//   - each part is a full object in partsBucket keyed uploadID/<number>, so
//     part payloads are quorum-replicated exactly like committed objects.
//
// Completion streams the selected parts into one final coordinator write
// (with the composite S3 ETag) and then deletes the upload state.

// uploadsBucket is the synthetic namespace holding a bucket's upload records.
func uploadsBucket(bucket string) string { return "\x00mpu\x00" + bucket }

// partsBucket is the synthetic namespace holding a bucket's uploaded parts.
func partsBucket(bucket string) string { return "\x00mpp\x00" + bucket }

// partKey names one part, zero-padded so lexicographic listing order is
// part-number order.
func partKey(uploadID string, partNumber int) string {
	return fmt.Sprintf("%s/%05d", uploadID, partNumber)
}

// CreateMultipartUpload implements fs.Storage.
func (s *Storage) CreateMultipartUpload(ctx context.Context, req *fs.CreateMultipartUploadRequest) (*fs.MultipartUpload, error) {
	if err := s.mustBucket(ctx, req.Bucket); err != nil {
		return nil, err
	}

	uploadID := uuid.New().String()

	rec, err := s.coord.Put(ctx, &PutRequest{
		Bucket:   uploadsBucket(req.Bucket),
		Key:      uploadID + "\x00" + req.Key,
		Size:     0,
		Body:     strings.NewReader(""),
		Metadata: req.Metadata,
		Tags:     append([]fs.Tag(nil), req.Tags...),
		ACL:      req.ACL,
	})
	if err != nil {
		return nil, err
	}

	return &fs.MultipartUpload{
		UploadID:  uploadID,
		Bucket:    req.Bucket,
		Key:       req.Key,
		Initiated: rec.Modified,
	}, nil
}

// findUpload resolves an upload record by ID, returning its sidecar and the
// object key it will complete into. fs.ErrUploadNotFound when absent.
func (s *Storage) findUpload(ctx context.Context, bucket, uploadID string) (*Sidecar, string, error) {
	recs, err := s.coord.ListObjects(ctx, uploadsBucket(bucket), uploadID+"\x00")
	if err != nil {
		return nil, "", err
	}

	if len(recs) == 0 {
		return nil, "", errors.Wrap(fs.ErrUploadNotFound, uploadID)
	}

	rec := recs[0]
	_, key, _ := strings.Cut(rec.Key, "\x00")

	return rec, key, nil
}

// UploadPart implements fs.Storage. Re-uploading a part number replaces it.
func (s *Storage) UploadPart(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
	if _, _, err := s.findUpload(ctx, req.Bucket, req.UploadID); err != nil {
		return nil, err
	}

	sc, err := s.coord.Put(ctx, &PutRequest{
		Bucket: partsBucket(req.Bucket),
		Key:    partKey(req.UploadID, req.PartNumber),
		Size:   req.Size,
		Body:   req.Reader,
	})
	if err != nil {
		return nil, err
	}

	return &fs.Part{
		PartNumber:   req.PartNumber,
		ETag:         sc.ETag,
		Size:         sc.Size,
		LastModified: sc.Modified,
	}, nil
}

// listPartSidecars returns an upload's part sidecars in part-number order.
func (s *Storage) listPartSidecars(ctx context.Context, bucket, uploadID string) ([]*Sidecar, error) {
	return s.coord.ListObjects(ctx, partsBucket(bucket), uploadID+"/")
}

// partNumber recovers the part number from a part sidecar's key.
func partNumber(sc *Sidecar) int {
	_, num, _ := strings.Cut(sc.Key, "/")
	n, _ := strconv.Atoi(num)

	return n
}

// ListParts implements fs.Storage.
func (s *Storage) ListParts(ctx context.Context, bucket, key, uploadID string) ([]fs.Part, error) {
	_, uploadKey, err := s.findUpload(ctx, bucket, uploadID)
	if err != nil {
		return nil, err
	}

	if uploadKey != key {
		return nil, errors.Wrap(fs.ErrUploadNotFound, uploadID)
	}

	sidecars, err := s.listPartSidecars(ctx, bucket, uploadID)
	if err != nil {
		return nil, err
	}

	parts := make([]fs.Part, 0, len(sidecars))
	for _, sc := range sidecars {
		parts = append(parts, fs.Part{
			PartNumber:   partNumber(sc),
			ETag:         sc.ETag,
			Size:         sc.Size,
			LastModified: sc.Modified,
		})
	}

	return parts, nil
}

// ListMultipartUploads implements fs.Storage.
func (s *Storage) ListMultipartUploads(ctx context.Context, bucket string) ([]fs.MultipartUpload, error) {
	if err := s.mustBucket(ctx, bucket); err != nil {
		return nil, err
	}

	recs, err := s.coord.ListObjects(ctx, uploadsBucket(bucket), "")
	if err != nil {
		return nil, err
	}

	uploads := make([]fs.MultipartUpload, 0, len(recs))

	for _, rec := range recs {
		id, key, ok := strings.Cut(rec.Key, "\x00")
		if !ok {
			continue
		}

		uploads = append(uploads, fs.MultipartUpload{
			UploadID:  id,
			Bucket:    bucket,
			Key:       key,
			Initiated: rec.Modified,
		})
	}

	sort.Slice(uploads, func(i, j int) bool {
		if uploads[i].Key != uploads[j].Key {
			return uploads[i].Key < uploads[j].Key
		}

		return uploads[i].UploadID < uploads[j].UploadID
	})

	return uploads, nil
}

// CompleteMultipartUpload implements fs.Storage: the selected parts stream
// into one quorum-replicated object write with the composite S3 ETag, then
// the upload state is deleted.
func (s *Storage) CompleteMultipartUpload(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
	rec, key, err := s.findUpload(ctx, req.Bucket, req.UploadID)
	if err != nil {
		return nil, err
	}

	if err := s.mustBucket(ctx, req.Bucket); err != nil {
		// The bucket is gone: the upload can never complete.
		s.deleteUpload(ctx, req.Bucket, req.UploadID, rec)
		return nil, err
	}

	sidecars, err := s.listPartSidecars(ctx, req.Bucket, req.UploadID)
	if err != nil {
		return nil, err
	}

	uploaded := make(map[int]*Sidecar, len(sidecars))
	for _, sc := range sidecars {
		uploaded[partNumber(sc)] = sc
	}

	// Requested parts in ascending number order; missing numbers are skipped,
	// matching the single-node backends.
	parts := make([]fs.CompletedPart, len(req.Parts))
	copy(parts, req.Parts)
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })

	var (
		totalSize int64
		partKeys  []string
		etagHash  = md5.New() //nolint:gosec // MD5 is required for S3 ETag compatibility.
	)

	for _, part := range parts {
		sc, ok := uploaded[part.PartNumber]
		if !ok {
			continue
		}

		totalSize += sc.Size

		partKeys = append(partKeys, sc.Key)

		if sum, err := hex.DecodeString(sc.Checksum); err == nil {
			_, _ = etagHash.Write(sum)
		}
	}

	etag := fmt.Sprintf("%x-%d", etagHash.Sum(nil), len(parts))

	l := s.locks.of(req.Bucket, key)
	l.Lock()

	sc, err := s.coord.Put(ctx, &PutRequest{
		Bucket: req.Bucket,
		Key:    key,
		Size:   totalSize,
		Body: &partsReader{
			ctx:    ctx,
			coord:  s.coord,
			bucket: partsBucket(req.Bucket),
			keys:   partKeys,
		},
		Metadata: rec.ObjectMetadata(),
		Tags:     append([]fs.Tag(nil), rec.Tags...),
		ACL:      rec.ACL,
		ETag:     etag,
	})

	l.Unlock()

	if err != nil {
		return nil, err
	}

	s.deleteUpload(ctx, req.Bucket, req.UploadID, rec)

	return &fs.CompleteMultipartUploadResponse{
		Location: "/" + req.Bucket + "/" + key,
		Bucket:   req.Bucket,
		Key:      key,
		ETag:     sc.ETag,
	}, nil
}

// AbortMultipartUpload implements fs.Storage.
func (s *Storage) AbortMultipartUpload(ctx context.Context, bucket, _, uploadID string) error {
	rec, _, err := s.findUpload(ctx, bucket, uploadID)
	if err != nil {
		return err
	}

	s.deleteUpload(ctx, bucket, uploadID, rec)

	return nil
}

// deleteUpload removes an upload's parts and record, best-effort: leftovers
// are unreachable (the record is deleted last) and swept by the scrubber.
func (s *Storage) deleteUpload(ctx context.Context, bucket, uploadID string, rec *Sidecar) {
	if sidecars, err := s.listPartSidecars(ctx, bucket, uploadID); err == nil {
		for _, sc := range sidecars {
			_ = s.coord.Delete(ctx, partsBucket(bucket), sc.Key)
		}
	}

	_ = s.coord.Delete(ctx, uploadsBucket(bucket), rec.Key)
}

// partsReader streams the selected parts back-to-back, opening each from the
// cluster only when reached.
type partsReader struct {
	ctx    context.Context
	coord  *Coordinator
	bucket string
	keys   []string
	cur    io.ReadCloser
}

func (r *partsReader) Read(p []byte) (int, error) {
	for {
		if r.cur == nil {
			if len(r.keys) == 0 {
				return 0, io.EOF
			}

			_, rc, err := r.coord.Get(r.ctx, r.bucket, r.keys[0])
			if err != nil {
				return 0, errors.Wrapf(err, "open part %q", r.keys[0])
			}

			r.keys = r.keys[1:]
			r.cur = rc
		}

		n, err := r.cur.Read(p)
		if errors.Is(err, io.EOF) {
			_ = r.cur.Close()
			r.cur = nil

			if n > 0 {
				return n, nil
			}

			continue
		}

		return n, err
	}
}
