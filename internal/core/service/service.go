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

func (s Service) DeleteBucket(ctx context.Context, bucket string) error {
	if err := validate.BucketName(bucket); err != nil {
		return errors.Wrap(err, "validate bucket name")
	}

	return s.storage.DeleteBucket(ctx, bucket)
}

func (s Service) DeleteObject(ctx context.Context, bucket, key string) error {
	if err := validate.BucketName(bucket); err != nil {
		return errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(key); err != nil {
		return errors.Wrap(err, "validate object key")
	}

	return s.storage.DeleteObject(ctx, bucket, key)
}

func (s Service) GetObject(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
	if err := validate.BucketName(bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(key); err != nil {
		return nil, errors.Wrap(err, "validate object key")
	}

	return s.storage.GetObject(ctx, bucket, key)
}

func (s Service) CreateMultipartUpload(ctx context.Context, bucket, key string) (*fs.MultipartUpload, error) {
	if err := validate.BucketName(bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(key); err != nil {
		return nil, errors.Wrap(err, "validate object key")
	}

	return s.storage.CreateMultipartUpload(ctx, bucket, key)
}

func (s Service) UploadPart(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
	if err := validate.BucketName(req.Bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(req.Key); err != nil {
		return nil, errors.Wrap(err, "validate object key")
	}

	return s.storage.UploadPart(ctx, req)
}

func (s Service) CompleteMultipartUpload(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
	if err := validate.BucketName(req.Bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(req.Key); err != nil {
		return nil, errors.Wrap(err, "validate object key")
	}

	return s.storage.CompleteMultipartUpload(ctx, req)
}

func (s Service) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	if err := validate.BucketName(bucket); err != nil {
		return errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(key); err != nil {
		return errors.Wrap(err, "validate object key")
	}

	return s.storage.AbortMultipartUpload(ctx, bucket, key, uploadID)
}
