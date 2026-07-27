package storagefs

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// versionedStore returns a store with a versioning-enabled bucket.
func versionedStore(t *testing.T) *Storage {
	t.Helper()

	s, err := New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(t.Context(), "bucket"))
	require.NoError(t, s.SetBucketVersioning(t.Context(), "bucket", fs.VersioningEnabled))

	return s
}

// putVersion writes one version of the test key and returns its id.
func putVersion(t *testing.T, s *Storage, body []byte) string {
	t.Helper()

	resp, err := s.PutObject(t.Context(), &fs.PutObjectRequest{
		Reader: bytes.NewReader(body), Bucket: "bucket", Key: "k", Size: int64(len(body)),
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.VersionID)

	return resp.VersionID
}

// TestDeleteInsertsMarkerAndKeepsVersions is the property that makes a delete
// undoable: nothing is removed, the key simply stops resolving.
func TestDeleteInsertsMarkerAndKeepsVersions(t *testing.T) {
	s := versionedStore(t)
	ctx := t.Context()

	v1 := putVersion(t, s, []byte("first"))
	v2 := putVersion(t, s, []byte("second"))

	result, err := s.DeleteObjectVersion(ctx, "bucket", "k", "")
	require.NoError(t, err)
	require.True(t, result.DeleteMarker, "a delete on a versioned bucket must leave a marker")
	require.NotEmpty(t, result.VersionID)
	require.NotEqual(t, v2, result.VersionID, "the marker is its own version")

	// The key no longer resolves...
	_, err = s.GetObject(ctx, "bucket", "k")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)

	// ...but every version written before it is still readable by id.
	for id, want := range map[string]string{v1: "first", v2: "second"} {
		resp, err := s.GetObjectVersion(ctx, "bucket", "k", id)
		require.NoError(t, err, "version %s became unreadable", id)

		body, err := io.ReadAll(resp.Reader)
		_ = resp.Reader.Close()

		require.NoError(t, err)
		require.Equal(t, want, string(body))
	}
}

// TestDeleteMarkerIsListedAsSuch: a client enumerating versions has to be able
// to see the tombstone, or it cannot tell a deleted key from a missing one.
func TestDeleteMarkerIsListedAsSuch(t *testing.T) {
	s := versionedStore(t)
	ctx := t.Context()

	putVersion(t, s, []byte("content"))

	result, err := s.DeleteObjectVersion(ctx, "bucket", "k", "")
	require.NoError(t, err)

	page, err := s.ListObjectVersions(ctx, &fs.ListObjectVersionsRequest{Bucket: "bucket", Limit: 10})
	require.NoError(t, err)

	var markers, contents int

	for _, v := range page.Versions {
		if v.DeleteMarker {
			markers++

			require.Equal(t, result.VersionID, v.VersionID)
			require.True(t, v.IsLatest, "the marker is the newest version")
		} else {
			contents++
		}
	}

	require.Equal(t, 1, markers)
	require.Equal(t, 1, contents)
}

// TestDeleteVersionRemovesBytes: naming a version is the only way content
// actually leaves a versioned bucket.
func TestDeleteVersionRemovesBytes(t *testing.T) {
	s := versionedStore(t)
	ctx := t.Context()

	v1 := putVersion(t, s, []byte("first"))
	v2 := putVersion(t, s, []byte("second"))

	result, err := s.DeleteObjectVersion(ctx, "bucket", "k", v1)
	require.NoError(t, err)
	require.False(t, result.DeleteMarker, "removing a version is not a marker")
	require.Equal(t, v1, result.VersionID)

	_, err = s.GetObjectVersion(ctx, "bucket", "k", v1)
	require.ErrorIs(t, err, fs.ErrObjectNotFound)

	// The other version, and the key itself, are untouched.
	resp, err := s.GetObject(ctx, "bucket", "k")
	require.NoError(t, err)

	body, err := io.ReadAll(resp.Reader)
	_ = resp.Reader.Close()

	require.NoError(t, err)
	require.Equal(t, "second", string(body))
	require.Equal(t, v2, resp.VersionID)
}

// TestDeleteMarkerCanBeRemoved is the undo: deleting the marker makes the key
// resolve again, to the version that was current before it.
func TestDeleteMarkerCanBeRemoved(t *testing.T) {
	s := versionedStore(t)
	ctx := t.Context()

	putVersion(t, s, []byte("first"))
	v2 := putVersion(t, s, []byte("second"))

	marker, err := s.DeleteObjectVersion(ctx, "bucket", "k", "")
	require.NoError(t, err)

	_, err = s.GetObject(ctx, "bucket", "k")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)

	_, err = s.DeleteObjectVersion(ctx, "bucket", "k", marker.VersionID)
	require.NoError(t, err)

	resp, err := s.GetObject(ctx, "bucket", "k")
	require.NoError(t, err)

	body, err := io.ReadAll(resp.Reader)
	_ = resp.Reader.Close()

	require.NoError(t, err)
	require.Equal(t, "second", string(body))
	require.Equal(t, v2, resp.VersionID)
}

// TestDeleteAbsentVersionSucceeds: S3 deletes are idempotent, and a version
// that is already gone is not an error.
func TestDeleteAbsentVersionSucceeds(t *testing.T) {
	s := versionedStore(t)

	putVersion(t, s, []byte("content"))

	_, err := s.DeleteObjectVersion(t.Context(), "bucket", "k", "e739dce046af78fd5f45c226404012e8")
	require.NoError(t, err)
}

// TestDeleteOnUnversionedBucketRemoves: the same call on a bucket that is not
// versioned deletes outright and reports nothing, so a caller does not have to
// ask about bucket state first.
func TestDeleteOnUnversionedBucketRemoves(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, s.CreateBucket(ctx, "bucket"))

	_, err = s.PutObject(ctx, &fs.PutObjectRequest{
		Reader: bytes.NewReader([]byte("x")), Bucket: "bucket", Key: "k", Size: 1,
	})
	require.NoError(t, err)

	result, err := s.DeleteObjectVersion(ctx, "bucket", "k", "")
	require.NoError(t, err)
	require.False(t, result.DeleteMarker)
	require.Empty(t, result.VersionID)

	_, err = s.GetObject(ctx, "bucket", "k")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
}
