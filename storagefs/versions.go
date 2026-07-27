package storagefs

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"crypto/sha256"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// Versions live outside the bucket's key tree entirely:
//
//	.versions/<bucket>/<sha256(key)>/<versionID>        the body
//	.versions/<bucket>/<sha256(key)>/<versionID>.json   its sidecar
//
// Hashing the key means the layout is immune to everything the plain key tree
// has to think about — length, separators, keys that are prefixes of other
// keys — and the version directory holds the original key so a walk never has
// to reverse the hash.
//
// A bucket that has never been versioned has no .versions directory and pays
// nothing. Once versioning is enabled, writes go here instead of to the plain
// path; an object still sitting at the plain path predates the first enable
// and *is* the "null" version, adopted lazily rather than by a migration.
const versionsDir = ".versions"

// versionSidecar is the per-version metadata document. It is a superset of the
// unversioned sidecar: same fields, plus what identifies the version.
type versionSidecar struct {
	sidecar

	// VersionID names this version. Stored as well as encoded in the file
	// name so a sidecar read in isolation is self-describing.
	VersionID string `json:"version_id"`
	// DeleteMarker reports that this version records a deletion rather than
	// content. A marker has no body file.
	DeleteMarker bool `json:"delete_marker,omitempty"`
	// Size is the body length, kept here because a marker has no file to stat
	// and a listing needs the number.
	Size int64 `json:"size"`
	// Modified is the version's creation time, for listings.
	Modified string `json:"modified"`
}

// versionsRoot is the directory holding every versioned key of a bucket.
func (s *Storage) versionsRoot(bucket string) string {
	return filepath.Join(s.root, versionsDir, bucket)
}

// versionDir is the directory holding one key's versions.
func (s *Storage) versionDir(bucket, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.versionsRoot(bucket), hex.EncodeToString(sum[:]))
}

// versionBodyPath is where a version's content lives.
func (s *Storage) versionBodyPath(bucket, key, versionID string) string {
	return filepath.Join(s.versionDir(bucket, key), versionID)
}

// versionSidecarPath is where a version's metadata lives.
func (s *Storage) versionSidecarPath(bucket, key, versionID string) string {
	return filepath.Join(s.versionDir(bucket, key), versionID+".json")
}

// writeVersionSidecar persists a version's metadata. As everywhere else, the
// sidecar is written last: a body without one is invisible.
func (s *Storage) writeVersionSidecar(bucket, key string, sc *versionSidecar) error {
	if err := os.MkdirAll(s.versionDir(bucket, key), defaultDirPermissions); err != nil {
		return errors.Wrap(err, "create version directory")
	}

	data, err := json.Marshal(sc)
	if err != nil {
		return errors.Wrap(err, "marshal version sidecar")
	}

	return s.atomicWrite(s.versionSidecarPath(bucket, key, sc.VersionID), data)
}

// readVersionSidecar loads one version's metadata; (nil, nil) when absent.
func (s *Storage) readVersionSidecar(bucket, key, versionID string) (*versionSidecar, error) {
	data, err := os.ReadFile(s.versionSidecarPath(bucket, key, versionID)) //nolint:gosec // Path derives from a hash and a validated version ID.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, errors.Wrap(err, "read version sidecar")
	}

	var sc versionSidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		// A corrupt sidecar hides its version rather than breaking the key:
		// the other versions, and the object as a whole, stay readable.
		return nil, nil //nolint:nilerr // Deliberate: an unreadable version is an absent one.
	}

	return &sc, nil
}

// listVersionIDs returns a key's version IDs, newest first.
//
// That order is the ID format doing its job: the IDs sort newest-first
// lexically, so a plain sort of the directory entries is already the order the
// read path wants. No index, no cached pointer.
func (s *Storage) listVersionIDs(bucket, key string) ([]string, error) {
	entries, err := os.ReadDir(s.versionDir(bucket, key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, errors.Wrap(err, "read version directory")
	}

	ids := make([]string, 0, len(entries)/2) //nolint:mnd // Two files per version.

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}

		if id := strings.TrimSuffix(name, ".json"); fs.ValidVersionID(id) {
			ids = append(ids, id)
		}
	}

	sort.Strings(ids)

	return ids, nil
}

// currentVersion resolves the version a request without a version ID
// addresses: the newest one that is not a delete marker, plus whether the
// newest version *is* a marker — which is a 404 that has to say so.
//
// This is a directory read and a scan, with no cached "latest" pointer to keep
// in step. That is the design decision the whole layout exists to support: a
// pointer would have to be updated in a second step, and the gap between the
// two steps is exactly where a non-transactional store loses track of which
// version is current.
func (s *Storage) currentVersion(bucket, key string) (*versionSidecar, bool, error) {
	ids, err := s.listVersionIDs(bucket, key)
	if err != nil {
		return nil, false, err
	}

	for i, id := range ids {
		sc, err := s.readVersionSidecar(bucket, key, id)
		if err != nil {
			return nil, false, err
		}

		if sc == nil {
			continue
		}

		if sc.DeleteMarker {
			// Only the newest version being a marker means the key is
			// deleted; a marker further down is just history.
			return nil, i == 0, nil
		}

		return sc, false, nil
	}

	return nil, false, nil
}

// versionedBucket reports whether writes to this bucket should create
// versions.
func (s *Storage) versionedBucket(bucket string) bool {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	return s.readBucketMeta(bucket).Versioning == fs.VersioningEnabled
}

// objectStateFor resolves the state a conditional write is judged against.
//
// On a versioned bucket that is the current *version*, not the plain path —
// which a versioned bucket never writes to. Getting this wrong would make
// If-Match against a versioned object compare with an object that is not
// there, and quietly turn every conditional write into a create.
func (s *Storage) objectStateFor(bucket, key, bucketPath string, versioned bool) (fs.ObjectState, error) {
	if !versioned {
		return s.currentObjectState(bucket, key, filepath.Join(bucketPath, objectRelPath(key)))
	}

	current, _, err := s.currentVersion(bucket, key)
	if err != nil || current == nil {
		return fs.ObjectState{}, err
	}

	modified, _ := time.Parse(time.RFC3339Nano, current.Modified)

	return fs.ObjectState{
		Exists:       true,
		ETag:         current.ETag,
		Size:         current.Size,
		LastModified: modified,
	}, nil
}

// currentVersionResponse serves the current version of a key, or (nil, nil)
// when the key has no versions at all — in which case the caller falls back to
// the plain path, where a pre-versioning object may still be sitting.
//
// A current version that is a delete marker is ErrObjectNotFound: the key is
// deleted, even though its history is not.
func (s *Storage) currentVersionResponse(bucket, key string) (*fs.GetObjectResponse, error) {
	current, deleted, err := s.currentVersion(bucket, key)
	if err != nil {
		return nil, err
	}

	if deleted {
		return nil, fs.ErrObjectNotFound
	}

	if current == nil {
		return nil, nil
	}

	f, err := os.Open(s.versionBodyPath(bucket, key, current.VersionID)) //nolint:gosec // Path derives from a hash and a stored version ID.
	if err != nil {
		if os.IsNotExist(err) {
			// The sidecar names a body that is not there: the version is
			// unreadable, not the object. Treating it as absent keeps the rest
			// of the chain serviceable.
			return nil, fs.ErrObjectNotFound
		}

		return nil, errors.Wrap(err, "open version")
	}

	modified, _ := time.Parse(time.RFC3339Nano, current.Modified)

	return &fs.GetObjectResponse{
		Reader:       f,
		Size:         current.Size,
		LastModified: modified,
		ETag:         current.ETag,
		Metadata:     current.metadata(),
		VersionID:    current.VersionID,
		TagCount:     len(current.Tags),
	}, nil
}

// GetObjectVersion implements fs.Versioner.
func (s *Storage) GetObjectVersion(
	_ context.Context, bucket, key, versionID string,
) (*fs.GetObjectResponse, error) {
	if !s.bucketExists(bucket) {
		return nil, fs.ErrBucketNotFound
	}

	if !fs.ValidVersionID(versionID) {
		return nil, fs.ErrObjectNotFound
	}

	// "null" addresses the object written before versioning was enabled, which
	// still sits in the plain key tree. It is a version like any other from the
	// client's side, and the only one whose body is not under .versions.
	if versionID == fs.NullVersionID {
		if sc, err := s.readVersionSidecar(bucket, key, versionID); err == nil && sc != nil {
			return s.openVersion(bucket, key, sc)
		}

		return s.GetObject(context.Background(), bucket, key)
	}

	sc, err := s.readVersionSidecar(bucket, key, versionID)
	if err != nil {
		return nil, err
	}

	if sc == nil {
		return nil, fs.ErrObjectNotFound
	}

	return s.openVersion(bucket, key, sc)
}

// openVersion serves one version's body.
func (s *Storage) openVersion(bucket, key string, sc *versionSidecar) (*fs.GetObjectResponse, error) {
	// A delete marker records that the key was deleted; there is nothing to
	// serve, and S3 reports it as a method that does not apply rather than as a
	// missing key.
	if sc.DeleteMarker {
		return nil, fs.ErrMethodNotAllowedOnDeleteMarker
	}

	f, err := os.Open(s.versionBodyPath(bucket, key, sc.VersionID)) //nolint:gosec // Path derives from a hash and a validated version ID.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fs.ErrObjectNotFound
		}

		return nil, errors.Wrap(err, "open version")
	}

	modified, _ := time.Parse(time.RFC3339Nano, sc.Modified)

	return &fs.GetObjectResponse{
		Reader:       f,
		Size:         sc.Size,
		LastModified: modified,
		ETag:         sc.ETag,
		Metadata:     sc.metadata(),
		VersionID:    sc.VersionID,
		TagCount:     len(sc.Tags),
	}, nil
}

// ListObjectVersions implements fs.Versioner.
//
// It walks the bucket's version directories, and then the plain key tree for
// objects that predate the first enable — those are reported as the "null"
// version, which is what they are.
func (s *Storage) ListObjectVersions(
	ctx context.Context, req *fs.ListObjectVersionsRequest,
) (*fs.ListObjectVersionsResponse, error) {
	if !s.bucketExists(req.Bucket) {
		return nil, fs.ErrBucketNotFound
	}

	byKey, err := s.gatherVersions(ctx, req.Bucket)
	if err != nil {
		return nil, err
	}

	if err := s.addNullVersions(req.Bucket, byKey); err != nil {
		return nil, err
	}

	return req.FoldVersionPage(byKey), nil
}

// gatherVersions reads every version of every key in a bucket, newest first.
func (s *Storage) gatherVersions(ctx context.Context, bucket string) (map[string][]fs.ObjectVersion, error) {
	entries, err := os.ReadDir(s.versionsRoot(bucket))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]fs.ObjectVersion{}, nil
		}

		return nil, errors.Wrap(err, "read versions directory")
	}

	byKey := make(map[string][]fs.ObjectVersion, len(entries))

	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if !entry.IsDir() {
			continue
		}

		if err := s.gatherKeyVersions(bucket, entry.Name(), byKey); err != nil {
			return nil, err
		}
	}

	return byKey, nil
}

// gatherKeyVersions reads one key's version directory, named by the hash of
// the key. The key itself comes from the sidecars, which is why they store it:
// a hash cannot be reversed.
func (s *Storage) gatherKeyVersions(bucket, hashed string, byKey map[string][]fs.ObjectVersion) error {
	dir := filepath.Join(s.versionsRoot(bucket), hashed)

	files, err := os.ReadDir(dir)
	if err != nil {
		return errors.Wrap(err, "read version directory")
	}

	ids := make([]string, 0, len(files))

	for _, f := range files {
		if id := strings.TrimSuffix(f.Name(), ".json"); id != f.Name() && fs.ValidVersionID(id) {
			ids = append(ids, id)
		}
	}

	sort.Strings(ids) // Newest first: that is what the ID format buys.

	for i, id := range ids {
		data, err := os.ReadFile(filepath.Join(dir, id+".json")) //nolint:gosec // Path built from a hash and a validated ID.
		if err != nil {
			continue
		}

		var sc versionSidecar
		if err := json.Unmarshal(data, &sc); err != nil {
			continue
		}

		modified, _ := time.Parse(time.RFC3339Nano, sc.Modified)

		byKey[sc.Key] = append(byKey[sc.Key], fs.ObjectVersion{
			Key:          sc.Key,
			VersionID:    sc.VersionID,
			IsLatest:     i == 0,
			DeleteMarker: sc.DeleteMarker,
			Size:         sc.Size,
			ETag:         sc.ETag,
			LastModified: modified,
			Owner:        sc.owner(),
		})
	}

	return nil
}

// addNullVersions reports objects still in the plain key tree as the "null"
// version. A key that also has versions keeps them: the null one is the oldest
// and never the latest, because every versioned write is newer than the enable
// that preceded it.
func (s *Storage) addNullVersions(bucket string, byKey map[string][]fs.ObjectVersion) error {
	// The plain walk, not ListObjects: the latter merges current versions in,
	// which would synthesize a second "null" entry for every versioned key.
	plain, err := s.listPlainObjects(context.Background(), bucket, "")
	if err != nil {
		return err
	}

	for _, o := range plain {
		versions := byKey[o.Key]

		byKey[o.Key] = append(versions, fs.ObjectVersion{
			Key:          o.Key,
			VersionID:    fs.NullVersionID,
			IsLatest:     len(versions) == 0,
			Size:         o.Size,
			ETag:         o.ETag,
			LastModified: o.LastModified,
			Owner:        o.Owner,
		})
	}

	return nil
}

// mergeCurrentVersions adds each versioned key's current version to a plain
// listing, and drops keys whose current version is a delete marker.
//
// A key present in both places — an object written before versioning was
// enabled and versions written after — is reported once, from its newest
// version. Reporting both would show one key twice.
func (s *Storage) mergeCurrentVersions(bucket, prefix string, objects []fs.Object) ([]fs.Object, error) {
	entries, err := os.ReadDir(s.versionsRoot(bucket))
	if err != nil {
		if os.IsNotExist(err) {
			return objects, nil
		}

		return nil, errors.Wrap(err, "read versions directory")
	}

	byKey := make(map[string][]fs.ObjectVersion, len(entries))

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		if err := s.gatherKeyVersions(bucket, entry.Name(), byKey); err != nil {
			return nil, err
		}
	}

	if len(byKey) == 0 {
		return objects, nil
	}

	versioned := make(map[string]fs.Object, len(byKey))
	deleted := make(map[string]struct{})

	for key, versions := range byKey {
		if len(versions) == 0 {
			continue
		}

		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}

		current := versions[0]
		if current.DeleteMarker {
			deleted[key] = struct{}{}
			continue
		}

		versioned[key] = fs.Object{
			Key:          current.Key,
			Size:         current.Size,
			LastModified: current.LastModified,
			ETag:         current.ETag,
			Owner:        current.Owner,
		}
	}

	merged := make([]fs.Object, 0, len(objects)+len(versioned))

	for _, o := range objects {
		if _, superseded := versioned[o.Key]; superseded {
			continue
		}

		if _, gone := deleted[o.Key]; gone {
			continue
		}

		merged = append(merged, o)
	}

	for _, o := range versioned {
		merged = append(merged, o)
	}

	return merged, nil
}
