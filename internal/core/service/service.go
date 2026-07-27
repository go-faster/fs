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

func (s Service) ListObjects(ctx context.Context, req *fs.ListObjectsRequest) (*fs.ListObjectsResponse, error) {
	if err := validate.BucketName(req.Bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Prefix(req.Prefix); err != nil {
		return nil, errors.Wrap(err, "validate prefix")
	}

	return s.storage.ListObjects(ctx, req)
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

func (s Service) SetObjectACL(ctx context.Context, bucket, key string, acl fs.ACL) error {
	if err := validate.BucketName(bucket); err != nil {
		return errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(key); err != nil {
		return errors.Wrap(err, "validate object key")
	}

	return s.storage.SetObjectACL(ctx, bucket, key, acl)
}

func (s Service) ObjectOwner(ctx context.Context, bucket, key string) (fs.Owner, error) {
	if err := validate.BucketName(bucket); err != nil {
		return fs.Owner{}, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(key); err != nil {
		return fs.Owner{}, errors.Wrap(err, "validate object key")
	}

	return s.storage.ObjectOwner(ctx, bucket, key)
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

// completedUpload reports the result of an already-finished completion when
// the object at the request's key was produced by the request's upload, making
// CompleteMultipartUpload idempotent.
//
// It answers only for that exact upload id: an object written by a later PUT,
// or completed from a different upload, is not this caller's result, and a
// stale completion must still fail.
func (s Service) completedUpload(
	ctx context.Context, req *fs.CompleteMultipartUploadRequest,
) (*fs.CompleteMultipartUploadResponse, bool) {
	attributer, ok := s.storage.(fs.ObjectAttributer)
	if !ok {
		return nil, false
	}

	attrs, err := attributer.ObjectAttributes(ctx, req.Bucket, req.Key)
	if err != nil || attrs.UploadID == "" || attrs.UploadID != req.UploadID {
		return nil, false
	}

	return &fs.CompleteMultipartUploadResponse{
		Location: "/" + req.Bucket + "/" + req.Key,
		Bucket:   req.Bucket,
		Key:      req.Key,
		ETag:     attrs.ETag,
	}, true
}

// CreateBucketOwned implements fs.BucketOwnership, falling back to an unowned
// create when the backend does not record owners.
func (s Service) CreateBucketOwned(ctx context.Context, bucket string, owner fs.Owner) error {
	if err := validate.BucketName(bucket); err != nil {
		return errors.Wrap(err, "validate bucket name")
	}

	if owned, ok := s.storage.(fs.BucketOwnership); ok {
		return owned.CreateBucketOwned(ctx, bucket, owner)
	}

	return s.storage.CreateBucket(ctx, bucket)
}

// BucketOwner implements fs.BucketOwnership. A backend that does not record
// owners reports every bucket as unowned, which the S3 layer reads as
// "grants alone decide" — how this server behaved before ownership existed.
func (s Service) BucketOwner(ctx context.Context, bucket string) (fs.Owner, error) {
	if err := validate.BucketName(bucket); err != nil {
		return fs.Owner{}, errors.Wrap(err, "validate bucket name")
	}

	owned, ok := s.storage.(fs.BucketOwnership)
	if !ok {
		return fs.Owner{}, nil
	}

	return owned.BucketOwner(ctx, bucket)
}

// BucketCORS implements fs.BucketCORSStore. A backend that cannot store rules
// reports none, which the S3 layer renders as NoSuchCORSConfiguration.
func (s Service) BucketCORS(ctx context.Context, bucket string) ([]fs.CORSRule, error) {
	if err := validate.BucketName(bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	store, ok := s.storage.(fs.BucketCORSStore)
	if !ok {
		return nil, nil
	}

	return store.BucketCORS(ctx, bucket)
}

// SetBucketCORS implements fs.BucketCORSStore.
func (s Service) SetBucketCORS(ctx context.Context, bucket string, rules []fs.CORSRule) error {
	if err := validate.BucketName(bucket); err != nil {
		return errors.Wrap(err, "validate bucket name")
	}

	store, ok := s.storage.(fs.BucketCORSStore)
	if !ok {
		return errors.Wrap(fs.ErrUnsupportedOperation, "backend cannot store CORS rules")
	}

	return store.SetBucketCORS(ctx, bucket, rules)
}

// DeleteBucketCORS implements fs.BucketCORSStore.
func (s Service) DeleteBucketCORS(ctx context.Context, bucket string) error {
	if err := validate.BucketName(bucket); err != nil {
		return errors.Wrap(err, "validate bucket name")
	}

	store, ok := s.storage.(fs.BucketCORSStore)
	if !ok {
		return errors.Wrap(fs.ErrUnsupportedOperation, "backend cannot store CORS rules")
	}

	return store.DeleteBucketCORS(ctx, bucket)
}

// BucketPublicAccessBlock implements fs.BucketSettingsStore.
func (s Service) BucketPublicAccessBlock(ctx context.Context, bucket string) (*fs.PublicAccessBlock, error) {
	store, err := s.settingsStore(bucket)
	if err != nil {
		return nil, err
	}

	return store.BucketPublicAccessBlock(ctx, bucket)
}

// SetBucketPublicAccessBlock implements fs.BucketSettingsStore.
func (s Service) SetBucketPublicAccessBlock(
	ctx context.Context, bucket string, block *fs.PublicAccessBlock,
) error {
	store, err := s.settingsStore(bucket)
	if err != nil {
		return err
	}

	return store.SetBucketPublicAccessBlock(ctx, bucket, block)
}

// BucketObjectOwnership implements fs.BucketSettingsStore.
func (s Service) BucketObjectOwnership(ctx context.Context, bucket string) (string, error) {
	store, err := s.settingsStore(bucket)
	if err != nil {
		return "", err
	}

	return store.BucketObjectOwnership(ctx, bucket)
}

// SetBucketObjectOwnership implements fs.BucketSettingsStore.
func (s Service) SetBucketObjectOwnership(ctx context.Context, bucket, ownership string) error {
	store, err := s.settingsStore(bucket)
	if err != nil {
		return err
	}

	return store.SetBucketObjectOwnership(ctx, bucket, ownership)
}

// BucketEncryption implements fs.BucketEncrypter.
func (s Service) BucketEncryption(ctx context.Context, bucket string) (string, error) {
	store, err := s.encrypter(bucket)
	if err != nil {
		return "", err
	}

	return store.BucketEncryption(ctx, bucket)
}

// SetBucketEncryption implements fs.BucketEncrypter.
func (s Service) SetBucketEncryption(ctx context.Context, bucket, algorithm string) error {
	store, err := s.encrypter(bucket)
	if err != nil {
		return err
	}

	return store.SetBucketEncryption(ctx, bucket, algorithm)
}

// encrypter validates the bucket name and resolves the backend's default
// encryption capability.
func (s Service) encrypter(bucket string) (fs.BucketEncrypter, error) {
	if err := validate.BucketName(bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	store, ok := s.storage.(fs.BucketEncrypter)
	if !ok {
		return nil, errors.Wrap(fs.ErrUnsupportedOperation, "backend cannot encrypt objects")
	}

	return store, nil
}

// settingsStore validates the bucket name and resolves the backend's
// per-bucket settings capability.
func (s Service) settingsStore(bucket string) (fs.BucketSettingsStore, error) {
	if err := validate.BucketName(bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	store, ok := s.storage.(fs.BucketSettingsStore)
	if !ok {
		return nil, errors.Wrap(fs.ErrUnsupportedOperation, "backend cannot store bucket settings")
	}

	return store, nil
}

// ObjectAttributes implements fs.ObjectAttributer by forwarding to the backend
// when it can describe an object without opening it, and reporting
// ErrUnsupportedOperation when it cannot.
func (s Service) ObjectAttributes(ctx context.Context, bucket, key string) (*fs.ObjectAttributes, error) {
	if err := validate.BucketName(bucket); err != nil {
		return nil, errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(key); err != nil {
		return nil, errors.Wrap(err, "validate object key")
	}

	attributer, ok := s.storage.(fs.ObjectAttributer)
	if !ok {
		return nil, errors.Wrap(fs.ErrUnsupportedOperation, "backend cannot describe objects")
	}

	return attributer.ObjectAttributes(ctx, bucket, key)
}

// DeleteObjectIf implements fs.ConditionalDeleter by forwarding to the backend
// when it supports conditional deletes, and reporting ErrUnsupportedOperation
// when it does not — never by falling back to a racy check-then-delete.
func (s Service) DeleteObjectIf(ctx context.Context, bucket, key string, cond fs.Conditions) error {
	if err := validate.BucketName(bucket); err != nil {
		return errors.Wrap(err, "validate bucket name")
	}

	if err := validate.Key(key); err != nil {
		return errors.Wrap(err, "validate object key")
	}

	deleter, ok := s.storage.(fs.ConditionalDeleter)
	if !ok {
		return errors.Wrap(fs.ErrUnsupportedOperation, "backend cannot delete conditionally")
	}

	return deleter.DeleteObjectIf(ctx, bucket, key, cond)
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
		// The upload is gone. That is either a stale completion or — far more
		// often — a retry of one that already succeeded and whose response the
		// client never saw. SDKs retry completions, so answering NoSuchUpload
		// for the second call turns a recovered network blip into a hard
		// failure. If the object at this key came from exactly this upload, the
		// work is already done: report it as done.
		if errors.Is(err, fs.ErrUploadNotFound) {
			if done, ok := s.completedUpload(ctx, req); ok {
				return done, nil
			}
		}

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
