package clusterstore

import (
	"context"
	"hash/fnv"
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

var _ fs.Storage = (*Storage)(nil)

// Storage is the cluster-backed fs.Storage: the S3 semantics layer over the
// Coordinator's replicated data plane and bucket registry. Every storagetest
// guarantee of the single-node backends applies here too (see the conformance
// test).
//
// Conditional writes (If-Match / If-None-Match) are serialized by a per-key
// lock held across the check and the write, which makes them atomic against
// writers on this node. Cross-node conditional-write linearizability needs a
// cluster lock and arrives with the etcd control plane; until then S3 clients
// pinned to a node (or a sticky load balancer) get full CAS semantics.
type Storage struct {
	coord *Coordinator
	locks keyLocks
}

// NewStorage wraps a Coordinator in the fs.Storage interface.
func NewStorage(c *Coordinator) *Storage {
	return &Storage{coord: c}
}

// keyLocks stripes per-object mutexes: bounded memory, enough exclusion for
// conditional-write atomicity on this node.
type keyLocks struct {
	mu [256]sync.Mutex
}

func (l *keyLocks) of(bucket, key string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(bucket))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))

	return &l.mu[h.Sum32()%uint32(len(l.mu))]
}

// mustBucket maps a missing bucket onto fs.ErrBucketNotFound.
func (s *Storage) mustBucket(ctx context.Context, bucket string) error {
	ok, err := s.coord.BucketExists(ctx, bucket)
	if err != nil {
		return err
	}

	if !ok {
		return errors.Wrap(fs.ErrBucketNotFound, bucket)
	}

	return nil
}

// ListBuckets implements fs.Storage.
func (s *Storage) ListBuckets(ctx context.Context) ([]fs.Bucket, error) {
	infos, err := s.coord.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}

	buckets := make([]fs.Bucket, 0, len(infos))
	for _, info := range infos {
		buckets = append(buckets, fs.Bucket{Name: info.Name, CreationDate: info.Created})
	}

	return buckets, nil
}

// CreateBucket implements fs.Storage.
func (s *Storage) CreateBucket(ctx context.Context, bucket string) error {
	return s.coord.CreateBucket(ctx, bucket, fs.ACLPrivate)
}

// DeleteBucket implements fs.Storage, refusing to delete a non-empty bucket.
func (s *Storage) DeleteBucket(ctx context.Context, bucket string) error {
	if err := s.mustBucket(ctx, bucket); err != nil {
		return err
	}

	objects, err := s.coord.ListObjects(ctx, bucket, "")
	if err != nil {
		return err
	}

	if len(objects) > 0 {
		return errors.Wrap(fs.ErrBucketNotEmpty, bucket)
	}

	return s.coord.DeleteBucket(ctx, bucket)
}

// BucketExists implements fs.Storage.
func (s *Storage) BucketExists(ctx context.Context, bucket string) (bool, error) {
	return s.coord.BucketExists(ctx, bucket)
}

// ListObjects implements fs.Storage.
func (s *Storage) ListObjects(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
	if err := s.mustBucket(ctx, bucket); err != nil {
		return nil, err
	}

	sidecars, err := s.coord.ListObjects(ctx, bucket, prefix)
	if err != nil {
		return nil, err
	}

	objects := make([]fs.Object, 0, len(sidecars))
	for _, sc := range sidecars {
		objects = append(objects, fs.Object{
			Key:          sc.Key,
			Size:         sc.Size,
			LastModified: sc.Modified,
			ETag:         sc.ETag,
		})
	}

	return objects, nil
}

// PutObject implements fs.Storage. The conditional check and the write happen
// under the object's key lock, so concurrent conditional PUTs on this node
// resolve to a single winner.
func (s *Storage) PutObject(ctx context.Context, req *fs.PutObjectRequest) (*fs.PutObjectResponse, error) {
	if err := s.mustBucket(ctx, req.Bucket); err != nil {
		return nil, err
	}

	l := s.locks.of(req.Bucket, req.Key)
	l.Lock()
	defer l.Unlock()

	if req.IfNoneMatch != "" || req.IfMatch != "" {
		var (
			exists      bool
			currentETag string
		)

		switch cur, err := s.coord.Stat(ctx, req.Bucket, req.Key); {
		case err == nil:
			exists, currentETag = true, cur.ETag
		case !errors.Is(err, ErrNotFound):
			return nil, err
		}

		if req.PreconditionFailed(exists, currentETag) {
			return nil, fs.ErrPreconditionFailed
		}
	}

	sc, err := s.coord.Put(ctx, &PutRequest{
		Bucket:   req.Bucket,
		Key:      req.Key,
		Size:     req.Size,
		Body:     req.Reader,
		Metadata: req.Metadata,
		Tags:     append([]fs.Tag(nil), req.Tags...),
		ACL:      req.ACL,
	})
	if err != nil {
		return nil, err
	}

	return &fs.PutObjectResponse{ETag: sc.ETag}, nil
}

// GetObject implements fs.Storage.
func (s *Storage) GetObject(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
	if err := s.mustBucket(ctx, bucket); err != nil {
		return nil, err
	}

	sc, rc, err := s.coord.Get(ctx, bucket, key)
	if err != nil {
		return nil, mapObjectErr(err, key)
	}

	return &fs.GetObjectResponse{
		Reader:       rc,
		Size:         sc.Size,
		LastModified: sc.Modified,
		ETag:         sc.ETag,
		Metadata:     sc.ObjectMetadata(),
	}, nil
}

// DeleteObject implements fs.Storage.
func (s *Storage) DeleteObject(ctx context.Context, bucket, key string) error {
	if err := s.mustBucket(ctx, bucket); err != nil {
		return err
	}

	return mapObjectErr(s.coord.Delete(ctx, bucket, key), key)
}

// GetObjectTagging implements fs.Storage.
func (s *Storage) GetObjectTagging(ctx context.Context, bucket, key string) ([]fs.Tag, error) {
	sc, err := s.statObject(ctx, bucket, key)
	if err != nil {
		return nil, err
	}

	return append([]fs.Tag(nil), sc.Tags...), nil
}

// PutObjectTagging implements fs.Storage.
func (s *Storage) PutObjectTagging(ctx context.Context, bucket, key string, tags []fs.Tag) error {
	return s.updateObject(ctx, bucket, key, func(sc *Sidecar) {
		sc.Tags = append([]fs.Tag(nil), tags...)
	})
}

// DeleteObjectTagging implements fs.Storage.
func (s *Storage) DeleteObjectTagging(ctx context.Context, bucket, key string) error {
	return s.updateObject(ctx, bucket, key, func(sc *Sidecar) {
		sc.Tags = nil
	})
}

// SetBucketACL implements fs.Storage.
func (s *Storage) SetBucketACL(ctx context.Context, bucket string, acl fs.ACL) error {
	return s.coord.SetBucketACL(ctx, bucket, acl)
}

// BucketACL implements fs.Storage.
func (s *Storage) BucketACL(ctx context.Context, bucket string) (fs.ACL, error) {
	info, err := s.coord.Bucket(ctx, bucket)
	if err != nil {
		return fs.ACLPrivate, err
	}

	return normalizeACL(info.ACL), nil
}

// ObjectACL implements fs.Storage.
func (s *Storage) ObjectACL(ctx context.Context, bucket, key string) (fs.ACL, error) {
	sc, err := s.statObject(ctx, bucket, key)
	if err != nil {
		return fs.ACLPrivate, err
	}

	return normalizeACL(sc.ACL), nil
}

// statObject fetches an object's sidecar with fs.Storage error mapping.
func (s *Storage) statObject(ctx context.Context, bucket, key string) (*Sidecar, error) {
	if err := s.mustBucket(ctx, bucket); err != nil {
		return nil, err
	}

	sc, err := s.coord.Stat(ctx, bucket, key)
	if err != nil {
		return nil, mapObjectErr(err, key)
	}

	return sc, nil
}

// updateObject rewrites an object's sidecar under its key lock.
func (s *Storage) updateObject(ctx context.Context, bucket, key string, mutate func(*Sidecar)) error {
	if err := s.mustBucket(ctx, bucket); err != nil {
		return err
	}

	l := s.locks.of(bucket, key)
	l.Lock()
	defer l.Unlock()

	return mapObjectErr(s.coord.UpdateSidecar(ctx, bucket, key, mutate), key)
}

// mapObjectErr converts coordinator sentinels to fs.Storage sentinels.
func mapObjectErr(err error, key string) error {
	if errors.Is(err, ErrNotFound) {
		return errors.Wrap(fs.ErrObjectNotFound, key)
	}

	return err
}

// normalizeACL defaults an unset ACL to private.
func normalizeACL(a fs.ACL) fs.ACL {
	if a == "" {
		return fs.ACLPrivate
	}

	return a
}
