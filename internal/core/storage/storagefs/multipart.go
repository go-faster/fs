package storagefs

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"github.com/google/uuid"

	"github.com/go-faster/fs"
)

// multipartUpload tracks an in-progress multipart upload
type multipartUpload struct {
	ID        string
	Bucket    string
	Key       string
	Initiated time.Time
	PartsDir  string
}

// multipartManager manages multipart uploads
type multipartManager struct {
	mu      sync.RWMutex
	uploads map[string]*multipartUpload // uploadID -> upload
}

func newMultipartManager() *multipartManager {
	return &multipartManager{
		uploads: make(map[string]*multipartUpload),
	}
}

func (s *Storage) CreateMultipartUpload(ctx context.Context, bucket, key string) (*fs.MultipartUpload, error) {
	// Verify bucket exists
	bucketPath := filepath.Join(s.root, bucket)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		return nil, fs.ErrBucketNotFound
	}

	uploadID := uuid.New().String()
	partsDir := filepath.Join(s.root, ".multipart", uploadID)

	if err := os.MkdirAll(partsDir, 0750); err != nil {
		return nil, errors.Wrap(err, "create parts directory")
	}

	upload := &multipartUpload{
		ID:        uploadID,
		Bucket:    bucket,
		Key:       key,
		Initiated: time.Now(),
		PartsDir:  partsDir,
	}

	s.multipart.mu.Lock()
	s.multipart.uploads[uploadID] = upload
	s.multipart.mu.Unlock()

	return &fs.MultipartUpload{
		UploadID:  uploadID,
		Bucket:    bucket,
		Key:       key,
		Initiated: upload.Initiated,
	}, nil
}

func (s *Storage) UploadPart(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
	s.multipart.mu.RLock()
	upload, ok := s.multipart.uploads[req.UploadID]
	s.multipart.mu.RUnlock()

	if !ok {
		return nil, errors.New("upload not found")
	}

	// Write part to temporary file
	partPath := filepath.Join(upload.PartsDir, strconv.Itoa(req.PartNumber))
	f, err := os.Create(partPath)
	if err != nil {
		return nil, errors.Wrap(err, "create part file")
	}

	hash := md5.New()
	writer := io.MultiWriter(f, hash)

	size, err := io.Copy(writer, req.Reader)
	if err != nil {
		f.Close()
		os.Remove(partPath)
		return nil, errors.Wrap(err, "write part")
	}

	if err := f.Close(); err != nil {
		return nil, errors.Wrap(err, "close part file")
	}

	etag := hex.EncodeToString(hash.Sum(nil))

	return &fs.Part{
		PartNumber: req.PartNumber,
		ETag:       etag,
		Size:       size,
	}, nil
}

func (s *Storage) CompleteMultipartUpload(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
	s.multipart.mu.Lock()
	upload, ok := s.multipart.uploads[req.UploadID]
	if !ok {
		s.multipart.mu.Unlock()
		return nil, errors.New("upload not found")
	}
	delete(s.multipart.uploads, req.UploadID)
	s.multipart.mu.Unlock()

	// Sort parts by part number
	parts := make([]fs.CompletedPart, len(req.Parts))
	copy(parts, req.Parts)
	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})

	// Create the final object path
	objectPath := filepath.Join(s.root, upload.Bucket, upload.Key)

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(objectPath), 0750); err != nil {
		return nil, errors.Wrap(err, "create object directory")
	}

	// Create the final file
	finalFile, err := os.Create(objectPath)
	if err != nil {
		return nil, errors.Wrap(err, "create final file")
	}
	defer finalFile.Close()

	// Concatenate all parts
	hash := md5.New()
	writer := io.MultiWriter(finalFile, hash)

	for _, part := range parts {
		partPath := filepath.Join(upload.PartsDir, strconv.Itoa(part.PartNumber))
		partFile, err := os.Open(partPath)
		if err != nil {
			return nil, errors.Wrapf(err, "open part %d", part.PartNumber)
		}

		_, err = io.Copy(writer, partFile)
		partFile.Close()
		if err != nil {
			return nil, errors.Wrapf(err, "copy part %d", part.PartNumber)
		}
	}

	// Clean up parts directory
	os.RemoveAll(upload.PartsDir)

	etag := hex.EncodeToString(hash.Sum(nil))

	return &fs.CompleteMultipartUploadResponse{
		Location: "/" + upload.Bucket + "/" + upload.Key,
		Bucket:   upload.Bucket,
		Key:      upload.Key,
		ETag:     etag,
	}, nil
}

func (s *Storage) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	s.multipart.mu.Lock()
	upload, ok := s.multipart.uploads[uploadID]
	if !ok {
		s.multipart.mu.Unlock()
		return errors.New("upload not found")
	}
	delete(s.multipart.uploads, uploadID)
	s.multipart.mu.Unlock()

	// Clean up parts directory
	if err := os.RemoveAll(upload.PartsDir); err != nil {
		return errors.Wrap(err, "remove parts directory")
	}

	return nil
}
