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
// Unconditional writes never take that lock — see PutObject.
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

// CreateBucketOwned implements fs.BucketOwnership.
func (s *Storage) CreateBucketOwned(ctx context.Context, bucket string, owner fs.Owner) error {
	return s.coord.CreateBucketOwned(ctx, bucket, fs.ACLPrivate, owner)
}

// BucketOwner implements fs.BucketOwnership.
func (s *Storage) BucketOwner(ctx context.Context, bucket string) (fs.Owner, error) {
	info, err := s.coord.fetchBucket(ctx, s.coord.topo.Topology(), bucket)
	if err != nil {
		return fs.Owner{}, err
	}

	return info.Owner, nil
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
//
// The page is served from the nodes' object indexes when they can serve it,
// which costs what the page contains. When they cannot — a node still building
// its index, or one running a binary without one — it falls back to gathering
// every sidecar in the cluster and folding that, which is what this did before
// the indexes existed. Both paths must return the same page; the difference is
// only what it costs.
func (s *Storage) ListObjects(ctx context.Context, req *fs.ListObjectsRequest) (*fs.ListObjectsResponse, error) {
	if err := s.mustBucket(ctx, req.Bucket); err != nil {
		return nil, err
	}

	res, err := s.listFromIndex(ctx, req)
	if err == nil {
		return res, nil
	}

	if !errors.Is(err, ErrIndexUnavailable) {
		return nil, err
	}

	return s.listFromSidecars(ctx, req)
}

// listFromIndex serves a page from the merged per-node indexes.
func (s *Storage) listFromIndex(ctx context.Context, req *fs.ListObjectsRequest) (*fs.ListObjectsResponse, error) {
	sidecars, prefixes, more, err := s.coord.ListPage(
		ctx, req.Bucket, req.Prefix, req.Delimiter, req.StartAfter, req.Limit,
	)
	if err != nil {
		return nil, err
	}

	out := &fs.ListObjectsResponse{
		Objects:        make([]fs.Object, 0, len(sidecars)),
		CommonPrefixes: prefixes,
		IsTruncated:    more,
	}

	for _, sc := range sidecars {
		out.Objects = append(out.Objects, fs.Object{
			Key:          sc.Key,
			Size:         sc.Size,
			LastModified: sc.Modified,
			ETag:         sc.ETag,
			Owner:        sc.Owner,
		})
	}

	// The last entry of the page in folded order, which is where the next page
	// resumes. Objects and prefixes are reported separately but interleave by
	// key, so the later of the two tails is the boundary.
	if n := len(out.Objects); n > 0 {
		out.NextStartAfter = out.Objects[n-1].Key
	}

	if n := len(prefixes); n > 0 && prefixes[n-1] > out.NextStartAfter {
		out.NextStartAfter = prefixes[n-1]
	}

	return out, nil
}

// listFromSidecars gathers the bucket and folds it, the original path.
//
// The gather is cluster-wide — every disk scanned, every sidecar read — so a
// page costs what the whole bucket costs. It stays because it is the only
// answer available while a node is still building its index, and because it
// needs nothing of a peer beyond what the very first version of the cluster
// could do.
func (s *Storage) listFromSidecars(ctx context.Context, req *fs.ListObjectsRequest) (*fs.ListObjectsResponse, error) {
	sidecars, err := s.coord.ListObjects(ctx, req.Bucket, req.Prefix)
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
			Owner:        sc.Owner,
		})
	}

	return req.FoldPage(objects), nil
}

// PutObject implements fs.Storage. The conditional check and the write happen
// under the object's key lock, so concurrent conditional PUTs on this node
// resolve to a single winner.
func (s *Storage) PutObject(ctx context.Context, req *fs.PutObjectRequest) (*fs.PutObjectResponse, error) {
	if err := s.mustBucket(ctx, req.Bucket); err != nil {
		return nil, err
	}

	// The per-key lock exists to make a conditional write's check and write
	// atomic, so an unconditional PUT — which has nothing to check — must not
	// take it. Holding it across coord.Put would hold it across the read of the
	// request body, letting one slow client block every other writer of that
	// key, and deadlocking a client that issues a second PUT to the same key
	// while it is still streaming the first: the second waits for the lock, the
	// first waits for body bytes the client will not send until the second
	// returns, and both die on the read timeout.
	if cond := req.Conditions(); !cond.IsZero() {
		l := s.locks.of(req.Bucket, req.Key)
		l.Lock()

		defer l.Unlock()

		state, err := s.objectState(ctx, req.Bucket, req.Key)
		if err != nil {
			return nil, err
		}

		if err := cond.CheckWrite(state); err != nil {
			return nil, err
		}
	}

	sc, err := s.coord.Put(ctx, &PutRequest{
		Bucket:     req.Bucket,
		Key:        req.Key,
		Size:       req.Size,
		Body:       req.Reader,
		Metadata:   req.Metadata,
		Tags:       append([]fs.Tag(nil), req.Tags...),
		ACL:        req.ACL,
		Owner:      req.Owner,
		ContentMD5: req.ContentMD5,
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
		TagCount:     len(sc.Tags),
	}, nil
}

// DeleteObject implements fs.Storage.
func (s *Storage) DeleteObject(ctx context.Context, bucket, key string) error {
	return s.DeleteObjectIf(ctx, bucket, key, fs.Conditions{})
}

// DeleteObjectIf implements fs.ConditionalDeleter. The condition is evaluated
// under the same per-key lock a conditional write takes, so no write can land
// between the check and the delete.
func (s *Storage) DeleteObjectIf(ctx context.Context, bucket, key string, cond fs.Conditions) error {
	if err := s.mustBucket(ctx, bucket); err != nil {
		return err
	}

	if !cond.IsZero() {
		l := s.locks.of(bucket, key)
		l.Lock()

		defer l.Unlock()

		state, err := s.objectState(ctx, bucket, key)
		if err != nil {
			return err
		}

		if err := cond.CheckDelete(state); err != nil {
			return err
		}
	}

	return mapObjectErr(s.coord.Delete(ctx, bucket, key), key)
}

// objectState reports the state conditional requests are evaluated against.
// The caller must hold the object's lock.
func (s *Storage) objectState(ctx context.Context, bucket, key string) (fs.ObjectState, error) {
	switch cur, err := s.coord.Stat(ctx, bucket, key); {
	case err == nil:
		return fs.ObjectState{
			Exists:       true,
			ETag:         cur.ETag,
			Size:         cur.Size,
			LastModified: cur.Modified,
		}, nil
	case errors.Is(err, ErrNotFound):
		return fs.ObjectState{}, nil
	default:
		return fs.ObjectState{}, err
	}
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

// SetObjectACL implements fs.Storage.
func (s *Storage) SetObjectACL(ctx context.Context, bucket, key string, acl fs.ACL) error {
	return s.updateObject(ctx, bucket, key, func(sc *Sidecar) {
		sc.ACL = acl
	})
}

// ObjectOwner implements fs.Storage.
func (s *Storage) ObjectOwner(ctx context.Context, bucket, key string) (fs.Owner, error) {
	sc, err := s.statObject(ctx, bucket, key)
	if err != nil {
		return fs.Owner{}, err
	}

	return sc.Owner, nil
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

// ObjectAttributes implements fs.ObjectAttributer, reading the part layout the
// completion recorded in the sidecar.
func (s *Storage) ObjectAttributes(ctx context.Context, bucket, key string) (*fs.ObjectAttributes, error) {
	sc, err := s.statObject(ctx, bucket, key)
	if err != nil {
		return nil, err
	}

	return &fs.ObjectAttributes{
		ETag:         sc.ETag,
		Size:         sc.Size,
		LastModified: sc.Modified,
		Parts:        append([]fs.ObjectPart(nil), sc.Parts...),
		UploadID:     sc.UploadID,
	}, nil
}

// BucketCORS implements fs.BucketCORSStore.
func (s *Storage) BucketCORS(ctx context.Context, bucket string) ([]fs.CORSRule, error) {
	return s.coord.BucketCORS(ctx, bucket)
}

// SetBucketCORS implements fs.BucketCORSStore.
func (s *Storage) SetBucketCORS(ctx context.Context, bucket string, rules []fs.CORSRule) error {
	return s.coord.SetBucketCORS(ctx, bucket, rules)
}

// DeleteBucketCORS implements fs.BucketCORSStore.
func (s *Storage) DeleteBucketCORS(ctx context.Context, bucket string) error {
	return s.coord.SetBucketCORS(ctx, bucket, nil)
}

// BucketPublicAccessBlock implements fs.BucketSettingsStore.
func (s *Storage) BucketPublicAccessBlock(ctx context.Context, bucket string) (*fs.PublicAccessBlock, error) {
	info, err := s.coord.fetchBucket(ctx, s.coord.topo.Topology(), bucket)
	if err != nil {
		return nil, err
	}

	return info.PublicAccessBlock, nil
}

// SetBucketPublicAccessBlock implements fs.BucketSettingsStore.
func (s *Storage) SetBucketPublicAccessBlock(ctx context.Context, bucket string, block *fs.PublicAccessBlock) error {
	return s.coord.SetBucketPublicAccessBlock(ctx, bucket, block)
}

// BucketObjectOwnership implements fs.BucketSettingsStore.
func (s *Storage) BucketObjectOwnership(ctx context.Context, bucket string) (string, error) {
	info, err := s.coord.fetchBucket(ctx, s.coord.topo.Topology(), bucket)
	if err != nil {
		return "", err
	}

	return info.ObjectOwnership, nil
}

// SetBucketObjectOwnership implements fs.BucketSettingsStore.
func (s *Storage) SetBucketObjectOwnership(ctx context.Context, bucket, ownership string) error {
	return s.coord.SetBucketObjectOwnership(ctx, bucket, ownership)
}
