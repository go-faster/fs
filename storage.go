package fs

import (
	"context"
)

// Storage defines the interface for S3-compatible storage operations.
//
//go:generate go tool moq -fmt goimports -out ./internal/mock/storage.go -pkg mock . Storage
//go:generate go run ./scripts/gencompat
type Storage interface {
	ListBuckets(ctx context.Context) ([]Bucket, error)
	CreateBucket(ctx context.Context, bucket string) error
	DeleteBucket(ctx context.Context, bucket string) error
	BucketExists(ctx context.Context, bucket string) (bool, error)
	ListObjects(ctx context.Context, bucket, prefix string) ([]Object, error)
	PutObject(ctx context.Context, req *PutObjectRequest) (*PutObjectResponse, error)
	GetObject(ctx context.Context, bucket, key string) (*GetObjectResponse, error)
	DeleteObject(ctx context.Context, bucket, key string) error

	// GetObjectTagging returns the object's tag set (empty when untagged).
	GetObjectTagging(ctx context.Context, bucket, key string) ([]Tag, error)
	// PutObjectTagging replaces the object's tag set.
	PutObjectTagging(ctx context.Context, bucket, key string, tags []Tag) error
	// DeleteObjectTagging removes the object's tag set.
	DeleteObjectTagging(ctx context.Context, bucket, key string) error

	CreateMultipartUpload(ctx context.Context, req *CreateMultipartUploadRequest) (*MultipartUpload, error)
	UploadPart(ctx context.Context, req *UploadPartRequest) (*Part, error)
	// ListParts returns the parts uploaded so far for an in-progress multipart
	// upload, sorted by ascending part number.
	ListParts(ctx context.Context, bucket, key, uploadID string) ([]Part, error)
	// ListMultipartUploads returns the in-progress multipart uploads for a
	// bucket, sorted by object key (then upload ID for equal keys).
	ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartUpload, error)
	CompleteMultipartUpload(ctx context.Context, req *CompleteMultipartUploadRequest) (*CompleteMultipartUploadResponse, error)
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error
}
