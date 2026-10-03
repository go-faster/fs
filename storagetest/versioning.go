package storagetest

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// The versioning cases run for a backend that implements fs.Versioner, and
// pin the S3 model: every write while enabled is a version; a delete without
// a version ID writes a marker and loses nothing; a delete with one removes
// exactly that version; the "null" version is what was written while
// versioning was unset or suspended, and there is at most one of it.

// otherETag matches no content the cases write.
const otherETag = `"0000"`

func versioner(t *testing.T, storage fs.Storage) fs.Versioner {
	t.Helper()

	v, ok := storage.(fs.Versioner)
	if !ok {
		t.Skip("backend does not implement fs.Versioner")
	}

	require.NoError(t, storage.CreateBucket(t.Context(), testBucket))

	return v
}

func putVersion(t *testing.T, storage fs.Storage, content string) *fs.PutObjectResponse {
	t.Helper()

	resp, err := storage.PutObject(t.Context(), &fs.PutObjectRequest{
		Bucket: testBucket, Key: testKey, Reader: bytes.NewReader([]byte(content)), Size: int64(len(content)),
	})
	require.NoError(t, err)

	return resp
}

func readAll(t *testing.T, resp *fs.GetObjectResponse) string {
	t.Helper()

	defer func() { _ = resp.Reader.Close() }()

	b, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)

	return string(b)
}

func versionsOf(t *testing.T, v fs.Versioner) []fs.ObjectVersion {
	t.Helper()

	resp, err := v.ListObjectVersions(t.Context(), &fs.ListObjectVersionsRequest{Bucket: testBucket, Prefix: testKey})
	require.NoError(t, err)

	var out []fs.ObjectVersion

	for _, ov := range resp.Versions {
		if ov.Key == testKey {
			out = append(out, ov)
		}
	}

	return out
}

func ids(versions []fs.ObjectVersion) []string {
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = v.VersionID
	}

	return out
}

func testVersioningEnabledKeepsEveryVersion(t *testing.T, storage fs.Storage) {
	v := versioner(t, storage)
	ctx := t.Context()

	require.NoError(t, v.SetBucketVersioning(ctx, testBucket, fs.VersioningEnabled))

	first := putVersion(t, storage, "one")
	second := putVersion(t, storage, "two")

	require.True(t, fs.ValidVersionID(first.VersionID), "an enabled bucket reports the version written")
	require.NotEqual(t, first.VersionID, second.VersionID)

	cur, err := storage.GetObject(ctx, testBucket, testKey)
	require.NoError(t, err)
	assert.Equal(t, "two", readAll(t, cur))
	assert.Equal(t, second.VersionID, cur.VersionID)

	old, err := v.GetObjectVersion(ctx, testBucket, testKey, first.VersionID)
	require.NoError(t, err)
	assert.Equal(t, "one", readAll(t, old), "an earlier version is still readable by its ID")

	versions := versionsOf(t, v)
	require.Equal(t, []string{second.VersionID, first.VersionID}, ids(versions), "newest first")
	assert.True(t, versions[0].IsLatest)
	assert.False(t, versions[1].IsLatest)
}

func testVersioningDeleteMarker(t *testing.T, storage fs.Storage) {
	v := versioner(t, storage)
	ctx := t.Context()

	require.NoError(t, v.SetBucketVersioning(ctx, testBucket, fs.VersioningEnabled))

	put := putVersion(t, storage, "kept")

	del, err := v.DeleteObjectVersion(ctx, testBucket, testKey, "")
	require.NoError(t, err)
	require.True(t, del.DeleteMarker, "a delete without a version ID writes a marker")
	require.True(t, fs.ValidVersionID(del.VersionID))

	_, err = storage.GetObject(ctx, testBucket, testKey)
	require.ErrorIs(t, err, fs.ErrObjectNotFound)

	_, err = v.GetObjectVersion(ctx, testBucket, testKey, del.VersionID)
	require.ErrorIs(t, err, fs.ErrMethodNotAllowedOnDeleteMarker)

	list, err := storage.ListObjects(ctx, &fs.ListObjectsRequest{Bucket: testBucket})
	require.NoError(t, err)
	assert.Empty(t, list.Objects, "a key under a marker is not listed")

	versions := versionsOf(t, v)
	require.Equal(t, []string{del.VersionID, put.VersionID}, ids(versions))
	assert.True(t, versions[0].DeleteMarker)
	assert.True(t, versions[0].IsLatest)

	// Removing the marker brings the key back.
	_, err = v.DeleteObjectVersion(ctx, testBucket, testKey, del.VersionID)
	require.NoError(t, err)

	cur, err := storage.GetObject(ctx, testBucket, testKey)
	require.NoError(t, err)
	assert.Equal(t, "kept", readAll(t, cur))
}

func testVersioningPermanentDelete(t *testing.T, storage fs.Storage) {
	v := versioner(t, storage)
	ctx := t.Context()

	require.NoError(t, v.SetBucketVersioning(ctx, testBucket, fs.VersioningEnabled))

	first := putVersion(t, storage, "one")
	second := putVersion(t, storage, "two")

	res, err := v.DeleteObjectVersion(ctx, testBucket, testKey, first.VersionID)
	require.NoError(t, err)
	assert.Equal(t, first.VersionID, res.VersionID)
	assert.False(t, res.DeleteMarker)

	_, err = v.GetObjectVersion(ctx, testBucket, testKey, first.VersionID)
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
	assert.Equal(t, []string{second.VersionID}, ids(versionsOf(t, v)))

	// Deleting a version that is not there is a success, like deleting a key
	// that is not there.
	_, err = v.DeleteObjectVersion(ctx, testBucket, testKey, first.VersionID)
	require.NoError(t, err)

	// Removing the current version makes the one below current.
	_, err = v.DeleteObjectVersion(ctx, testBucket, testKey, second.VersionID)
	require.NoError(t, err)

	_, err = storage.GetObject(ctx, testBucket, testKey)
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
	assert.Empty(t, versionsOf(t, v))
}

func testVersioningNullBeforeEnable(t *testing.T, storage fs.Storage) {
	v := versioner(t, storage)
	ctx := t.Context()

	putVersion(t, storage, "before")
	require.NoError(t, v.SetBucketVersioning(ctx, testBucket, fs.VersioningEnabled))

	after := putVersion(t, storage, "after")

	require.Equal(t, []string{after.VersionID, fs.NullVersionID}, ids(versionsOf(t, v)),
		"what was written before the first enable is the null version")

	null, err := v.GetObjectVersion(ctx, testBucket, testKey, fs.NullVersionID)
	require.NoError(t, err)
	assert.Equal(t, "before", readAll(t, null))

	_, err = v.DeleteObjectVersion(ctx, testBucket, testKey, fs.NullVersionID)
	require.NoError(t, err)
	assert.Equal(t, []string{after.VersionID}, ids(versionsOf(t, v)))
}

func testVersioningSuspended(t *testing.T, storage fs.Storage) {
	v := versioner(t, storage)
	ctx := t.Context()

	require.NoError(t, v.SetBucketVersioning(ctx, testBucket, fs.VersioningEnabled))

	kept := putVersion(t, storage, "versioned")

	require.NoError(t, v.SetBucketVersioning(ctx, testBucket, fs.VersioningSuspended))

	putVersion(t, storage, "null one")
	putVersion(t, storage, "null two")

	cur, err := storage.GetObject(ctx, testBucket, testKey)
	require.NoError(t, err)
	assert.Equal(t, "null two", readAll(t, cur), "a suspended write is current")

	require.Equal(t, []string{fs.NullVersionID, kept.VersionID}, ids(versionsOf(t, v)),
		"one null version, replaced by each suspended write; the enabled one is kept")

	old, err := v.GetObjectVersion(ctx, testBucket, testKey, kept.VersionID)
	require.NoError(t, err)
	assert.Equal(t, "versioned", readAll(t, old))
}

func testVersioningUnsetDeleteLeavesNoMarker(t *testing.T, storage fs.Storage) {
	v := versioner(t, storage)
	ctx := t.Context()

	putVersion(t, storage, "gone")
	require.NoError(t, storage.DeleteObject(ctx, testBucket, testKey))
	require.NoError(t, v.SetBucketVersioning(ctx, testBucket, fs.VersioningEnabled))

	assert.Empty(t, versionsOf(t, v),
		"a delete on a never-versioned bucket removes the object; there is no history to show")
}

func testVersioningConditionalDelete(t *testing.T, storage fs.Storage) {
	v := versioner(t, storage)

	cv, ok := storage.(fs.ConditionalVersionDeleter)
	if !ok {
		t.Skip("backend does not implement fs.ConditionalVersionDeleter")
	}

	ctx := t.Context()
	require.NoError(t, v.SetBucketVersioning(ctx, testBucket, fs.VersioningEnabled))

	put := putVersion(t, storage, "guarded")

	_, err := cv.DeleteObjectVersionIf(ctx, testBucket, testKey, "", fs.Conditions{IfMatch: otherETag})
	require.ErrorIs(t, err, fs.ErrPreconditionFailed, "a delete guarded by another ETag is refused")

	_, err = cv.DeleteObjectVersionIf(ctx, testBucket, testKey, put.VersionID, fs.Conditions{IfMatch: otherETag})
	require.ErrorIs(t, err, fs.ErrPreconditionFailed, "and so is one naming the version")

	del, err := cv.DeleteObjectVersionIf(ctx, testBucket, testKey, "", fs.Conditions{IfMatch: `"` + put.ETag + `"`})
	require.NoError(t, err)
	require.True(t, del.DeleteMarker)

	// Under a marker over real content the key exists and matches nothing.
	_, err = cv.DeleteObjectVersionIf(ctx, testBucket, testKey, "", fs.Conditions{IfMatch: `"` + put.ETag + `"`})
	require.ErrorIs(t, err, fs.ErrPreconditionFailed)

	_, err = cv.DeleteObjectVersionIf(ctx, testBucket, testKey, "", fs.Conditions{IfMatch: "*"})
	require.NoError(t, err)

	// A key that never existed has nothing to guard.
	_, err = cv.DeleteObjectVersionIf(context.Background(), testBucket, "never", "", fs.Conditions{IfMatch: otherETag})
	require.NoError(t, err)
}

// pageVersions lists every version a page at a time, following the markers.
func pageVersions(t *testing.T, v fs.Versioner, req fs.ListObjectVersionsRequest) (versions []fs.ObjectVersion, prefixes []string) {
	t.Helper()

	for range 100 {
		resp, err := v.ListObjectVersions(t.Context(), &req)
		require.NoError(t, err)

		versions = append(versions, resp.Versions...)
		prefixes = append(prefixes, resp.CommonPrefixes...)

		if !resp.IsTruncated {
			return versions, prefixes
		}

		req.KeyMarker, req.VersionIDMarker = resp.NextKeyMarker, resp.NextVersionIDMarker
	}

	t.Fatal("listing never ended")

	return nil, nil
}

// testVersioningPages: paging a listing one entry at a time returns exactly
// what one page does — including past a null version in the middle of a
// key's history, and past a common prefix.
func testVersioningPages(t *testing.T, storage fs.Storage) {
	v := versioner(t, storage)
	ctx := t.Context()

	require.NoError(t, v.SetBucketVersioning(ctx, testBucket, fs.VersioningEnabled))
	putVersion(t, storage, "oldest")
	require.NoError(t, v.SetBucketVersioning(ctx, testBucket, fs.VersioningSuspended))
	putVersion(t, storage, "null")
	require.NoError(t, v.SetBucketVersioning(ctx, testBucket, fs.VersioningEnabled))
	putVersion(t, storage, "newest")

	for _, k := range []string{"dir/a", "dir/b", "zz"} {
		_, err := storage.PutObject(ctx, &fs.PutObjectRequest{Bucket: testBucket, Key: k, Reader: bytes.NewReader([]byte(k)), Size: int64(len(k))})
		require.NoError(t, err)
	}

	for _, req := range []fs.ListObjectVersionsRequest{
		{Bucket: testBucket},
		{Bucket: testBucket, Delimiter: "/"},
	} {
		whole, err := v.ListObjectVersions(ctx, &req)
		require.NoError(t, err)
		require.False(t, whole.IsTruncated)

		req.Limit = 1
		versions, prefixes := pageVersions(t, v, req)

		assert.Equal(t, whole.Versions, versions, "delimiter %q", req.Delimiter)
		assert.Equal(t, whole.CommonPrefixes, prefixes, "delimiter %q", req.Delimiter)
	}

	keyVersions := 0

	for _, ov := range versionsOf(t, v) {
		if ov.Key == testKey {
			keyVersions++
		}
	}

	require.Equal(t, 3, keyVersions, "the key's history: newest, null, oldest")
}
