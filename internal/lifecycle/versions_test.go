package lifecycle_test

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/internal/lifecycle"
)

// versionedEngine returns an engine with testBucket versioned and rules set.
// The engine is the backend that versions; the memory one does not.
func versionedEngine(t *testing.T, rules ...fs.LifecycleRule) *engine.Engine {
	t.Helper()

	e, err := engine.Open(t.TempDir(), engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })

	ctx := t.Context()
	require.NoError(t, e.CreateBucket(ctx, testBucket))
	require.NoError(t, e.SetBucketVersioning(ctx, testBucket, fs.VersioningEnabled))
	require.NoError(t, e.SetBucketLifecycle(ctx, testBucket, rules))

	return e
}

// versions lists every version of the bucket, newest first within each key.
func versions(t *testing.T, e *engine.Engine) []fs.ObjectVersion {
	t.Helper()

	var (
		out []fs.ObjectVersion
		req = fs.ListObjectVersionsRequest{Bucket: testBucket}
	)

	for {
		page, err := e.ListObjectVersions(t.Context(), &req)
		require.NoError(t, err)

		out = append(out, page.Versions...)

		if !page.IsTruncated {
			return out
		}

		req.KeyMarker, req.VersionIDMarker = page.NextKeyMarker, page.NextVersionIDMarker
	}
}

// bodies names what each listed version is: its content, or "marker".
func bodies(t *testing.T, e *engine.Engine, key string) []string {
	t.Helper()

	var out []string

	for _, v := range versions(t, e) {
		if v.Key != key {
			continue
		}

		if v.DeleteMarker {
			out = append(out, "marker")

			continue
		}

		res, err := e.GetObjectVersion(t.Context(), testBucket, key, v.VersionID)
		require.NoError(t, err)

		data, err := io.ReadAll(res.Reader)
		require.NoError(t, err)
		require.NoError(t, res.Reader.Close())

		out = append(out, string(data))
	}

	return out
}

func sweepAt(t *testing.T, storage fs.Storage, after time.Duration) lifecycle.Report {
	t.Helper()

	sweeper := &lifecycle.Sweeper{
		Storage: storage,
		Now:     func() time.Time { return time.Now().Add(after) },
	}

	report, err := sweeper.Sweep(t.Context())
	require.NoError(t, err)
	require.Zero(t, report.Failed)

	return report
}

// TestSweepNoncurrentDays removes a noncurrent version NoncurrentDays after it
// was replaced, and leaves the current version and other prefixes alone.
func TestSweepNoncurrentDays(t *testing.T) {
	t.Parallel()

	e := versionedEngine(t, fs.LifecycleRule{ID: "history", Status: fs.LifecycleEnabled, Prefix: "docs/", NoncurrentDays: 3})

	put(t, e, "docs/a", "v1")
	put(t, e, "docs/a", "v2")
	put(t, e, "other/b", "v1")
	put(t, e, "other/b", "v2")

	report := sweepAt(t, e, 2*fs.LifecycleDay)
	require.Zero(t, report.Noncurrent)
	require.Equal(t, []string{"v2", "v1"}, bodies(t, e, "docs/a"))

	report = sweepAt(t, e, 4*fs.LifecycleDay)
	require.Equal(t, 1, report.Noncurrent)
	require.Zero(t, report.Expired)
	require.Equal(t, []string{"v2"}, bodies(t, e, "docs/a"))
	require.Equal(t, []string{"v2", "v1"}, bodies(t, e, "other/b"))
}

// TestSweepKeepsNewerNoncurrent keeps the NewerNoncurrentVersions newest
// noncurrent versions whatever their age.
func TestSweepKeepsNewerNoncurrent(t *testing.T) {
	t.Parallel()

	e := versionedEngine(t, fs.LifecycleRule{
		ID: "keep2", Status: fs.LifecycleEnabled, NoncurrentDays: 1, NewerNoncurrentVersions: 2,
	})

	for _, v := range []string{"v1", "v2", "v3", "v4"} {
		put(t, e, "k", v)
	}

	report := sweepAt(t, e, 3*fs.LifecycleDay)
	require.Equal(t, 1, report.Noncurrent)
	require.Equal(t, []string{"v4", "v3", "v2"}, bodies(t, e, "k"))
}

// TestSweepExpiredMarker removes the noncurrent version under a delete marker
// and then the marker, left with nothing under it, in the same pass.
func TestSweepExpiredMarker(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	e := versionedEngine(t, fs.LifecycleRule{
		ID: "all", Status: fs.LifecycleEnabled, NoncurrentDays: 1, ExpiredObjectDeleteMarker: true,
	})

	put(t, e, "gone", "v1")

	_, err := e.DeleteObjectVersion(ctx, testBucket, "gone", "")
	require.NoError(t, err)

	report := sweepAt(t, e, 0)
	require.Zero(t, report.Markers, "a marker with a version under it stays")
	require.Equal(t, []string{"marker", "v1"}, bodies(t, e, "gone"))

	report = sweepAt(t, e, 2*fs.LifecycleDay)
	require.Equal(t, 1, report.Noncurrent)
	require.Equal(t, 1, report.Markers)
	require.Empty(t, versions(t, e))
}

// TestSweepMarkerUnderDays: an Expiration Days rule cleans up expired object
// delete markers once they are its age, without ExpiredObjectDeleteMarker.
func TestSweepMarkerUnderDays(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	e := versionedEngine(t, fs.LifecycleRule{ID: "days", Status: fs.LifecycleEnabled, ExpirationDays: 2})

	put(t, e, "k", "v1")

	marker, err := e.DeleteObjectVersion(ctx, testBucket, "k", "")
	require.NoError(t, err)

	for _, v := range versions(t, e) {
		if v.VersionID != marker.VersionID {
			_, err := e.DeleteObjectVersion(ctx, testBucket, "k", v.VersionID)
			require.NoError(t, err)
		}
	}

	require.Equal(t, []string{"marker"}, bodies(t, e, "k"))

	report := sweepAt(t, e, fs.LifecycleDay)
	require.Zero(t, report.Markers)

	report = sweepAt(t, e, 3*fs.LifecycleDay)
	require.Equal(t, 1, report.Markers)
	require.Empty(t, versions(t, e))
}

// TestSweepExpiresCurrentVersioned expires the current version with a delete
// marker, as before, and counts it as the newest noncurrent version for the
// rule keeping one.
func TestSweepExpiresCurrentVersioned(t *testing.T) {
	t.Parallel()

	e := versionedEngine(t, fs.LifecycleRule{
		ID: "both", Status: fs.LifecycleEnabled, Prefix: "k",
		ExpirationDays: 1, NoncurrentDays: 1, NewerNoncurrentVersions: 1,
	})

	put(t, e, "k", "v1")
	put(t, e, "k", "v2")

	report := sweepAt(t, e, 3*fs.LifecycleDay)
	require.Equal(t, 1, report.Expired)
	require.Equal(t, 1, report.Noncurrent)
	require.Equal(t, []string{"marker", "v2"}, bodies(t, e, "k"))
}

// TestSweepNoncurrentAcrossPages: a key split by the listing's page boundary
// keeps what the walk knows of it. Two single-version keys and 350 keys of
// three versions put the boundary at entry 1000, between the second and the
// oldest version of k0332: only the walk's memory of the key says the oldest
// was replaced, and when.
func TestSweepNoncurrentAcrossPages(t *testing.T) {
	t.Parallel()

	e := versionedEngine(t, fs.LifecycleRule{
		ID: "keep1", Status: fs.LifecycleEnabled, NoncurrentDays: 1, NewerNoncurrentVersions: 1,
	})

	put(t, e, "a0", "v1")
	put(t, e, "a1", "v1")

	const keys = 350

	for i := range keys {
		for _, v := range []string{"v1", "v2", "v3"} {
			put(t, e, fmt.Sprintf("k%04d", i), v)
		}
	}

	report := sweepAt(t, e, 2*fs.LifecycleDay)
	require.Equal(t, keys, report.Noncurrent)

	left := map[string][]fs.ObjectVersion{}
	for _, v := range versions(t, e) {
		left[v.Key] = append(left[v.Key], v)
	}

	require.Len(t, left, keys+2)

	for i := range keys {
		key := fmt.Sprintf("k%04d", i)
		require.Len(t, left[key], 2, key)
	}

	require.Equal(t, []string{"v3", "v2"}, bodies(t, e, "k0332"))
}

// TestSweepNullNoncurrent: a version written before versioning was enabled is
// the null version, and once replaced it is noncurrent like any other.
func TestSweepNullNoncurrent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	e, err := engine.Open(t.TempDir(), engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })

	require.NoError(t, e.CreateBucket(ctx, testBucket))
	put(t, e, "k", "null")
	require.NoError(t, e.SetBucketVersioning(ctx, testBucket, fs.VersioningEnabled))
	put(t, e, "k", "v2")
	require.NoError(t, e.SetBucketVersioning(ctx, testBucket, fs.VersioningSuspended))
	require.NoError(t, e.SetBucketLifecycle(ctx, testBucket, []fs.LifecycleRule{
		{ID: "n", Status: fs.LifecycleEnabled, NoncurrentDays: 1},
	}))

	vs := versions(t, e)
	require.Len(t, vs, 2)
	require.Equal(t, fs.NullVersionID, vs[1].VersionID)

	report := sweepAt(t, e, 2*fs.LifecycleDay)
	require.Equal(t, 1, report.Noncurrent)
	require.Equal(t, []string{"v2"}, bodies(t, e, "k"))
}

// racingVersioner removes a version itself between the listing and the
// sweep's removal, as a client would.
type racingVersioner struct {
	*engine.Engine
	removed bool
}

func (r *racingVersioner) ListObjectVersions(ctx context.Context, req *fs.ListObjectVersionsRequest) (*fs.ListObjectVersionsResponse, error) {
	page, err := r.Engine.ListObjectVersions(ctx, req)
	if err != nil || r.removed {
		return page, err
	}

	r.removed = true

	for _, v := range page.Versions {
		if !v.IsLatest {
			if _, err := r.DeleteObjectVersion(ctx, req.Bucket, v.Key, v.VersionID); err != nil {
				return nil, err
			}
		}
	}

	return page, nil
}

// TestSweepVersionRemovedMidPass: a version a client removed after the
// listing is not a failure, and is not counted as the sweep's.
func TestSweepVersionRemovedMidPass(t *testing.T) {
	t.Parallel()

	e := versionedEngine(t, fs.LifecycleRule{ID: "n", Status: fs.LifecycleEnabled, NoncurrentDays: 1})

	put(t, e, "k", "v1")
	put(t, e, "k", "v2")

	// The engine answers removing a version already gone as done, so the
	// sweep may count it; what matters is that the pass does not fail.
	sweepAt(t, &racingVersioner{Engine: e}, 2*fs.LifecycleDay)
	require.Equal(t, []string{"v2"}, bodies(t, e, "k"))
}

// TestSweepStats adds the passes up for the metrics.
func TestSweepStats(t *testing.T) {
	t.Parallel()

	e := versionedEngine(t, fs.LifecycleRule{ID: "n", Status: fs.LifecycleEnabled, NoncurrentDays: 1})

	put(t, e, "k", "v1")
	put(t, e, "k", "v2")

	sweeper := &lifecycle.Sweeper{
		Storage: e,
		Now:     func() time.Time { return time.Now().Add(2 * fs.LifecycleDay) },
	}

	_, err := sweeper.Sweep(t.Context())
	require.NoError(t, err)

	_, err = sweeper.Sweep(t.Context())
	require.NoError(t, err)

	st := sweeper.Stats()
	require.Equal(t, 1, st.Total.Noncurrent)
	require.Equal(t, 2, st.Total.Buckets)
	require.Zero(t, st.Failed)
}
