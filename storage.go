package fs

import (
	"context"
)

type Storage interface {
	ListBuckets(ctx context.Context) ([]Bucket, error)
	CreateBucket(ctx context.Context, bucket string) error
	ListObjects(ctx context.Context, bucket, prefix string) ([]Object, error)
	PutObject(ctx context.Context, req *PutObjectRequest) error
}
