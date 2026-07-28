package objindex_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/objindex"
)

// open returns a fresh index under a temp dir, closed at the end of the test.
func open(t *testing.T) *objindex.Index {
	t.Helper()

	idx, err := objindex.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	return idx
}

// buckets drains the store's bucket stream into the slice the assertions want.
func buckets(t *testing.T, idx *objindex.Index) []string {
	t.Helper()

	var out []string

	require.NoError(t, idx.Buckets(t.Context(), func(bucket string) error {
		out = append(out, bucket)

		return nil
	}))

	return out
}

// entry builds a record; seq orders it against others for the same key.
func entry(bucket, key string, size, seq int64) metastore.Entry {
	return metastore.Entry{
		Bucket:     bucket,
		Key:        key,
		Size:       size,
		Seq:        seq,
		Modified:   time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Minute),
		Generation: "gen",
		Disk:       "d0",
	}
}

// TestScopeIsLocal pins the capability a caller reads its listing algorithm
// from: this index describes one node, so a cluster-wide answer is a merge.
func TestScopeIsLocal(t *testing.T) {
	assert.Equal(t, metastore.ScopeLocal, open(t).Scope())
}

// TestScanHonoursCancellation is the difference between a context that is
// accepted and one that is honored. A scan with no limit walks the bucket, so a
// caller that gave up — a listing whose client disconnected, a coverage pass on
// a node shutting down — must not leave the node reading to the end of a disk
// on nobody's behalf.
func TestScanHonoursCancellation(t *testing.T) {
	idx := open(t)

	for _, key := range []string{"a", "b", "c", "d"} {
		require.NoError(t, idx.Put(t.Context(), entry("photos", key, 1, 1)))
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	seen := 0

	err := idx.Scan(ctx, "photos", "", "", 0, func(metastore.Entry) error {
		seen++

		cancel()

		return nil
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, seen, "the scan stopped where the caller did, not at the end of the bucket")
}

// TestCoverageHonoursCancellation covers the walk that has no limit at all:
// coverage scans every entry in every bucket, so it is the one an operator is
// most likely to be waiting on when a node is asked to stop.
func TestCoverageHonoursCancellation(t *testing.T) {
	idx := open(t)

	require.NoError(t, idx.Put(t.Context(), entry("photos", "a.jpg", 1, 1)))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := idx.Coverage(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// TestCancelledContextDoesNotWrite: a point operation checks before it touches
// the database, so a caller that has given up cannot still move a bucket's
// counters.
func TestCancelledContextDoesNotWrite(t *testing.T) {
	idx := open(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, idx.Put(ctx, entry("photos", "a.jpg", 100, 1)), context.Canceled)

	usage, err := idx.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Zero(t, usage.Objects)
}

func TestPutAndGet(t *testing.T) {
	idx := open(t)

	require.NoError(t, idx.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))

	got, found, err := idx.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(100), got.Size)
	assert.EqualValues(t, "d0", got.Disk)

	_, found, err = idx.Get(t.Context(), "photos", "missing.jpg")
	require.NoError(t, err)
	assert.False(t, found)

	usage, err := idx.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 1, Bytes: 100}, usage)
}

// TestUsageMovesWithTheEntry is the point of writing both in one batch: the
// counters cannot be left disagreeing with the entries that produced them, so
// a bucket's totals need no recount to be exact on this node.
func TestUsageMovesWithTheEntry(t *testing.T) {
	idx := open(t)

	require.NoError(t, idx.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))
	require.NoError(t, idx.Put(t.Context(), entry("photos", "b.jpg", 250, 1)))

	usage, err := idx.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 2, Bytes: 350}, usage)

	// An overwrite replaces an object rather than adding one: only the size
	// difference moves.
	require.NoError(t, idx.Put(t.Context(), entry("photos", "a.jpg", 40, 2)))

	usage, err = idx.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 2, Bytes: 290}, usage)

	require.NoError(t, idx.Delete(t.Context(), "photos", "b.jpg"))

	usage, err = idx.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 1, Bytes: 40}, usage)

	// Buckets are counted apart.
	require.NoError(t, idx.Put(t.Context(), entry("logs", "x.log", 7, 1)))

	photos, err := idx.Usage(t.Context(), "photos")
	require.NoError(t, err)
	logs, err := idx.Usage(t.Context(), "logs")
	require.NoError(t, err)

	assert.Equal(t, int64(1), photos.Objects)
	assert.Equal(t, int64(1), logs.Objects)
}

// TestOlderRecordIsIgnored covers the rule that keeps a late arrival from
// undoing a newer write: a rebalance copying a superseded generation, or a
// repair completing after the object was overwritten, must not restore the old
// size in either the entry or the counters.
func TestOlderRecordIsIgnored(t *testing.T) {
	idx := open(t)

	require.NoError(t, idx.Put(t.Context(), entry("photos", "a.jpg", 500, 5)))
	require.NoError(t, idx.Put(t.Context(), entry("photos", "a.jpg", 100, 2)))

	got, _, err := idx.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	assert.Equal(t, int64(500), got.Size, "the newer record stands")

	usage, err := idx.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 1, Bytes: 500}, usage, "and the counters did not move")
}

// TestVerifiedAtSurvivesReindex: only the scrub sets it, and a write knows
// nothing about verification — so re-indexing an object must not erase when it
// was last checked.
func TestVerifiedAtSurvivesReindex(t *testing.T) {
	idx := open(t)

	verified := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)

	first := entry("photos", "a.jpg", 100, 1)
	first.VerifiedAt = verified
	require.NoError(t, idx.Put(t.Context(), first))

	require.NoError(t, idx.Put(t.Context(), entry("photos", "a.jpg", 120, 2)))

	got, _, err := idx.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	assert.True(t, verified.Equal(got.VerifiedAt))
	assert.Equal(t, int64(120), got.Size)
}

func TestDeleteMissingIsNotAnError(t *testing.T) {
	idx := open(t)

	require.NoError(t, idx.Delete(t.Context(), "photos", "never.jpg"))

	usage, err := idx.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Zero(t, usage.Objects)
}

// TestScanOrder covers what listings will rest on: keys in order, bounded by
// prefix, resumable from a cursor, and cut at a limit.
func TestScanOrder(t *testing.T) {
	idx := open(t)

	for _, key := range []string{"a.txt", "docs/one", "docs/two", "z.txt"} {
		require.NoError(t, idx.Put(t.Context(), entry("photos", key, 10, 1)))
	}

	// Another bucket must not leak into the scan.
	require.NoError(t, idx.Put(t.Context(), entry("other", "a.txt", 10, 1)))

	collect := func(prefix, after string, limit int) []string {
		var keys []string

		require.NoError(t, idx.Scan(t.Context(), "photos", prefix, after, limit, func(e metastore.Entry) error {
			keys = append(keys, e.Key)

			return nil
		}))

		return keys
	}

	assert.Equal(t, []string{"a.txt", "docs/one", "docs/two", "z.txt"}, collect("", "", 0))
	assert.Equal(t, []string{"docs/one", "docs/two"}, collect("docs/", "", 0))
	assert.Equal(t, []string{"docs/two", "z.txt"}, collect("", "docs/one", 0))
	assert.Equal(t, []string{"a.txt", "docs/one"}, collect("", "", 2))
	assert.Empty(t, collect("", "z.txt", 0), "the cursor is exclusive")

	assert.Equal(t, []string{"other", "photos"}, buckets(t, idx))
}

// TestScanStopsOnCallbackError lets a reader stop early without draining the
// bucket, which is what paging a listing needs.
// TestBucketsStopsOnCallbackError: the reason Buckets streams is that nothing
// bounds how many buckets an account holds, which is only worth anything if a
// caller can stop early.
func TestBucketsStopsOnCallbackError(t *testing.T) {
	idx := open(t)

	for _, bucket := range []string{"a", "b", "c"} {
		require.NoError(t, idx.Put(t.Context(), entry(bucket, "k", 1, 1)))
	}

	stop := assert.AnError
	seen := 0

	err := idx.Buckets(t.Context(), func(string) error {
		seen++

		return stop
	})

	require.ErrorIs(t, err, stop)
	assert.Equal(t, 1, seen)
}

// TestBucketsHonoursCancellation: same contract as Scan, and it matters for the
// same reason — a caller that gave up must not leave the node walking.
func TestBucketsHonoursCancellation(t *testing.T) {
	idx := open(t)

	require.NoError(t, idx.Put(t.Context(), entry("photos", "a.jpg", 1, 1)))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, idx.Buckets(ctx, func(string) error { return nil }), context.Canceled)
}

// TestBucketNameOutlivesTheIterator: pebble's iter.Key() is only valid until the
// next Next, so a name handed to fn has to be a copy. A caller that retains one
// — which collecting into a slice is — would otherwise read whatever the
// iterator's buffer holds by the time it looks.
func TestBucketNameOutlivesTheIterator(t *testing.T) {
	idx := open(t)

	for _, bucket := range []string{"aaaaaaaaaaaa", "b", "cccccccc"} {
		require.NoError(t, idx.Put(t.Context(), entry(bucket, "k", 1, 1)))
	}

	assert.Equal(t, []string{"aaaaaaaaaaaa", "b", "cccccccc"}, buckets(t, idx))
}

func TestScanStopsOnCallbackError(t *testing.T) {
	idx := open(t)

	for _, key := range []string{"a", "b", "c"} {
		require.NoError(t, idx.Put(t.Context(), entry("photos", key, 1, 1)))
	}

	stop := assert.AnError
	seen := 0

	err := idx.Scan(t.Context(), "photos", "", "", 0, func(metastore.Entry) error {
		seen++

		return stop
	})

	require.ErrorIs(t, err, stop)
	assert.Equal(t, 1, seen)
}

// TestStateRoundTrip covers the contract that makes unsynced writes safe: an
// index is usable only after a build, and only a clean handover carries that
// across a restart.
func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()

	first, err := objindex.Open(dir)
	require.NoError(t, err)

	state, err := first.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state, "a fresh index has nothing in it")

	require.NoError(t, first.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))
	require.NoError(t, first.MarkReady(t.Context()))
	require.NoError(t, first.Close())

	second, err := objindex.Open(dir)
	require.NoError(t, err)

	state, err = second.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state,
		"opening invalidates: only the next clean close may call it ready again")

	got, found, err := second.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	require.True(t, found, "the entries are still there, they are just not trusted yet")
	assert.Equal(t, int64(100), got.Size)

	require.NoError(t, second.MarkReady(t.Context()))
	require.NoError(t, second.Close())

	third, err := objindex.Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = third.Close() })

	// Close marked it ready, Open marked it building again — the point is that
	// a process which never closes leaves it building, which is what schedules
	// the rebuild.
	state, err = third.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state)
}

// TestResetEmpties covers why a rebuild starts from nothing: an object deleted
// while the index was not watching leaves an entry that adding alone would
// never remove, and an index listing what is gone is worse than one behind.
func TestResetEmpties(t *testing.T) {
	idx := open(t)

	require.NoError(t, idx.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))
	require.NoError(t, idx.MarkReady(t.Context()))

	require.NoError(t, idx.Reset(t.Context()))

	_, found, err := idx.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	assert.False(t, found)

	usage, err := idx.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Zero(t, usage.Objects)
	assert.Zero(t, usage.Bytes)

	assert.Empty(t, buckets(t, idx))

	state, err := idx.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state, "a reset index is not usable until rebuilt")
}

// TestKeysWithSeparators checks the NUL-delimited key encoding holds up for
// keys that contain the delimiter S3 clients use, and for bucket names that
// prefix one another.
func TestKeysWithSeparators(t *testing.T) {
	idx := open(t)

	require.NoError(t, idx.Put(t.Context(), entry("photos", "a/b/c.jpg", 1, 1)))
	require.NoError(t, idx.Put(t.Context(), entry("photos", "a/b", 2, 1)))
	// "photos-archive" sorts adjacent to "photos" and must not bleed into it.
	require.NoError(t, idx.Put(t.Context(), entry("photos-archive", "a/b/c.jpg", 3, 1)))

	var keys []string

	require.NoError(t, idx.Scan(t.Context(), "photos", "", "", 0, func(e metastore.Entry) error {
		keys = append(keys, e.Key)

		return nil
	}))

	assert.Equal(t, []string{"a/b", "a/b/c.jpg"}, keys)
}

func TestSyncWritesOption(t *testing.T) {
	idx, err := objindex.Open(t.TempDir(), objindex.WithSyncWrites(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	require.NoError(t, idx.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))

	got, found, err := idx.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(100), got.Size)
}

// TestSetVerifiedKeepsTheEntry checks the two writers stay out of each other's
// way: only the scrub knows when an object was checked, only the write path
// knows its size, and neither may erase the other's field.
func TestSetVerifiedKeepsTheEntry(t *testing.T) {
	idx := open(t)

	require.NoError(t, idx.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))

	at := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	require.NoError(t, idx.SetVerified(t.Context(), []metastore.Verification{{Bucket: "photos", Key: "a.jpg", At: at}}))

	got, found, err := idx.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, at.Equal(got.VerifiedAt))
	assert.Equal(t, int64(100), got.Size, "the write path's fields survive a verification")

	// A later write keeps the stamp.
	require.NoError(t, idx.Put(t.Context(), entry("photos", "a.jpg", 55, 2)))

	got, _, err = idx.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	assert.True(t, at.Equal(got.VerifiedAt), "and the verification survives a write")
	assert.Equal(t, int64(55), got.Size)
}

// TestSetVerifiedSkipsUnknownObjects: a stamp for something the node does not
// hold would be an entry with no object behind it, which would then be listed.
func TestSetVerifiedSkipsUnknownObjects(t *testing.T) {
	idx := open(t)

	require.NoError(t, idx.SetVerified(t.Context(), []metastore.Verification{
		{Bucket: "photos", Key: "ghost.jpg", At: time.Now()},
	}))

	_, found, err := idx.Get(t.Context(), "photos", "ghost.jpg")
	require.NoError(t, err)
	assert.False(t, found)

	usage, err := idx.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Zero(t, usage.Objects)
}

// TestCoverage is the number counters of scrub work cannot give: how stale the
// node's verification is.
func TestCoverage(t *testing.T) {
	idx := open(t)

	for _, key := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		require.NoError(t, idx.Put(t.Context(), entry("photos", key, 10, 1)))
	}

	cov, err := idx.Coverage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(3), cov.Objects)
	assert.Equal(t, int64(3), cov.Never, "nothing has been checked yet")
	assert.True(t, cov.Oldest.IsZero())

	older := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)

	require.NoError(t, idx.SetVerified(t.Context(), []metastore.Verification{
		{Bucket: "photos", Key: "a.jpg", At: newer},
		{Bucket: "photos", Key: "b.jpg", At: older},
	}))

	cov, err = idx.Coverage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), cov.Never, "one object still unchecked")
	assert.True(t, older.Equal(cov.Oldest), "coverage is only as good as the least recent check")

	require.NoError(t, idx.SetVerified(t.Context(), []metastore.Verification{
		{Bucket: "photos", Key: "c.jpg", At: newer},
	}))

	cov, err = idx.Coverage(t.Context())
	require.NoError(t, err)
	assert.Zero(t, cov.Never)
	assert.True(t, older.Equal(cov.Oldest))
}
