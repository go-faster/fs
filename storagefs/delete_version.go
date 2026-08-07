package storagefs

import (
	"context"
	"os"
	"path/filepath"
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

// DeleteObjectVersionIf implements fs.ConditionalVersionDeleter.
//
// The condition is evaluated under putMu — the same lock that serializes writes
// to the key — so no writer can slip between the check and the delete. That is
// the whole reason this lives in the backend rather than in the S3 layer: a
// handler that read the current version, checked it and then deleted would race
// exactly the write the client used If-Match to guard against.
func (s *Storage) DeleteObjectVersionIf(
	ctx context.Context, bucket, key, versionID string, cond fs.Conditions,
) (fs.DeleteResult, error) {
	if cond.IsZero() {
		return s.DeleteObjectVersion(ctx, bucket, key, versionID)
	}

	if !s.bucketExists(bucket) {
		return fs.DeleteResult{}, fs.ErrBucketNotFound
	}

	s.putMu.Lock()
	defer s.putMu.Unlock()

	state, err := s.deleteTargetState(bucket, key, versionID)
	if err != nil {
		return fs.DeleteResult{}, err
	}

	if err := cond.CheckDelete(state); err != nil {
		return fs.DeleteResult{}, err
	}

	// Past the check, still holding putMu: the delete itself takes no lock, so
	// the pair is atomic against every other writer to this key.
	return s.deleteVersionLocked(ctx, bucket, key, versionID)
}

// deleteTargetState describes the version a delete would act on: the one
// versionID names, or the key's current version.
//
// A key that has never existed reports absent, and CheckDelete lets every
// condition pass against it — deleting what is not there is a success in S3
// whatever the condition says.
//
// A key whose current version is a **delete marker** is a different case and
// not an absent one. S3 answers `If-Match: *` on it with success and any
// specific ETag with 412, which is the behavior of something that exists and
// matches nothing. Reporting it absent instead makes every condition pass, so a
// guarded delete against an already-deleted key is accepted without the guard
// ever being evaluated.
func (s *Storage) deleteTargetState(bucket, key, versionID string) (fs.ObjectState, error) {
	if versionID == "" {
		if !s.versionedBucket(bucket) {
			// Not versioned: the object lives in the plain key tree.
			return s.currentObjectState(bucket, key, filepath.Join(s.root, bucket, objectRelPath(key)))
		}

		sc, deleted, err := s.currentVersion(bucket, key)
		if err != nil {
			return fs.ObjectState{}, err
		}

		if deleted {
			// A marker over real content is not the same as a marker over
			// nothing, and S3 distinguishes them: with content beneath, the key
			// exists-and-matches-nothing, so "*" holds and any specific
			// condition is refused. With only markers — a delete of a key that
			// was never written, which still leaves one — there is nothing to
			// guard and every condition passes, because deleting what is not
			// there is a success whatever the condition says.
			content, err := s.hasContentVersion(bucket, key)
			if err != nil || !content {
				return fs.ObjectState{}, err
			}

			return fs.ObjectState{Exists: true}, nil
		}

		if sc == nil {
			return fs.ObjectState{}, nil
		}

		return versionState(sc), nil
	}

	if versionID == fs.NullVersionID {
		return s.currentObjectState(bucket, key, filepath.Join(s.root, bucket, objectRelPath(key)))
	}

	sc, err := s.readVersionSidecar(bucket, key, versionID)
	if err != nil || sc == nil || sc.DeleteMarker {
		return fs.ObjectState{}, err
	}

	return versionState(sc), nil
}

// hasContentVersion reports whether the key has any version that is not a
// delete marker.
func (s *Storage) hasContentVersion(bucket, key string) (bool, error) {
	ids, err := s.listVersionIDs(bucket, key)
	if err != nil {
		return false, err
	}

	for _, id := range ids {
		sc, err := s.readVersionSidecar(bucket, key, id)
		if err != nil {
			return false, err
		}

		if sc != nil && !sc.DeleteMarker {
			return true, nil
		}
	}

	return false, nil
}

// versionState converts a version's sidecar to the state a condition is
// evaluated against.
func versionState(sc *versionSidecar) fs.ObjectState {
	state := fs.ObjectState{Exists: true, ETag: sc.ETag, Size: sc.Size}

	// A sidecar written before Modified was recorded, or one whose timestamp
	// cannot be parsed, reports a zero time rather than failing the delete:
	// x-amz-if-match-last-modified-time then does not hold, which refuses the
	// delete, where ETag and size still decide normally.
	if t, err := time.Parse(time.RFC3339Nano, sc.Modified); err == nil {
		state.LastModified = t
	}

	return state
}

// deleteVersionLocked is DeleteObjectVersion's body, for a caller already
// holding putMu.
func (s *Storage) deleteVersionLocked(ctx context.Context, bucket, key, versionID string) (fs.DeleteResult, error) {
	if versionID != "" {
		return s.deleteOneVersion(ctx, bucket, key, versionID)
	}

	if !s.versionedBucket(bucket) {
		return fs.DeleteResult{}, s.DeleteObject(ctx, bucket, key)
	}

	return s.insertDeleteMarker(bucket, key)
}
