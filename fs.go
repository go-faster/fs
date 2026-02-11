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

// Service defines the interface for S3-compatible storage operations.
//
//go:generate go tool moq -fmt goimports -out ./internal/mock/service.go -pkg mock . Service
type Service interface {
	ListBuckets(ctx context.Context) ([]Bucket, error)
	CreateBucket(ctx context.Context, bucket string) error
	DeleteBucket(ctx context.Context, bucket string) error
	ListObjects(ctx context.Context, bucket, prefix string) ([]Object, error)
	PutObject(ctx context.Context, req *PutObjectRequest) error
	DeleteObject(ctx context.Context, bucket, key string) error
}
