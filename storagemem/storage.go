// Package storagemem implements fs.Storage using in-memory storage.
package storagemem

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // MD5 is required for S3 ETag compatibility.
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"github.com/google/uuid"

	"github.com/go-faster/fs"
)

var _ fs.Storage = (*Storage)(nil)

// New creates a new in-memory storage.
func New() *Storage {
	return &Storage{
		buckets: make(map[string]*bucket),
		uploads: make(map[string]*multipartUpload),
	}
}

type object struct {
	data         []byte
	lastModified time.Time
	etag         string
	metadata     fs.ObjectMetadata
	tags         []fs.Tag
}

type bucket struct {
	name         string
	creationDate time.Time
	objects      map[string]*object
}

type uploadPart struct {
	partNumber   int
	data         []byte
	etag         string
	lastModified time.Time
}

type multipartUpload struct {
	id        string
	bucket    string
	key       string
	initiated time.Time
	parts     map[int]*uploadPart
	metadata  fs.ObjectMetadata
	tags      []fs.Tag
}

// Storage implements fs.Storage interface using in-memory storage.
type Storage struct {
	mu      sync.RWMutex
	buckets map[string]*bucket
	uploads map[string]*multipartUpload
}

func (s *Storage) ListBuckets(ctx context.Context) ([]fs.Bucket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	buckets := make([]fs.Bucket, 0, len(s.buckets))
	for _, b := range s.buckets {
		buckets = append(buckets, fs.Bucket{
			Name:         b.name,
			CreationDate: b.creationDate,
		})
	}

	return buckets, nil
}

func (s *Storage) CreateBucket(ctx context.Context, bucketName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.buckets[bucketName]; exists {
		return errors.Wrapf(fs.ErrBucketAlreadyExists, "bucket %q", bucketName)
	}

	s.buckets[bucketName] = &bucket{
		name:         bucketName,
		creationDate: time.Now(),
		objects:      make(map[string]*object),
	}

	return nil
}

func (s *Storage) BucketExists(_ context.Context, bucketName string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, exists := s.buckets[bucketName]

	return exists, nil
}

func (s *Storage) DeleteBucket(ctx context.Context, bucketName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, exists := s.buckets[bucketName]
	if !exists {
		return fs.ErrBucketNotFound
	}

	if len(b.objects) > 0 {
		return fs.ErrBucketNotEmpty
	}

	delete(s.buckets, bucketName)

	return nil
}

func (s *Storage) ListObjects(ctx context.Context, bucketName, prefix string) ([]fs.Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	b, exists := s.buckets[bucketName]
	if !exists {
		return nil, fs.ErrBucketNotFound
	}

	objects := make([]fs.Object, 0)
	for key, obj := range b.objects {
		if prefix == "" || strings.HasPrefix(key, prefix) {
			objects = append(objects, fs.Object{
				Key:          key,
				Size:         int64(len(obj.data)),
				LastModified: obj.lastModified,
				ETag:         obj.etag,
			})
		}
	}

	return objects, nil
}

func (s *Storage) PutObject(ctx context.Context, req *fs.PutObjectRequest) (*fs.PutObjectResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, exists := s.buckets[req.Bucket]
	if !exists {
		return nil, fs.ErrBucketNotFound
	}

	// Read all data from the reader
	data, err := io.ReadAll(req.Reader)
	if err != nil {
		return nil, errors.Wrap(err, "read data")
	}

	// Calculate ETag (MD5 hash).
	hash := md5.Sum(data) //nolint:gosec // MD5 is required for S3 ETag compatibility.
	etag := fmt.Sprintf("%x", hash)

	b.objects[req.Key] = &object{
		data:         data,
		lastModified: time.Now(),
		etag:         etag,
		metadata:     req.Metadata,
		tags:         append([]fs.Tag(nil), req.Tags...),
	}

	return &fs.PutObjectResponse{ETag: etag}, nil
}

// getObject returns the live object entry; the caller must hold s.mu.
func (s *Storage) getObject(bucketName, key string) (*object, error) {
	b, exists := s.buckets[bucketName]
	if !exists {
		return nil, fs.ErrBucketNotFound
	}

	obj, exists := b.objects[key]
	if !exists {
		return nil, fs.ErrObjectNotFound
	}

	return obj, nil
}

func (s *Storage) GetObjectTagging(_ context.Context, bucketName, key string) ([]fs.Tag, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	obj, err := s.getObject(bucketName, key)
	if err != nil {
		return nil, err
	}

	return append([]fs.Tag(nil), obj.tags...), nil
}

func (s *Storage) PutObjectTagging(_ context.Context, bucketName, key string, tags []fs.Tag) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	obj, err := s.getObject(bucketName, key)
	if err != nil {
		return err
	}

	obj.tags = append([]fs.Tag(nil), tags...)

	return nil
}

func (s *Storage) DeleteObjectTagging(_ context.Context, bucketName, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	obj, err := s.getObject(bucketName, key)
	if err != nil {
		return err
	}

	obj.tags = nil

	return nil
}

func (s *Storage) GetObject(ctx context.Context, bucketName, key string) (*fs.GetObjectResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	b, exists := s.buckets[bucketName]
	if !exists {
		return nil, fs.ErrBucketNotFound
	}

	obj, exists := b.objects[key]
	if !exists {
		return nil, fs.ErrObjectNotFound
	}

	// Create a copy of the data to avoid races
	dataCopy := make([]byte, len(obj.data))
	copy(dataCopy, obj.data)

	return &fs.GetObjectResponse{
		Reader:       readSeekNopCloser{bytes.NewReader(dataCopy)},
		Size:         int64(len(dataCopy)),
		LastModified: obj.lastModified,
		ETag:         obj.etag,
		Metadata:     obj.metadata,
	}, nil
}

// readSeekNopCloser adapts a *bytes.Reader into an io.ReadSeekCloser so the
// handler can serve byte ranges via http.ServeContent.
type readSeekNopCloser struct {
	*bytes.Reader
}

func (readSeekNopCloser) Close() error { return nil }

func (s *Storage) DeleteObject(ctx context.Context, bucketName, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, exists := s.buckets[bucketName]
	if !exists {
		return fs.ErrBucketNotFound
	}

	if _, exists := b.objects[key]; !exists {
		return fs.ErrObjectNotFound
	}

	delete(b.objects, key)

	return nil
}

func (s *Storage) CreateMultipartUpload(ctx context.Context, req *fs.CreateMultipartUploadRequest) (*fs.MultipartUpload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.buckets[req.Bucket]; !exists {
		return nil, fs.ErrBucketNotFound
	}

	uploadID := uuid.New().String()
	upload := &multipartUpload{
		id:        uploadID,
		bucket:    req.Bucket,
		key:       req.Key,
		initiated: time.Now(),
		parts:     make(map[int]*uploadPart),
		metadata:  req.Metadata,
		tags:      append([]fs.Tag(nil), req.Tags...),
	}

	s.uploads[uploadID] = upload

	return &fs.MultipartUpload{
		UploadID:  uploadID,
		Bucket:    req.Bucket,
		Key:       req.Key,
		Initiated: upload.initiated,
	}, nil
}

func (s *Storage) UploadPart(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	upload, exists := s.uploads[req.UploadID]
	if !exists {
		return nil, fs.ErrUploadNotFound
	}

	data, err := io.ReadAll(req.Reader)
	if err != nil {
		return nil, errors.Wrap(err, "read part data")
	}

	hash := md5.Sum(data) //nolint:gosec // MD5 is required for S3 ETag compatibility.
	etag := hex.EncodeToString(hash[:])

	part := &uploadPart{
		partNumber:   req.PartNumber,
		data:         data,
		etag:         etag,
		lastModified: time.Now(),
	}
	upload.parts[req.PartNumber] = part

	return &fs.Part{
		PartNumber:   req.PartNumber,
		ETag:         etag,
		Size:         int64(len(data)),
		LastModified: part.lastModified,
	}, nil
}

func (s *Storage) ListParts(_ context.Context, bucket, key, uploadID string) ([]fs.Part, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	upload, exists := s.uploads[uploadID]
	if !exists || upload.bucket != bucket || upload.key != key {
		return nil, fs.ErrUploadNotFound
	}

	parts := make([]fs.Part, 0, len(upload.parts))
	for _, p := range upload.parts {
		parts = append(parts, fs.Part{
			PartNumber:   p.partNumber,
			ETag:         p.etag,
			Size:         int64(len(p.data)),
			LastModified: p.lastModified,
		})
	}

	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})

	return parts, nil
}

func (s *Storage) ListMultipartUploads(_ context.Context, bucketName string) ([]fs.MultipartUpload, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, exists := s.buckets[bucketName]; !exists {
		return nil, fs.ErrBucketNotFound
	}

	uploads := make([]fs.MultipartUpload, 0)

	for _, u := range s.uploads {
		if u.bucket != bucketName {
			continue
		}

		uploads = append(uploads, fs.MultipartUpload{
			UploadID:  u.id,
			Bucket:    u.bucket,
			Key:       u.key,
			Initiated: u.initiated,
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

// multipartETag returns the S3 ETag for a completed multipart upload.
func multipartETag(parts []fs.CompletedPart, uploaded map[int]*uploadPart) string {
	hash := md5.New() //nolint:gosec // MD5 is required for S3 ETag compatibility.

	for _, part := range parts {
		p, ok := uploaded[part.PartNumber]
		if !ok {
			continue
		}

		partHash := md5.Sum(p.data) //nolint:gosec // MD5 is required for S3 ETag compatibility.
		_, _ = hash.Write(partHash[:])
	}

	return fmt.Sprintf("%x-%d", hash.Sum(nil), len(parts))
}

func (s *Storage) CompleteMultipartUpload(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	upload, exists := s.uploads[req.UploadID]
	if !exists {
		return nil, fs.ErrUploadNotFound
	}

	b, exists := s.buckets[upload.bucket]
	if !exists {
		delete(s.uploads, req.UploadID)
		return nil, fs.ErrBucketNotFound
	}

	// Sort parts by part number
	parts := make([]fs.CompletedPart, len(req.Parts))
	copy(parts, req.Parts)
	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})

	// Concatenate all parts
	var totalSize int64

	for _, part := range parts {
		if p, ok := upload.parts[part.PartNumber]; ok {
			totalSize += int64(len(p.data))
		}
	}

	data := make([]byte, 0, totalSize)

	for _, part := range parts {
		if p, ok := upload.parts[part.PartNumber]; ok {
			data = append(data, p.data...)
		}
	}

	etag := multipartETag(parts, upload.parts)

	b.objects[upload.key] = &object{
		data:         data,
		lastModified: time.Now(),
		etag:         etag,
		metadata:     upload.metadata,
		tags:         upload.tags,
	}

	delete(s.uploads, req.UploadID)

	return &fs.CompleteMultipartUploadResponse{
		Location: "/" + upload.bucket + "/" + upload.key,
		Bucket:   upload.bucket,
		Key:      upload.key,
		ETag:     etag,
	}, nil
}

func (s *Storage) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.uploads[uploadID]; !exists {
		return fs.ErrUploadNotFound
	}

	delete(s.uploads, uploadID)

	return nil
}
