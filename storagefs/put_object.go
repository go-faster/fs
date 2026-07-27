package storagefs

import (
	"context"
	"crypto/md5" //nolint:gosec // MD5 is required for S3 ETag compatibility.
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

func (s *Storage) PutObject(ctx context.Context, req *fs.PutObjectRequest) (*fs.PutObjectResponse, error) {
	bucketPath := filepath.Join(s.root, req.Bucket)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		return nil, fs.ErrBucketNotFound
	}

	// On a versioned bucket the write lands in the key's version directory
	// under its own ID; the plain key tree is not touched at all, so the two
	// layouts never have to agree about anything.
	versioned := s.versionedBucket(req.Bucket)

	var versionID string

	objectPath := filepath.Join(bucketPath, objectRelPath(req.Key))

	if versioned {
		versionID = fs.NewVersionID()
		objectPath = s.versionBodyPath(req.Bucket, req.Key, versionID)
	}

	if err := os.MkdirAll(filepath.Dir(objectPath), defaultDirPermissions); err != nil {
		return nil, errors.Wrap(err, "create object directory")
	}

	// A key is a directory here, so it has to exist before the content can be
	// put inside it — but a write that is then refused (a failed precondition,
	// a digest mismatch) must not leave that directory behind as debris no
	// listing reports and no delete removes.
	committed := false

	defer func() {
		if !committed {
			pruneEmptyDirs(filepath.Dir(objectPath), bucketPath)
		}
	}()

	// Stream to a staging temp file while hashing, then rename into place so a
	// partially written object is never visible in the bucket; the sidecar is
	// written after the object (sidecar-less files stay readable).
	tmp, err := s.newObjectTemp()
	if err != nil {
		return nil, err
	}

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}

	hash := md5.New() //nolint:gosec // MD5 is required for S3 ETag compatibility.

	// The ETag stays the MD5 of the *plaintext*, which is what AWS reports for
	// an SSE-S3 object and what keeps every existing ETag, conditional-write
	// and multipart formula working unchanged. So the digest is taken from the
	// body as it arrives, before any encryption.
	enc, err := s.encryptTo(tmp, req.ServerSideEncryption, 0)
	if err != nil {
		cleanup()
		return nil, err
	}

	size, err := io.Copy(io.MultiWriter(enc, hash), req.Reader)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write object: %w", err)
	}

	// Seals the trailing partial chunk; without it the last chunk is never
	// written at all.
	if err := enc.Close(); err != nil {
		cleanup()
		return nil, err
	}

	// Flush object data to stable storage before it becomes visible (per policy).
	if err := s.syncFile(tmp); err != nil {
		cleanup()
		return nil, err
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, errors.Wrap(err, "close object")
	}

	etag := hex.EncodeToString(hash.Sum(nil))

	// The body is on disk but not yet visible: this is the last moment at which
	// a digest mismatch can be refused without anyone having been able to read
	// the object.
	if req.ContentMD5 != "" && req.ContentMD5 != etag {
		_ = os.Remove(tmp.Name())
		return nil, fs.ErrBadDigest
	}

	// Finalize under putMu so the conditional-write check and the rename are
	// atomic against other writers to this key (the body is already on disk).
	s.putMu.Lock()
	defer s.putMu.Unlock()

	if cond := req.Conditions(); !cond.IsZero() {
		state, err := s.objectStateFor(req.Bucket, req.Key, bucketPath, versioned)
		if err != nil {
			_ = os.Remove(tmp.Name())
			return nil, err
		}

		if err := cond.CheckWrite(state); err != nil {
			_ = os.Remove(tmp.Name())
			return nil, err
		}
	}

	if err := os.Rename(tmp.Name(), objectPath); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, errors.Wrap(err, "rename object")
	}

	// Persist the rename (per policy) so the object is durably visible.
	if err := s.syncDir(filepath.Dir(objectPath)); err != nil {
		return nil, err
	}

	sc := newSidecar(req.Key, etag, etag, req.Metadata, req.Tags, req.ACL, req.Owner)
	sc.Encryption = enc.finish(size)

	// A versioned write records the same sidecar under the version's own id,
	// encryption record included, so a version reads back exactly as the
	// current object would.
	if versioned {
		if err := s.writeVersionSidecar(req.Bucket, req.Key, &versionSidecar{
			sidecar:   *sc,
			VersionID: versionID,
			Size:      size,
			Modified:  time.Now().UTC().Format(time.RFC3339Nano),
		}); err != nil {
			return nil, err
		}
	} else if err := s.writeSidecar(req.Bucket, sc); err != nil {
		return nil, err
	}

	committed = true

	return &fs.PutObjectResponse{
		ETag:                 etag,
		VersionID:            versionID,
		ServerSideEncryption: req.ServerSideEncryption,
	}, nil
}

// currentObjectState reports the state conditional requests are evaluated
// against: whether the object at path exists, and its ETag, size and
// modification time. The ETag prefers the sidecar's stored value and falls back
// to recompute-on-read.
func (s *Storage) currentObjectState(bucket, key, path string) (fs.ObjectState, error) {
	info, statErr := os.Stat(path)
	if os.IsNotExist(statErr) {
		return fs.ObjectState{}, nil
	}

	if statErr != nil {
		return fs.ObjectState{}, errors.Wrap(statErr, "stat object")
	}

	etag, err := s.objectETag(bucket, key, path, info)
	if err != nil {
		return fs.ObjectState{}, err
	}

	return fs.ObjectState{
		Exists:       true,
		ETag:         etag,
		Size:         info.Size(),
		LastModified: info.ModTime(),
	}, nil
}
