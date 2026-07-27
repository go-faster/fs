package storagefs

import (
	"context"
	"os"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// DeleteObjectVersion implements fs.Versioner.
//
// A delete on a versioned bucket does not remove anything: it writes a marker
// that becomes the newest version, so the key stops resolving while every
// version written before it stays readable by id. That is the whole point of
// versioning — a delete has to be undoable — and it is why the only way bytes
// actually leave is a delete that names a version.
func (s *Storage) DeleteObjectVersion(ctx context.Context, bucket, key, versionID string) (fs.DeleteResult, error) {
	if !s.bucketExists(bucket) {
		return fs.DeleteResult{}, fs.ErrBucketNotFound
	}

	if versionID != "" {
		return s.deleteOneVersion(ctx, bucket, key, versionID)
	}

	if !s.versionedBucket(bucket) {
		// Not versioned: an ordinary delete, and nothing to report.
		return fs.DeleteResult{}, s.DeleteObject(ctx, bucket, key)
	}

	return s.insertDeleteMarker(bucket, key)
}

// insertDeleteMarker writes the tombstone that hides a key.
func (s *Storage) insertDeleteMarker(bucket, key string) (fs.DeleteResult, error) {
	id := fs.NewVersionID()

	sc := &versionSidecar{
		sidecar:      sidecar{Version: sidecarVersion, Key: key},
		VersionID:    id,
		DeleteMarker: true,
		Modified:     time.Now().UTC().Format(time.RFC3339Nano),
	}

	// The marker is a sidecar with no body. It sorts newest-first like any
	// other version, so it becomes current by the same rule and needs no
	// separate "is deleted" flag on the key.
	if err := s.writeVersionSidecar(bucket, key, sc); err != nil {
		return fs.DeleteResult{}, err
	}

	return fs.DeleteResult{VersionID: id, DeleteMarker: true}, nil
}

// deleteOneVersion permanently removes a single version.
func (s *Storage) deleteOneVersion(ctx context.Context, bucket, key, versionID string) (fs.DeleteResult, error) {
	if !fs.ValidVersionID(versionID) {
		return fs.DeleteResult{}, fs.ErrObjectNotFound
	}

	// "null" names the object written before versioning was enabled, which
	// still sits in the plain key tree rather than under .versions.
	if versionID == fs.NullVersionID {
		if err := s.DeleteObject(ctx, bucket, key); err != nil && !errors.Is(err, fs.ErrObjectNotFound) {
			return fs.DeleteResult{}, err
		}

		// A "null" sidecar may also exist under .versions for an object
		// adopted into the chain; remove it too so the version stops listing.
		_ = os.Remove(s.versionSidecarPath(bucket, key, versionID))
		_ = os.Remove(s.versionBodyPath(bucket, key, versionID))

		return fs.DeleteResult{VersionID: versionID}, nil
	}

	sc, err := s.readVersionSidecar(bucket, key, versionID)
	if err != nil {
		return fs.DeleteResult{}, err
	}

	if sc == nil {
		// S3 treats deleting an absent version as success, the same as
		// deleting an absent key.
		return fs.DeleteResult{VersionID: versionID}, nil
	}

	// The sidecar goes first: it is what makes a version visible, so removing
	// it before the body means a crash between the two leaves an unreferenced
	// file rather than a version whose bytes are gone.
	if err := os.Remove(s.versionSidecarPath(bucket, key, versionID)); err != nil && !os.IsNotExist(err) {
		return fs.DeleteResult{}, errors.Wrap(err, "remove version sidecar")
	}

	if err := os.Remove(s.versionBodyPath(bucket, key, versionID)); err != nil && !os.IsNotExist(err) {
		return fs.DeleteResult{}, errors.Wrap(err, "remove version body")
	}

	pruneEmptyDirs(s.versionDir(bucket, key), s.versionsRoot(bucket))

	return fs.DeleteResult{VersionID: versionID, DeleteMarker: sc.DeleteMarker}, nil
}
