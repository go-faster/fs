// Package metastoretest provides a conformance test suite for
// metastore.Store implementations.
//
// Backend packages call Run from a regular test to verify they satisfy the
// contract every caller is written against:
//
//	func TestConformance(t *testing.T) {
//		metastoretest.Run(t, func(t testing.TB) metastore.Store {
//			idx, err := objindex.Open(t.TempDir())
//			require.NoError(t, err)
//			t.Cleanup(func() { _ = idx.Close() })
//
//			return idx
//		})
//	}
//
// It exists because the store is about to stop having one implementation. The
// ordering rule, the cancellation contract and the counter arithmetic are not
// pebble's behavior that other backends should imitate — they are the
// contract, and a backend that disagrees with any of them produces a listing
// that disagrees with the disks while looking internally consistent.
//
// What belongs here is anything a caller may rely on. What does not is
// construction, durability knobs, and how a backend recovers its own state
// across a restart: those are legitimately per-implementation, and objindex
// keeps its own tests for them.
package metastoretest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/metastore"
)

// Names used across the suite, so a backend reading a failure can tell which
// case it came from.
const (
	testBucket = "photos"
	// otherBucket exists only to prove a scan of one bucket cannot see another.
	otherBucket = "other"
	// testKey is the key the single-object cases operate on.
	testKey = "a.jpg"
	// firstKey sorts first in the listing fixtures, and nestedKey sorts into
	// the middle of them under a common prefix — the shape a delimiter listing
	// and a resume cursor both turn on.
	firstKey   = "a.txt"
	nestedKey  = "docs/one"
	nestedKey2 = "docs/two"
	lastKey    = "z.txt"
)

// Factory returns a fresh, empty metastore.Store for a single (sub)test.
// Cleanup should be registered on t.
type Factory func(t testing.TB) metastore.Store

// Run executes the conformance suite against stores produced by factory. Every
// subtest receives its own store.
func Run(t *testing.T, factory Factory) {
	t.Helper()

	for name, test := range suite {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test(t, factory(t))
		})
	}
}

var suite = map[string]func(t *testing.T, store metastore.Store){
	"Scope/IsDeclared":              testScopeIsDeclared,
	"Put/AndGet":                    testPutAndGet,
	"Put/OlderRecordIgnored":        testOlderRecordIsIgnored,
	"Put/VerifiedAtSurvivesReindex": testVerifiedAtSurvivesReindex,
	"Delete/MissingIsNotAnError":    testDeleteMissingIsNotAnError,
	"Usage/MovesWithTheEntry":       testUsageMovesWithTheEntry,
	"Scan/Order":                    testScanOrder,
	"Scan/StopsOnCallbackError":     testScanStopsOnCallbackError,
	"Scan/HonoursCancellation":      testScanHonoursCancellation,
	"Scan/KeysWithSeparators":       testKeysWithSeparators,
	"Buckets/Order":                 testBucketsOrder,
	"Buckets/StopsOnCallbackError":  testBucketsStopsOnCallbackError,
	"Buckets/HonoursCancellation":   testBucketsHonoursCancellation,
	"Buckets/NameOutlivesIterator":  testBucketNameOutlivesIterator,
	"Verified/KeepsTheEntry":        testSetVerifiedKeepsTheEntry,
	"Verified/SkipsUnknownObjects":  testSetVerifiedSkipsUnknownObjects,
	"Coverage/TracksVerification":   testCoverage,
	"Coverage/HonoursCancellation":  testCoverageHonoursCancellation,
	"State/MarkReadyAndBuilding":    testStateMarks,
	"Reset/Empties":                 testResetEmpties,
	"Context/CancelledDoesNotWrite": testCancelledContextDoesNotWrite,
}

// Entry builds a record; seq orders it against others for the same key. It is
// exported so a backend's own tests can share the fixtures.
func Entry(bucket, key string, size, seq int64) metastore.Entry {
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

// Buckets drains a store's bucket stream into a slice.
func Buckets(t *testing.T, store metastore.Store) []string {
	t.Helper()

	var out []string

	require.NoError(t, store.Buckets(t.Context(), func(bucket string) error {
		out = append(out, bucket)

		return nil
	}))

	return out
}

// keys drains a bucket's scan into the key order it produced.
func keys(t *testing.T, store metastore.Store, prefix, after string, limit int) []string {
	t.Helper()

	var out []string

	require.NoError(t, store.Scan(t.Context(), testBucket, prefix, after, limit,
		func(e metastore.Entry) error {
			out = append(out, e.Key)

			return nil
		}))

	return out
}

// testScopeIsDeclared: a store answers with one of the two scopes and does not
// invent a third. Callers pick their listing algorithm from this, so an
// unrecognized value would silently take the wrong branch.
func testScopeIsDeclared(t *testing.T, store metastore.Store) {
	assert.Contains(t,
		[]metastore.Scope{metastore.ScopeLocal, metastore.ScopeCluster},
		store.Scope())
}

func testPutAndGet(t *testing.T, store metastore.Store) {
	require.NoError(t, store.Put(t.Context(), Entry(testBucket, testKey, 100, 1)))

	got, found, err := store.Get(t.Context(), testBucket, testKey)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(100), got.Size)
	assert.EqualValues(t, "d0", got.Disk)

	_, found, err = store.Get(t.Context(), testBucket, "missing.jpg")
	require.NoError(t, err)
	assert.False(t, found)

	usage, err := store.Usage(t.Context(), testBucket)
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 1, Bytes: 100}, usage)
}

// testUsageMovesWithTheEntry is the point of writing both together: the
// counters cannot be left disagreeing with the entries that produced them, so a
// bucket's totals need no recount to be exact.
func testUsageMovesWithTheEntry(t *testing.T, store metastore.Store) {
	require.NoError(t, store.Put(t.Context(), Entry(testBucket, testKey, 100, 1)))
	require.NoError(t, store.Put(t.Context(), Entry(testBucket, "b.jpg", 250, 1)))

	usage, err := store.Usage(t.Context(), testBucket)
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 2, Bytes: 350}, usage)

	// An overwrite replaces an object rather than adding one: only the size
	// difference moves.
	require.NoError(t, store.Put(t.Context(), Entry(testBucket, testKey, 40, 2)))

	usage, err = store.Usage(t.Context(), testBucket)
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 2, Bytes: 290}, usage)

	require.NoError(t, store.Delete(t.Context(), testBucket, "b.jpg"))

	usage, err = store.Usage(t.Context(), testBucket)
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 1, Bytes: 40}, usage)

	// Buckets are counted apart.
	require.NoError(t, store.Put(t.Context(), Entry("logs", "x.log", 7, 1)))

	photos, err := store.Usage(t.Context(), testBucket)
	require.NoError(t, err)
	logs, err := store.Usage(t.Context(), "logs")
	require.NoError(t, err)

	assert.Equal(t, int64(1), photos.Objects)
	assert.Equal(t, int64(1), logs.Objects)
}

// testOlderRecordIsIgnored covers the rule that keeps a late arrival from
// undoing a newer write: a rebalance copying a superseded generation, or a
// repair completing after the object was overwritten, must not restore the old
// size in either the entry or the counters.
func testOlderRecordIsIgnored(t *testing.T, store metastore.Store) {
	require.NoError(t, store.Put(t.Context(), Entry(testBucket, testKey, 500, 5)))
	require.NoError(t, store.Put(t.Context(), Entry(testBucket, testKey, 100, 2)))

	got, _, err := store.Get(t.Context(), testBucket, testKey)
	require.NoError(t, err)
	assert.Equal(t, int64(500), got.Size, "the newer record stands")

	usage, err := store.Usage(t.Context(), testBucket)
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 1, Bytes: 500}, usage, "and the counters did not move")
}

// testVerifiedAtSurvivesReindex: only the scrub sets it, and a write knows
// nothing about verification — so re-indexing an object must not erase when it
// was last checked.
func testVerifiedAtSurvivesReindex(t *testing.T, store metastore.Store) {
	verified := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)

	first := Entry(testBucket, testKey, 100, 1)
	first.VerifiedAt = verified
	require.NoError(t, store.Put(t.Context(), first))

	require.NoError(t, store.Put(t.Context(), Entry(testBucket, testKey, 120, 2)))

	got, _, err := store.Get(t.Context(), testBucket, testKey)
	require.NoError(t, err)
	assert.True(t, verified.Equal(got.VerifiedAt))
	assert.Equal(t, int64(120), got.Size)
}

func testDeleteMissingIsNotAnError(t *testing.T, store metastore.Store) {
	require.NoError(t, store.Delete(t.Context(), testBucket, "never.jpg"))

	usage, err := store.Usage(t.Context(), testBucket)
	require.NoError(t, err)
	assert.Zero(t, usage.Objects)
}

// testScanOrder covers what listings rest on: keys in order, bounded by prefix,
// resumable from a cursor, and cut at a limit.
func testScanOrder(t *testing.T, store metastore.Store) {
	for _, key := range []string{firstKey, nestedKey, nestedKey2, lastKey} {
		require.NoError(t, store.Put(t.Context(), Entry(testBucket, key, 10, 1)))
	}

	// Another bucket must not leak into the scan.
	require.NoError(t, store.Put(t.Context(), Entry(otherBucket, firstKey, 10, 1)))

	assert.Equal(t, []string{firstKey, nestedKey, nestedKey2, lastKey},
		keys(t, store, "", "", 0))
	assert.Equal(t, []string{nestedKey, nestedKey2}, keys(t, store, "docs/", "", 0))
	assert.Equal(t, []string{nestedKey2, lastKey}, keys(t, store, "", nestedKey, 0))
	assert.Equal(t, []string{firstKey, nestedKey}, keys(t, store, "", "", 2))
	assert.Empty(t, keys(t, store, "", lastKey, 0), "the cursor is exclusive")

	assert.Equal(t, []string{otherBucket, testBucket}, Buckets(t, store))
}

// testScanStopsOnCallbackError lets a reader stop early without draining the
// bucket, which is what paging a listing needs.
func testScanStopsOnCallbackError(t *testing.T, store metastore.Store) {
	for _, key := range []string{"a", "b", "c"} {
		require.NoError(t, store.Put(t.Context(), Entry(testBucket, key, 1, 1)))
	}

	stop := assert.AnError
	seen := 0

	err := store.Scan(t.Context(), testBucket, "", "", 0, func(metastore.Entry) error {
		seen++

		return stop
	})

	require.ErrorIs(t, err, stop)
	assert.Equal(t, 1, seen)
}

// testScanHonoursCancellation is the difference between a context that is
// accepted and one that is honored. A scan with no limit walks the bucket, so a
// caller that gave up — a listing whose client disconnected, a coverage pass on
// a node shutting down — must not leave the store reading on nobody's behalf.
func testScanHonoursCancellation(t *testing.T, store metastore.Store) {
	for _, key := range []string{"a", "b", "c", "d"} {
		require.NoError(t, store.Put(t.Context(), Entry(testBucket, key, 1, 1)))
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	seen := 0

	err := store.Scan(ctx, testBucket, "", "", 0, func(metastore.Entry) error {
		seen++

		cancel()

		return nil
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, seen, "the scan stopped where the caller did, not at the end of the bucket")
}

// testKeysWithSeparators checks the key encoding holds up for keys containing
// the delimiter S3 clients use, and for bucket names that prefix one another.
func testKeysWithSeparators(t *testing.T, store metastore.Store) {
	require.NoError(t, store.Put(t.Context(), Entry(testBucket, "a/b/c.jpg", 1, 1)))
	require.NoError(t, store.Put(t.Context(), Entry(testBucket, "a/b", 2, 1)))
	// "photos-archive" sorts adjacent to testBucket and must not bleed into it.
	require.NoError(t, store.Put(t.Context(), Entry("photos-archive", "a/b/c.jpg", 3, 1)))

	assert.Equal(t, []string{"a/b", "a/b/c.jpg"}, keys(t, store, "", "", 0))
}

func testBucketsOrder(t *testing.T, store metastore.Store) {
	for _, bucket := range []string{"zebra", "apple", "mango"} {
		require.NoError(t, store.Put(t.Context(), Entry(bucket, "k", 1, 1)))
	}

	assert.Equal(t, []string{"apple", "mango", "zebra"}, Buckets(t, store))
}

// testBucketsStopsOnCallbackError: the reason Buckets streams is that nothing
// bounds how many buckets an account holds, which is only worth anything if a
// caller can stop early.
func testBucketsStopsOnCallbackError(t *testing.T, store metastore.Store) {
	for _, bucket := range []string{"a", "b", "c"} {
		require.NoError(t, store.Put(t.Context(), Entry(bucket, "k", 1, 1)))
	}

	stop := assert.AnError
	seen := 0

	err := store.Buckets(t.Context(), func(string) error {
		seen++

		return stop
	})

	require.ErrorIs(t, err, stop)
	assert.Equal(t, 1, seen)
}

func testBucketsHonoursCancellation(t *testing.T, store metastore.Store) {
	require.NoError(t, store.Put(t.Context(), Entry(testBucket, testKey, 1, 1)))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, store.Buckets(ctx, func(string) error { return nil }), context.Canceled)
}

// testBucketNameOutlivesIterator: a name handed to fn has to outlive the
// iteration that produced it. A backend streaming out of a reused buffer —
// which pebble's iter.Key() is — would hand back whatever the buffer holds by
// the time a caller that collected them looks. Names of differing lengths are
// what make that visible instead of accidentally correct.
func testBucketNameOutlivesIterator(t *testing.T, store metastore.Store) {
	for _, bucket := range []string{"aaaaaaaaaaaa", "b", "cccccccc"} {
		require.NoError(t, store.Put(t.Context(), Entry(bucket, "k", 1, 1)))
	}

	assert.Equal(t, []string{"aaaaaaaaaaaa", "b", "cccccccc"}, Buckets(t, store))
}

// testSetVerifiedKeepsTheEntry checks the two writers stay out of each other's
// way: only the scrub knows when an object was checked, only the write path
// knows its size, and neither may erase the other's field.
func testSetVerifiedKeepsTheEntry(t *testing.T, store metastore.Store) {
	require.NoError(t, store.Put(t.Context(), Entry(testBucket, testKey, 100, 1)))

	at := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	require.NoError(t, store.SetVerified(t.Context(),
		[]metastore.Verification{{Bucket: testBucket, Key: testKey, At: at}}))

	got, found, err := store.Get(t.Context(), testBucket, testKey)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, at.Equal(got.VerifiedAt))
	assert.Equal(t, int64(100), got.Size, "the write path's fields survive a verification")

	// A later write keeps the stamp.
	require.NoError(t, store.Put(t.Context(), Entry(testBucket, testKey, 55, 2)))

	got, _, err = store.Get(t.Context(), testBucket, testKey)
	require.NoError(t, err)
	assert.True(t, at.Equal(got.VerifiedAt), "and the verification survives a write")
	assert.Equal(t, int64(55), got.Size)
}

// testSetVerifiedSkipsUnknownObjects: a stamp for something the store does not
// hold would be an entry with no object behind it, which would then be listed.
func testSetVerifiedSkipsUnknownObjects(t *testing.T, store metastore.Store) {
	require.NoError(t, store.SetVerified(t.Context(), []metastore.Verification{
		{Bucket: testBucket, Key: "ghost.jpg", At: time.Now()},
	}))

	_, found, err := store.Get(t.Context(), testBucket, "ghost.jpg")
	require.NoError(t, err)
	assert.False(t, found)

	usage, err := store.Usage(t.Context(), testBucket)
	require.NoError(t, err)
	assert.Zero(t, usage.Objects)
}

// testCoverage is the number counters of scrub work cannot give: how stale
// verification is.
func testCoverage(t *testing.T, store metastore.Store) {
	for _, key := range []string{testKey, "b.jpg", "c.jpg"} {
		require.NoError(t, store.Put(t.Context(), Entry(testBucket, key, 10, 1)))
	}

	cov, err := store.Coverage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(3), cov.Objects)
	assert.Equal(t, int64(3), cov.Never, "nothing has been checked yet")
	assert.True(t, cov.Oldest.IsZero())

	older := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)

	require.NoError(t, store.SetVerified(t.Context(), []metastore.Verification{
		{Bucket: testBucket, Key: testKey, At: newer},
		{Bucket: testBucket, Key: "b.jpg", At: older},
	}))

	cov, err = store.Coverage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), cov.Never, "one object still unchecked")
	assert.True(t, older.Equal(cov.Oldest), "coverage is only as good as the least recent check")

	require.NoError(t, store.SetVerified(t.Context(), []metastore.Verification{
		{Bucket: testBucket, Key: "c.jpg", At: newer},
	}))

	cov, err = store.Coverage(t.Context())
	require.NoError(t, err)
	assert.Zero(t, cov.Never)
	assert.True(t, older.Equal(cov.Oldest))
}

// testCoverageHonoursCancellation covers the walk that has no limit at all:
// coverage visits every entry in every bucket, so it is the one an operator is
// most likely to be waiting on when a node is asked to stop.
func testCoverageHonoursCancellation(t *testing.T, store metastore.Store) {
	require.NoError(t, store.Put(t.Context(), Entry(testBucket, testKey, 1, 1)))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := store.Coverage(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// testStateMarks: a store is trusted only after something says so, and can be
// untrusted again when a rebuild starts.
func testStateMarks(t *testing.T, store metastore.Store) {
	state, err := store.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state, "a fresh store holds nothing and says so")

	require.NoError(t, store.MarkReady(t.Context()))

	state, err = store.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateReady, state)

	require.NoError(t, store.MarkBuilding(t.Context()))

	state, err = store.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state)
}

// testResetEmpties covers why a rebuild starts from nothing: an object deleted
// while the store was not watching leaves an entry that adding alone would
// never remove, and a store listing what is gone is worse than one behind.
func testResetEmpties(t *testing.T, store metastore.Store) {
	require.NoError(t, store.Put(t.Context(), Entry(testBucket, testKey, 100, 1)))
	require.NoError(t, store.MarkReady(t.Context()))

	require.NoError(t, store.Reset(t.Context()))

	_, found, err := store.Get(t.Context(), testBucket, testKey)
	require.NoError(t, err)
	assert.False(t, found)

	usage, err := store.Usage(t.Context(), testBucket)
	require.NoError(t, err)
	assert.Zero(t, usage.Objects)
	assert.Zero(t, usage.Bytes)

	assert.Empty(t, Buckets(t, store))

	state, err := store.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state, "a reset store is not usable until rebuilt")
}

// testCancelledContextDoesNotWrite: a point operation checks before it touches
// storage, so a caller that has given up cannot still move a bucket's counters.
func testCancelledContextDoesNotWrite(t *testing.T, store metastore.Store) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, store.Put(ctx, Entry(testBucket, testKey, 100, 1)), context.Canceled)

	usage, err := store.Usage(t.Context(), testBucket)
	require.NoError(t, err)
	assert.Zero(t, usage.Objects)
}
