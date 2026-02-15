// Package storagemem implements fs.Storage using in-memory storage.
package storagemem

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

var _ fs.Storage = (*Storage)(nil)

// New creates a new in-memory storage.
func New() *Storage {
	return &Storage{
		buckets: make(map[string]*bucket),
	}
}

type object struct {
	data         []byte
	lastModified time.Time
	etag         string
}

type bucket struct {
	name         string
	creationDate time.Time
	objects      map[string]*object
}

// Storage implements fs.Storage interface using in-memory storage.
type Storage struct {
	mu      sync.RWMutex
	buckets map[string]*bucket
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
		return errors.Errorf("bucket already exists: %s", bucketName)
	}

	s.buckets[bucketName] = &bucket{
		name:         bucketName,
		creationDate: time.Now(),
		objects:      make(map[string]*object),
	}

	return nil
}

func (s *Storage) DeleteBucket(ctx context.Context, bucketName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, exists := s.buckets[bucketName]
	if !exists {
		return fs.ErrBucketNotFound
	}

	if len(b.objects) > 0 {
		return errors.New("bucket not empty")
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

func (s *Storage) PutObject(ctx context.Context, req *fs.PutObjectRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, exists := s.buckets[req.Bucket]
	if !exists {
		return fs.ErrBucketNotFound
	}

	// Read all data from the reader
	data, err := io.ReadAll(req.Reader)
	if err != nil {
		return errors.Wrap(err, "read data")
	}

	// Calculate ETag (MD5 hash)
	hash := md5.Sum(data)
	etag := fmt.Sprintf("%x", hash)

	b.objects[req.Key] = &object{
		data:         data,
		lastModified: time.Now(),
		etag:         etag,
	}

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
		Reader:       io.NopCloser(bytes.NewReader(dataCopy)),
		Size:         int64(len(dataCopy)),
		LastModified: obj.lastModified,
		ETag:         obj.etag,
	}, nil
}

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
