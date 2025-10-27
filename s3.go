// Package fs provides S3-compatible storage implementations.
package fs

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// S3Server implements a basic S3-compatible storage server.
type S3Server struct {
	root string
	mu   sync.RWMutex
}

// NewS3Server creates a new S3-compatible server with the given root directory.
func NewS3Server(root string) (*S3Server, error) {
	if err := os.MkdirAll(root, 0750); err != nil {
		return nil, fmt.Errorf("failed to create root directory: %w", err)
	}
	return &S3Server{
		root: root,
	}, nil
}

// Object represents an S3 object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
}

// Bucket represents an S3 bucket.
type Bucket struct {
	Name         string
	CreationDate time.Time
}

// ListAllMyBucketsResult is the XML response for listing buckets.
type ListAllMyBucketsResult struct {
	XMLName xml.Name       `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListAllMyBucketsResult"`
	Buckets BucketsWrapper `xml:"Buckets"`
}

// BucketsWrapper wraps the list of buckets.
type BucketsWrapper struct {
	Buckets []BucketInfo `xml:"Bucket"`
}

// BucketInfo is the XML representation of a bucket.
type BucketInfo struct {
	Name         string    `xml:"Name"`
	CreationDate time.Time `xml:"CreationDate"`
}

// ListBucketResult is the XML response for listing objects in a bucket.
type ListBucketResult struct {
	XMLName  xml.Name     `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	Name     string       `xml:"Name"`
	Contents []ObjectInfo `xml:"Contents"`
}

// ObjectInfo is the XML representation of an object.
type ObjectInfo struct {
	Key          string    `xml:"Key"`
	Size         int64     `xml:"Size"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag,omitempty"`
}

// ListBuckets lists all buckets in the storage.
func (s *S3Server) ListBuckets(ctx context.Context) ([]Bucket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("failed to read buckets: %w", err)
	}

	var buckets []Bucket
	for _, entry := range entries {
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			buckets = append(buckets, Bucket{
				Name:         entry.Name(),
				CreationDate: info.ModTime(),
			})
		}
	}
	return buckets, nil
}

// CreateBucket creates a new bucket.
func (s *S3Server) CreateBucket(ctx context.Context, bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	bucketPath := filepath.Join(s.root, bucket)
	if err := os.MkdirAll(bucketPath, 0750); err != nil {
		return fmt.Errorf("failed to create bucket: %w", err)
	}
	return nil
}

// DeleteBucket deletes a bucket (must be empty).
func (s *S3Server) DeleteBucket(ctx context.Context, bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	bucketPath := filepath.Join(s.root, bucket)
	if err := os.Remove(bucketPath); err != nil {
		return fmt.Errorf("failed to delete bucket: %w", err)
	}
	return nil
}

// PutObject stores an object in a bucket.
func (s *S3Server) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	objectPath := filepath.Join(s.root, bucket, key)
	if err := os.MkdirAll(filepath.Dir(objectPath), 0750); err != nil {
		return fmt.Errorf("failed to create object directory: %w", err)
	}

	// #nosec G304 -- objectPath is constructed from validated bucket and key
	f, err := os.Create(objectPath)
	if err != nil {
		return fmt.Errorf("failed to create object file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("failed to close file: %w", cerr)
		}
	}()

	if _, err := io.Copy(f, reader); err != nil {
		return fmt.Errorf("failed to write object: %w", err)
	}

	return nil
}

// GetObject retrieves an object from a bucket.
func (s *S3Server) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	objectPath := filepath.Join(s.root, bucket, key)
	info, err := os.Stat(objectPath)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to stat object: %w", err)
	}

	// #nosec G304 -- objectPath is constructed from validated bucket and key
	f, err := os.Open(objectPath)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to open object: %w", err)
	}

	return f, info.Size(), nil
}

// DeleteObject deletes an object from a bucket.
func (s *S3Server) DeleteObject(ctx context.Context, bucket, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	objectPath := filepath.Join(s.root, bucket, key)
	if err := os.Remove(objectPath); err != nil {
		return fmt.Errorf("failed to delete object: %w", err)
	}
	return nil
}

// ListObjects lists objects in a bucket with a given prefix.
func (s *S3Server) ListObjects(ctx context.Context, bucket, prefix string) ([]Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	bucketPath := filepath.Join(s.root, bucket)
	var objects []Object

	err := filepath.Walk(bucketPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(bucketPath, path)
		if err != nil {
			return err
		}

		// Convert to forward slashes for S3 compatibility
		key := filepath.ToSlash(relPath)

		if prefix == "" || strings.HasPrefix(key, prefix) {
			objects = append(objects, Object{
				Key:          key,
				Size:         info.Size(),
				LastModified: info.ModTime(),
			})
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to list objects: %w", err)
	}

	return objects, nil
}

// ServeHTTP implements a basic S3-compatible HTTP handler.
func (s *S3Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse the request path
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(path, "/", 2)

	// List buckets
	if path == "" && r.Method == http.MethodGet {
		buckets, err := s.ListBuckets(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Convert to XML response structure
		bucketInfos := make([]BucketInfo, len(buckets))
		for i, bucket := range buckets {
			bucketInfos[i] = BucketInfo(bucket)
		}

		response := ListAllMyBucketsResult{
			Buckets: BucketsWrapper{
				Buckets: bucketInfos,
			},
		}

		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(xml.Header)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := xml.NewEncoder(w).Encode(response); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		return
	}

	if len(parts) == 0 {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	bucket := parts[0]
	var key string
	if len(parts) > 1 {
		key = parts[1]
	}

	switch r.Method {
	case http.MethodPut:
		if key == "" {
			// Create bucket
			if err := s.CreateBucket(ctx, bucket); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		} else {
			// Put object
			if err := s.PutObject(ctx, bucket, key, r.Body, r.ContentLength); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}

	case http.MethodGet:
		if key == "" {
			// List objects
			prefix := r.URL.Query().Get("prefix")
			objects, err := s.ListObjects(ctx, bucket, prefix)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			// Convert to XML response structure
			objectInfos := make([]ObjectInfo, len(objects))
			for i, obj := range objects {
				objectInfos[i] = ObjectInfo(obj)
			}

			response := ListBucketResult{
				Name:     bucket,
				Contents: objectInfos,
			}

			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte(xml.Header)); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := xml.NewEncoder(w).Encode(response); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			// Get object
			rc, size, err := s.GetObject(ctx, bucket, key)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			defer func() {
				if cerr := rc.Close(); cerr != nil {
					fmt.Fprintf(os.Stderr, "failed to close reader: %v\n", cerr)
				}
			}()

			w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
			w.WriteHeader(http.StatusOK)
			if _, err := io.Copy(w, rc); err != nil {
				fmt.Fprintf(os.Stderr, "failed to copy object data: %v\n", err)
			}
		}

	case http.MethodDelete:
		if key == "" {
			// Delete bucket
			if err := s.DeleteBucket(ctx, bucket); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		} else {
			// Delete object
			if err := s.DeleteObject(ctx, bucket, key); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
