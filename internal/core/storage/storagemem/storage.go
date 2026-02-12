package storagemem

import (
	"context"

	"github.com/go-faster/fs"
)

var _ fs.Storage = (*Storage)(nil)

type Storage struct {
}

func (s Storage) ListObjects(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
	// TODO: implement me
	panic("implement me")
}

func (s Storage) PutObject(ctx context.Context, req *fs.PutObjectRequest) error {
	// TODO: implement me
	panic("implement me")
}

func (s Storage) ListBuckets(ctx context.Context) ([]fs.Bucket, error) {
	// TODO: implement me
	panic("implement me")
}

func (s Storage) CreateBucket(ctx context.Context, bucket string) error {
	// TODO: implement me
	panic("implement me")
}

func (s Storage) DeleteBucket(ctx context.Context, bucket string) error {
	// TODO: implement me
	panic("implement me")
}

func (s Storage) DeleteObject(ctx context.Context, bucket, key string) error {
	// TODO: implement me
	panic("implement me")
}

func (s Storage) GetObject(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
	// TODO: implement me
	panic("implement me")
}
