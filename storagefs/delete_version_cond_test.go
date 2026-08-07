package storagefs_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/storagefs"
)

// condStore returns a versioned bucket holding one object, and its ETag.
func condStore(t *testing.T) (store *storagefs.Storage, etag string) {
	t.Helper()

	ctx := t.Context()

	s, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, s.CreateBucket(ctx, "b"))
	require.NoError(t, s.SetBucketVersioning(ctx, "b", fs.VersioningEnabled))

	body := "hello"
	put, err := s.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: "b", Key: "k", Reader: strings.NewReader(body), Size: int64(len(body)),
	})
	require.NoError(t, err)
	require.NotEmpty(t, put.ETag)

	return s, put.ETag
}

// TestDeleteObjectVersionIfRefusesOnMismatch is the guard on issue #233.
//
// A conditional delete against a versioned bucket must evaluate its condition.
// Before the fix it did not: the request took the unversioned path, failed to
// find the object there (it lives in the version tree), and the miss was
// reported as the success a delete of an absent key gives — so a client that
// guarded a delete with If-Match was told the delete happened and no condition
// was ever checked.
func TestDeleteObjectVersionIfRefusesOnMismatch(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s, etag := condStore(t)

	_, err := s.DeleteObjectVersionIf(ctx, "b", "k", "", fs.Conditions{IfMatch: `"deadbeef"`})
	require.ErrorIs(t, err, fs.ErrPreconditionFailed, "a mismatched If-Match must refuse the delete")

	// And the object is still there, unmarked: a refused delete must not have
	// written a tombstone on the way to refusing.
	got, err := s.GetObject(ctx, "b", "k")
	require.NoError(t, err)
	require.NoError(t, got.Reader.Close())
	require.Equal(t, etag, got.ETag)
}

// TestDeleteObjectVersionIfDeletesOnMatch is the other half: a condition that
// holds must not block the delete.
func TestDeleteObjectVersionIfDeletesOnMatch(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s, etag := condStore(t)

	result, err := s.DeleteObjectVersionIf(ctx, "b", "k", "", fs.Conditions{IfMatch: etag})
	require.NoError(t, err)
	require.True(t, result.DeleteMarker, "a delete on a versioned bucket leaves a marker")
	require.NotEmpty(t, result.VersionID)

	_, err = s.GetObject(ctx, "b", "k")
	require.Error(t, err, "the key must no longer resolve")
}

// TestDeleteObjectVersionIfNamedVersion covers ?versionId=: the condition is
// evaluated against the version named, not against whatever is current.
func TestDeleteObjectVersionIfNamedVersion(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s, first := condStore(t)

	// A second version, so "current" and "the named one" differ.
	second := "goodbye"
	put, err := s.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: "b", Key: "k", Reader: strings.NewReader(second), Size: int64(len(second)),
	})
	require.NoError(t, err)

	versions, err := s.ListObjectVersions(ctx, &fs.ListObjectVersionsRequest{Bucket: "b"})
	require.NoError(t, err)
	require.Len(t, versions.Versions, 2)

	var firstID string

	for _, v := range versions.Versions {
		if v.ETag == first {
			firstID = v.VersionID
		}
	}

	require.NotEmpty(t, firstID)

	// The current version's ETag must not satisfy a condition on the older one.
	_, err = s.DeleteObjectVersionIf(ctx, "b", "k", firstID, fs.Conditions{IfMatch: put.ETag})
	require.ErrorIs(t, err, fs.ErrPreconditionFailed)

	_, err = s.DeleteObjectVersionIf(ctx, "b", "k", firstID, fs.Conditions{IfMatch: first})
	require.NoError(t, err)
}

// TestDeleteObjectVersionIfSizeAndTime covers the other two guards, which are
// evaluated by the same path and would otherwise be assumed rather than shown.
func TestDeleteObjectVersionIfSizeAndTime(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s, _ := condStore(t)

	wrong := int64(999)
	_, err := s.DeleteObjectVersionIf(ctx, "b", "k", "", fs.Conditions{Size: &wrong})
	require.ErrorIs(t, err, fs.ErrPreconditionFailed)

	right := int64(len("hello"))
	_, err = s.DeleteObjectVersionIf(ctx, "b", "k", "", fs.Conditions{Size: &right})
	require.NoError(t, err)
}

// TestDeleteObjectVersionIfDeleteMarkerSemantics pins the distinction the
// s3-tests encode, which is subtle enough to be re-broken by anyone
// simplifying deleteTargetState.
//
// A delete marker over real content means the key exists and matches nothing:
// If-Match "*" holds, any specific ETag is refused. A delete marker over
// nothing — the marker a delete of a never-written key still leaves — means the
// key is absent, and every condition passes, because deleting what is not there
// is a success whatever the condition says.
func TestDeleteObjectVersionIfDeleteMarkerSemantics(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Run("MarkerOverNothing", func(t *testing.T) {
		s, err := storagefs.New(t.TempDir())
		require.NoError(t, err)
		require.NoError(t, s.CreateBucket(ctx, "b"))
		require.NoError(t, s.SetBucketVersioning(ctx, "b", fs.VersioningEnabled))

		// Deleting a key that was never written leaves a marker.
		_, err = s.DeleteObjectVersionIf(ctx, "b", "ghost", "", fs.Conditions{IfMatch: "*"})
		require.NoError(t, err)

		// And a second guarded delete still succeeds: there is nothing there to
		// guard, so the condition has nothing to refuse.
		_, err = s.DeleteObjectVersionIf(ctx, "b", "ghost", "", fs.Conditions{IfMatch: `"deadbeef"`})
		require.NoError(t, err, "a marker over nothing must not make conditions fail")
	})

	t.Run("MarkerOverContent", func(t *testing.T) {
		s, etag := condStore(t)

		// Delete the object, so a marker is current over real content.
		_, err := s.DeleteObjectVersionIf(ctx, "b", "k", "", fs.Conditions{IfMatch: etag})
		require.NoError(t, err)

		// "*" holds: something is there.
		_, err = s.DeleteObjectVersionIf(ctx, "b", "k", "", fs.Conditions{IfMatch: "*"})
		require.NoError(t, err)

		// A specific ETag does not: the marker matches nothing.
		_, err = s.DeleteObjectVersionIf(ctx, "b", "k", "", fs.Conditions{IfMatch: `"deadbeef"`})
		require.ErrorIs(t, err, fs.ErrPreconditionFailed,
			"a marker over content exists and matches nothing")
	})
}
