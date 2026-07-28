package storagefs

import (
	"context"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

func (s *Storage) GetObject(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
	objectPath := filepath.Join(s.root, bucket, objectRelPath(key))

	// Check if bucket exists
	bucketPath := filepath.Join(s.root, bucket)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		return nil, fs.ErrBucketNotFound
	}

	// A key with versions is served from the newest one. This is checked
	// before the plain path rather than after, because a bucket that was
	// versioned and then suspended can have both: the versions written while
	// it was enabled, and an older object from before the first enable. The
	// versions are newer by construction.
	switch current, err := s.currentVersionResponse(bucket, key); {
	case err != nil:
		return nil, err
	case current != nil:
		return current, nil
	}

	// #nosec G304 -- objectPath is constructed from validated bucket and key.
	f, err := os.Open(objectPath)
	if os.IsNotExist(err) {
		return nil, fs.ErrObjectNotFound
	}

	if err != nil {
		return nil, errors.Wrap(err, "open object")
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, errors.Wrap(err, "stat object")
	}

	// Verify-on-read: recompute and check the checksum before serving so corrupt
	// content is never returned (opt-in; costs an extra full read).
	if s.verifyReads {
		if err := s.verifyContent(bucket, key, objectPath); err != nil {
			_ = f.Close()
			return nil, err
		}
	}

	resp := &fs.GetObjectResponse{
		Reader:       f,
		Size:         info.Size(),
		LastModified: info.ModTime(),
	}

	// The sidecar carries the stored ETag and metadata; files without one
	// (pre-sidecar data directories) fall back to recompute-on-read.
	sc, err := s.readSidecar(bucket, key)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	if sc != nil {
		resp.ETag = sc.ETag
		resp.Metadata = sc.metadata()
		resp.TagCount = len(sc.Tags)
		resp.ChecksumAlgorithm = sc.ChecksumAlgorithm
		resp.Checksum = sc.ClientChecksum
		resp.ChecksumType = sc.ChecksumType
	}

	// An encrypted body is served through a decrypting reader, and reports the
	// plaintext size: the stored size carries a tag per chunk and is nobody
	// else's business.
	if sc != nil && sc.Encryption != nil {
		reader, err := s.openEncrypted(f, sc.Encryption, 0)
		if err != nil {
			_ = f.Close()
			return nil, err
		}

		resp.Reader = reader
		resp.Size = sc.Encryption.PlainSize
		resp.ServerSideEncryption = sc.Encryption.Algorithm
	}

	if resp.ETag == "" {
		etag, err := s.etagFor(objectPath, info)
		if err != nil {
			_ = f.Close()
			return nil, errors.Wrap(err, "etag")
		}

		resp.ETag = etag
	}

	return resp, nil
}
