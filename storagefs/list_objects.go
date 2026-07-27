package storagefs

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// ListObjects returns one page of a bucket's objects, sorted by key.
//
// The walk still visits the whole bucket before a page can be cut: keys are
// arbitrary, so directory order is not key order and the set has to be sorted
// before StartAfter means anything. Paging bounds what the caller holds and
// what crosses the S3 layer, not yet what this backend reads — serving a page
// without the walk needs an index, which is the next step, not this one.
//
// NB: bucket and prefix are already sanitized.
func (s *Storage) ListObjects(ctx context.Context, req *fs.ListObjectsRequest) (*fs.ListObjectsResponse, error) {
	objects, err := s.listPlainObjects(ctx, req.Bucket, req.Prefix)
	if err != nil {
		return nil, err
	}

	// A versioned bucket keeps its content under .versions, so the walk above
	// finds only what predates the first enable. Merge in each key's current
	// version — and let it win, because it is newer than the plain-path object
	// of the same name by construction.
	objects, err = s.mergeCurrentVersions(req.Bucket, req.Prefix, objects)
	if err != nil {
		return nil, err
	}

	return req.FoldPage(objects), nil
}

// listPlainObjects walks the bucket's key tree: everything written while the
// bucket was not versioned. It is deliberately unaware of versions, because
// the version listing needs exactly this — the objects that predate the first
// enable, which are the "null" versions.
func (s *Storage) listPlainObjects(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
	bucketPath := filepath.Join(s.root, bucket)

	var objects []fs.Object

	err := filepath.Walk(bucketPath, func(path string, info os.FileInfo, err error) error {
		select {
		case <-ctx.Done():
			// Stop walking on context done.
			return ctx.Err()
		default:
		}

		if os.IsNotExist(err) {
			return fs.ErrBucketNotFound
		}

		if err != nil {
			return errors.Wrap(err, "walk objects")
		}

		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(bucketPath, path)
		if err != nil {
			return errors.Wrap(err, "determine relative path")
		}

		// Only the reserved content leaf is an object; every other entry is
		// part of the key's directory chain.
		key, ok := keyFromContentPath(relPath)
		if !ok {
			return nil
		}

		if prefix == "" || strings.HasPrefix(key, prefix) {
			etag, owner, err := s.objectETagOwner(bucket, key, path, info)
			if err != nil {
				return errors.Wrap(err, "etag")
			}

			objects = append(objects, fs.Object{
				Key:          key,
				Size:         info.Size(),
				LastModified: info.ModTime(),
				ETag:         etag,
				Owner:        owner,
			})
		}

		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "list objects")
	}

	return objects, nil
}
