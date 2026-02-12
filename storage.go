package fs

import (
	"context"
)

// Storage defines the interface for S3-compatible storage operations.
//
//go:generate go tool moq -fmt goimports -out ./internal/mock/storage.go -pkg mock . Storage
type Storage interface {
	ListBuckets(ctx context.Context) ([]Bucket, error)
	CreateBucket(ctx context.Context, bucket string) error
	DeleteBucket(ctx context.Context, bucket string) error
	ListObjects(ctx context.Context, bucket, prefix string) ([]Object, error)
	PutObject(ctx context.Context, req *PutObjectRequest) error
	GetObject(ctx context.Context, bucket, key string) (*GetObjectResponse, error)
	DeleteObject(ctx context.Context, bucket, key string) error
}
