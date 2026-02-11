package service

import (
	"context"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/validate"
)

var _ fs.Service = (*Service)(nil)

func New(storage fs.Storage) *Service {
	return &Service{storage: storage}
}

type Service struct {
	storage fs.Storage
}

func (s Service) ListObjects(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
	if err := validate.BucketName(bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}
	if err := validate.Prefix(prefix); err != nil {
		return nil, errors.Wrap(err, "validate prefix")
	}
	return s.storage.ListObjects(ctx, bucket, prefix)
}

func (s Service) PutObject(ctx context.Context, req *fs.PutObjectRequest) error {
	if err := validate.BucketName(req.Bucket); err != nil {
		return errors.Wrap(err, "validate bucket name")
	}
	if err := validate.Key(req.Key); err != nil {
		return errors.Wrap(err, "validate object key")
	}
	return s.storage.PutObject(ctx, req)
}

func (s Service) ListBuckets(ctx context.Context) ([]fs.Bucket, error) {
	return s.storage.ListBuckets(ctx)
}

func (s Service) CreateBucket(ctx context.Context, bucket string) error {
	if err := validate.BucketName(bucket); err != nil {
		return errors.Wrap(err, "validate bucket name")
	}

	return s.storage.CreateBucket(ctx, bucket)
}
