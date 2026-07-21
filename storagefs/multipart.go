package storagefs

import (
	"context"
	"crypto/md5" //nolint:gosec // MD5 is required for S3 ETag compatibility.
	"encoding/hex"
	"encoding/json"
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

const (
	multipartDir     = ".multipart"
	metadataFileName = "metadata.json"
)

// multipartMetadata represents the persistent metadata for a multipart upload.
type multipartMetadata struct {
	ID        string    `json:"id"`
	Bucket    string    `json:"bucket"`
	Key       string    `json:"key"`
	Initiated time.Time `json:"initiated"`
	// Metadata, Tags and ACL are applied to the object when the upload
	// completes.
	Metadata fs.ObjectMetadata `json:"metadata,omitzero"`
	Tags     []fs.Tag          `json:"tags,omitempty"`
	ACL      fs.ACL            `json:"acl,omitempty"`
}

// multipartManager manages multipart uploads with disk-based persistence.
type multipartManager struct {
	mu   sync.RWMutex
	root string
}

func newMultipartManager(root string) *multipartManager {
	return &multipartManager{
		root: root,
	}
}

// multipartPath returns the path to the multipart upload directory.
func (m *multipartManager) multipartPath() string {
	return filepath.Join(m.root, multipartDir)
}

// uploadPath returns the path for a specific upload.
func (m *multipartManager) uploadPath(uploadID string) string {
	return filepath.Join(m.multipartPath(), uploadID)
}

// metadataPath returns the path to the metadata file for an upload.
func (m *multipartManager) metadataPath(uploadID string) string {
	return filepath.Join(m.uploadPath(uploadID), metadataFileName)
}

// saveMetadata writes the upload metadata to disk.
func (m *multipartManager) saveMetadata(meta *multipartMetadata) error {
	metaPath := m.metadataPath(meta.ID)

	data, err := json.Marshal(meta)
	if err != nil {
		return errors.Wrap(err, "marshal metadata")
	}

	if err := os.WriteFile(metaPath, data, 0600); err != nil {
		return errors.Wrap(err, "write metadata file")
	}

	return nil
}

// loadMetadata reads the upload metadata from disk.
func (m *multipartManager) loadMetadata(uploadID string) (*multipartMetadata, error) {
	metaPath := m.metadataPath(uploadID)

	data, err := os.ReadFile(metaPath) //nolint:gosec // Path is constructed internally from validated uploadID.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fs.ErrUploadNotFound
		}

		return nil, errors.Wrap(err, "read metadata file")
	}

	var meta multipartMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, errors.Wrap(err, "unmarshal metadata")
	}

	return &meta, nil
}

// deleteUpload removes an upload directory and its metadata.
func (m *multipartManager) deleteUpload(uploadID string) error {
	uploadPath := m.uploadPath(uploadID)
	return os.RemoveAll(uploadPath)
}

func (s *Storage) CreateMultipartUpload(_ context.Context, req *fs.CreateMultipartUploadRequest) (*fs.MultipartUpload, error) {
	// Verify bucket exists.
	bucketPath := filepath.Join(s.root, req.Bucket)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		return nil, fs.ErrBucketNotFound
	}

	uploadID := uuid.New().String()
	uploadPath := s.multipart.uploadPath(uploadID)

	if err := os.MkdirAll(uploadPath, 0750); err != nil {
		return nil, errors.Wrap(err, "create upload directory")
	}

	meta := &multipartMetadata{
		ID:        uploadID,
		Bucket:    req.Bucket,
		Key:       req.Key,
		Initiated: time.Now(),
		Metadata:  req.Metadata,
		Tags:      req.Tags,
		ACL:       req.ACL,
	}

	s.multipart.mu.Lock()
	defer s.multipart.mu.Unlock()

	if err := s.multipart.saveMetadata(meta); err != nil {
		_ = os.RemoveAll(uploadPath)
		return nil, errors.Wrap(err, "save metadata")
	}

	return &fs.MultipartUpload{
		UploadID:  uploadID,
		Bucket:    req.Bucket,
		Key:       req.Key,
		Initiated: meta.Initiated,
	}, nil
}

func (s *Storage) UploadPart(_ context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
	s.multipart.mu.RLock()
	_, err := s.multipart.loadMetadata(req.UploadID)
	s.multipart.mu.RUnlock()

	if err != nil {
		return nil, err
	}

	// Write part to file.
	partPath := filepath.Join(s.multipart.uploadPath(req.UploadID), strconv.Itoa(req.PartNumber))

	f, err := os.Create(partPath) //nolint:gosec // Path is constructed internally from validated uploadID and partNumber.
	if err != nil {
		return nil, errors.Wrap(err, "create part file")
	}

	hash := md5.New() //nolint:gosec // MD5 is required for S3 ETag compatibility.
	writer := io.MultiWriter(f, hash)

	size, err := io.Copy(writer, req.Reader)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(partPath)

		return nil, errors.Wrap(err, "write part")
	}

	if err := f.Close(); err != nil {
		return nil, errors.Wrap(err, "close part file")
	}

	etag := hex.EncodeToString(hash.Sum(nil))

	part := &fs.Part{
		PartNumber: req.PartNumber,
		ETag:       etag,
		Size:       size,
	}

	if info, err := os.Stat(partPath); err == nil {
		part.LastModified = info.ModTime()
	}

	return part, nil
}

func (s *Storage) ListParts(_ context.Context, bucket, key, uploadID string) ([]fs.Part, error) {
	s.multipart.mu.RLock()
	defer s.multipart.mu.RUnlock()

	meta, err := s.multipart.loadMetadata(uploadID)
	if err != nil {
		return nil, err
	}

	if meta.Bucket != bucket || meta.Key != key {
		return nil, fs.ErrUploadNotFound
	}

	uploadPath := s.multipart.uploadPath(uploadID)

	entries, err := os.ReadDir(uploadPath)
	if err != nil {
		return nil, errors.Wrap(err, "read upload directory")
	}

	parts := make([]fs.Part, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == metadataFileName {
			continue
		}

		partNumber, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		part, err := statPart(filepath.Join(uploadPath, entry.Name()), partNumber)
		if err != nil {
			return nil, err
		}

		parts = append(parts, part)
	}

	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})

	return parts, nil
}

// statPart reads a stored part file's size, modification time, and MD5 ETag.
func statPart(path string, partNumber int) (fs.Part, error) {
	f, err := os.Open(path) //nolint:gosec // Path is constructed internally from validated uploadID and partNumber.
	if err != nil {
		return fs.Part{}, errors.Wrapf(err, "open part %d", partNumber)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fs.Part{}, errors.Wrapf(err, "stat part %d", partNumber)
	}

	hash := md5.New() //nolint:gosec // MD5 is required for S3 ETag compatibility.
	if _, err := io.Copy(hash, f); err != nil {
		return fs.Part{}, errors.Wrapf(err, "hash part %d", partNumber)
	}

	return fs.Part{
		PartNumber:   partNumber,
		ETag:         hex.EncodeToString(hash.Sum(nil)),
		Size:         info.Size(),
		LastModified: info.ModTime(),
	}, nil
}

func (s *Storage) ListMultipartUploads(_ context.Context, bucket string) ([]fs.MultipartUpload, error) {
	bucketPath := filepath.Join(s.root, bucket)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		return nil, fs.ErrBucketNotFound
	}

	s.multipart.mu.RLock()
	defer s.multipart.mu.RUnlock()

	entries, err := os.ReadDir(s.multipart.multipartPath())
	if err != nil {
		if os.IsNotExist(err) {
			return []fs.MultipartUpload{}, nil
		}

		return nil, errors.Wrap(err, "read multipart directory")
	}

	uploads := make([]fs.MultipartUpload, 0, len(entries))

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		meta, err := s.multipart.loadMetadata(entry.Name())
		if err != nil {
			// Skip uploads with missing or unreadable metadata (e.g. an upload
			// being created or aborted concurrently).
			continue
		}

		if meta.Bucket != bucket {
			continue
		}

		uploads = append(uploads, fs.MultipartUpload{
			UploadID:  meta.ID,
			Bucket:    meta.Bucket,
			Key:       meta.Key,
			Initiated: meta.Initiated,
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

func (s *Storage) CompleteMultipartUpload(_ context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
	s.multipart.mu.Lock()
	defer s.multipart.mu.Unlock()

	meta, err := s.multipart.loadMetadata(req.UploadID)
	if err != nil {
		return nil, err
	}

	// Sort parts by part number.
	parts := make([]fs.CompletedPart, len(req.Parts))
	copy(parts, req.Parts)
	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})

	// Create the final object path.
	objectPath := filepath.Join(s.root, meta.Bucket, toOSPath(meta.Key))

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(objectPath), 0750); err != nil {
		return nil, errors.Wrap(err, "create object directory")
	}

	// Create the final file.
	finalFile, err := os.Create(objectPath) //nolint:gosec // Path is constructed internally from validated bucket and key.
	if err != nil {
		return nil, errors.Wrap(err, "create final file")
	}
	defer func() { _ = finalFile.Close() }()

	// Concatenate all parts.
	hash := md5.New() //nolint:gosec // MD5 is required for S3 ETag compatibility.

	uploadPath := s.multipart.uploadPath(req.UploadID)
	for _, part := range parts {
		partPath := filepath.Join(uploadPath, strconv.Itoa(part.PartNumber))

		partFile, err := os.Open(partPath) //nolint:gosec // Path is constructed internally from validated uploadID and partNumber.
		if err != nil {
			return nil, errors.Wrapf(err, "open part %d", part.PartNumber)
		}

		partHash := md5.New() //nolint:gosec // MD5 is required for S3 ETag compatibility.
		_, err = io.Copy(io.MultiWriter(finalFile, partHash), partFile)
		_ = partFile.Close()

		if err != nil {
			return nil, errors.Wrapf(err, "copy part %d", part.PartNumber)
		}

		_, _ = hash.Write(partHash.Sum(nil))
	}

	if err := finalFile.Close(); err != nil {
		return nil, errors.Wrap(err, "close final file")
	}

	// Clean up upload directory.
	if err := s.multipart.deleteUpload(req.UploadID); err != nil {
		return nil, errors.Wrap(err, "cleanup upload")
	}

	etag := hex.EncodeToString(hash.Sum(nil)) + "-" + strconv.Itoa(len(parts))

	// Persist the multipart ETag and the metadata captured at initiation.
	if err := s.writeSidecar(meta.Bucket, newSidecar(meta.Key, etag, meta.Metadata, meta.Tags, meta.ACL)); err != nil {
		return nil, err
	}

	return &fs.CompleteMultipartUploadResponse{
		Location: "/" + meta.Bucket + "/" + meta.Key,
		Bucket:   meta.Bucket,
		Key:      meta.Key,
		ETag:     etag,
	}, nil
}

func (s *Storage) AbortMultipartUpload(_ context.Context, _, _, uploadID string) error {
	s.multipart.mu.Lock()
	defer s.multipart.mu.Unlock()

	// Verify upload exists.
	if _, err := s.multipart.loadMetadata(uploadID); err != nil {
		return err
	}

	// Clean up upload directory.
	if err := s.multipart.deleteUpload(uploadID); err != nil {
		return errors.Wrap(err, "remove upload directory")
	}

	return nil
}
