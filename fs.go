// Package fs is a S3-compatible storage server implementation.
package fs

import (
	"context"
	"io"
	"time"
)

// Bucket represents an S3 bucket.
type Bucket struct {
	Name         string
	CreationDate time.Time
}

// Object represents an S3 object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
}

type PutObjectRequest struct {
	Reader io.Reader
	Bucket string
	Key    string
	Size   int64
}

// GetObjectResponse represents the response for GetObject operation.
type GetObjectResponse struct {
	Reader       io.ReadCloser
	Size         int64
	LastModified time.Time
	ETag         string
	ContentType  string
}

// MultipartUpload represents an in-progress multipart upload.
type MultipartUpload struct {
	UploadID  string
	Bucket    string
	Key       string
	Initiated time.Time
}

// Part represents a part of a multipart upload.
type Part struct {
	PartNumber int
	ETag       string
	Size       int64
}

// UploadPartRequest represents a request to upload a part.
type UploadPartRequest struct {
	Bucket     string
	Key        string
	UploadID   string
	PartNumber int
	Reader     io.Reader
	Size       int64
}

// CompletedPart represents a completed part for completing multipart upload.
type CompletedPart struct {
	PartNumber int
	ETag       string
}

// CompleteMultipartUploadRequest represents a request to complete multipart upload.
type CompleteMultipartUploadRequest struct {
	Bucket   string
	Key      string
	UploadID string
	Parts    []CompletedPart
}

// CompleteMultipartUploadResponse represents the response for completing multipart upload.
type CompleteMultipartUploadResponse struct {
	Location string
	Bucket   string
	Key      string
	ETag     string
}

// Service defines the interface for S3-compatible storage operations.
//
//go:generate go tool moq -fmt goimports -out ./internal/mock/service.go -pkg mock . Service
type Service interface {
	ListBuckets(ctx context.Context) ([]Bucket, error)
	CreateBucket(ctx context.Context, bucket string) error
	DeleteBucket(ctx context.Context, bucket string) error
	BucketExists(ctx context.Context, bucket string) (bool, error)
	ListObjects(ctx context.Context, bucket, prefix string) ([]Object, error)
	PutObject(ctx context.Context, req *PutObjectRequest) error
	GetObject(ctx context.Context, bucket, key string) (*GetObjectResponse, error)
	DeleteObject(ctx context.Context, bucket, key string) error

	CreateMultipartUpload(ctx context.Context, bucket, key string) (*MultipartUpload, error)
	UploadPart(ctx context.Context, req *UploadPartRequest) (*Part, error)
	CompleteMultipartUpload(ctx context.Context, req *CompleteMultipartUploadRequest) (*CompleteMultipartUploadResponse, error)
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error
}
