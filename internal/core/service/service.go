package service

import (
	"context"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/validate"
)

var _ fs.Storage = (*Service)(nil)

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

func (s Service) PutObject(ctx context.Context, req *fs.PutObjectRequest) (*fs.PutObjectResponse, error) {
	if err := validate.BucketName(req.Bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(req.Key); err != nil {
		return nil, errors.Wrap(err, "validate object key")
	}

	if err := validateTags(req.Tags); err != nil {
		return nil, err
	}

	return s.storage.PutObject(ctx, req)
}

// S3 object-tagging limits.
const (
	maxObjectTags  = 10
	maxTagKeyLen   = 128
	maxTagValueLen = 256
)

// validateTags enforces the S3 object-tagging limits: at most 10 tags with
// unique, non-empty keys of at most 128 characters and values of at most 256.
func validateTags(tags []fs.Tag) error {
	if len(tags) > maxObjectTags {
		return errors.Wrapf(fs.ErrInvalidTag, "%d tags exceed the limit of %d", len(tags), maxObjectTags)
	}

	seen := make(map[string]struct{}, len(tags))

	for _, tag := range tags {
		if tag.Key == "" || len(tag.Key) > maxTagKeyLen {
			return errors.Wrapf(fs.ErrInvalidTag, "tag key %q", tag.Key)
		}

		if len(tag.Value) > maxTagValueLen {
			return errors.Wrapf(fs.ErrInvalidTag, "tag %q value too long", tag.Key)
		}

		if _, ok := seen[tag.Key]; ok {
			return errors.Wrapf(fs.ErrInvalidTag, "duplicate tag key %q", tag.Key)
		}

		seen[tag.Key] = struct{}{}
	}

	return nil
}

func (s Service) GetObjectTagging(ctx context.Context, bucket, key string) ([]fs.Tag, error) {
	if err := validate.BucketName(bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(key); err != nil {
		return nil, errors.Wrap(err, "validate object key")
	}

	return s.storage.GetObjectTagging(ctx, bucket, key)
}

func (s Service) PutObjectTagging(ctx context.Context, bucket, key string, tags []fs.Tag) error {
	if err := validate.BucketName(bucket); err != nil {
		return errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(key); err != nil {
		return errors.Wrap(err, "validate object key")
	}

	if err := validateTags(tags); err != nil {
		return err
	}

	return s.storage.PutObjectTagging(ctx, bucket, key, tags)
}

func (s Service) DeleteObjectTagging(ctx context.Context, bucket, key string) error {
	if err := validate.BucketName(bucket); err != nil {
		return errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(key); err != nil {
		return errors.Wrap(err, "validate object key")
	}

	return s.storage.DeleteObjectTagging(ctx, bucket, key)
}

func (s Service) SetBucketACL(ctx context.Context, bucket string, acl fs.ACL) error {
	if err := validate.BucketName(bucket); err != nil {
		return errors.Wrap(err, "validate bucket name")
	}

	return s.storage.SetBucketACL(ctx, bucket, acl)
}

func (s Service) BucketACL(ctx context.Context, bucket string) (fs.ACL, error) {
	if err := validate.BucketName(bucket); err != nil {
		return fs.ACLPrivate, errors.Wrap(err, "validate bucket name")
	}

	return s.storage.BucketACL(ctx, bucket)
}

func (s Service) ObjectACL(ctx context.Context, bucket, key string) (fs.ACL, error) {
	if err := validate.BucketName(bucket); err != nil {
		return fs.ACLPrivate, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(key); err != nil {
		return fs.ACLPrivate, errors.Wrap(err, "validate object key")
	}

	return s.storage.ObjectACL(ctx, bucket, key)
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

func (s Service) BucketExists(ctx context.Context, bucket string) (bool, error) {
	if err := validate.BucketName(bucket); err != nil {
		return false, errors.Wrap(err, "validate bucket name")
	}

	return s.storage.BucketExists(ctx, bucket)
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

func (s Service) CreateMultipartUpload(ctx context.Context, req *fs.CreateMultipartUploadRequest) (*fs.MultipartUpload, error) {
	if err := validate.BucketName(req.Bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(req.Key); err != nil {
		return nil, errors.Wrap(err, "validate object key")
	}

	if err := validateTags(req.Tags); err != nil {
		return nil, err
	}

	return s.storage.CreateMultipartUpload(ctx, req)
}

const (
	// MinPartNumber and MaxPartNumber bound valid S3 part numbers.
	MinPartNumber = 1
	MaxPartNumber = 10000

	// MinPartSize is the S3 minimum size for every multipart part except the
	// last one listed in CompleteMultipartUpload.
	MinPartSize = 5 * 1024 * 1024
)

func (s Service) UploadPart(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
	if err := validate.BucketName(req.Bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(req.Key); err != nil {
		return nil, errors.Wrap(err, "validate object key")
	}

	if req.PartNumber < MinPartNumber || req.PartNumber > MaxPartNumber {
		return nil, errors.Wrapf(fs.ErrInvalidPartNumber, "part number %d", req.PartNumber)
	}

	return s.storage.UploadPart(ctx, req)
}

func (s Service) ListParts(ctx context.Context, bucket, key, uploadID string) ([]fs.Part, error) {
	if err := validate.BucketName(bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(key); err != nil {
		return nil, errors.Wrap(err, "validate object key")
	}

	return s.storage.ListParts(ctx, bucket, key, uploadID)
}

func (s Service) ListMultipartUploads(ctx context.Context, bucket string) ([]fs.MultipartUpload, error) {
	if err := validate.BucketName(bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	return s.storage.ListMultipartUploads(ctx, bucket)
}

func (s Service) CompleteMultipartUpload(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
	if err := validate.BucketName(req.Bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(req.Key); err != nil {
		return nil, errors.Wrap(err, "validate object key")
	}

	if len(req.Parts) == 0 {
		return nil, errors.Wrap(fs.ErrInvalidPart, "no parts specified")
	}

	// Part numbers must be strictly ascending (duplicates included).
	for i := 1; i < len(req.Parts); i++ {
		if req.Parts[i].PartNumber <= req.Parts[i-1].PartNumber {
			return nil, errors.Wrapf(fs.ErrInvalidPartOrder,
				"part %d after part %d", req.Parts[i].PartNumber, req.Parts[i-1].PartNumber)
		}
	}

	uploaded, err := s.storage.ListParts(ctx, req.Bucket, req.Key, req.UploadID)
	if err != nil {
		return nil, errors.Wrap(err, "list parts")
	}

	byNumber := make(map[int]fs.Part, len(uploaded))
	for _, p := range uploaded {
		byNumber[p.PartNumber] = p
	}

	// Every referenced part must exist with a matching ETag; only then are
	// sizes validated (matching S3's error precedence).
	for _, part := range req.Parts {
		stored, ok := byNumber[part.PartNumber]
		if !ok || stored.ETag != part.ETag {
			return nil, errors.Wrapf(fs.ErrInvalidPart, "part %d", part.PartNumber)
		}
	}

	// Every part except the last listed one must meet the size floor.
	for _, part := range req.Parts[:len(req.Parts)-1] {
		if stored := byNumber[part.PartNumber]; stored.Size < MinPartSize {
			return nil, errors.Wrapf(fs.ErrEntityTooSmall,
				"part %d is %d bytes", part.PartNumber, stored.Size)
		}
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
