package shardstore_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/metastore/metastoretest"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
)

// openShard returns a shard serving the ranges given, closed at the end of the
// test. With no ranges it serves nothing, which is the state a shard starts in.
func openShard(t *testing.T, ranges ...rangemap.Range) *shardstore.Shard {
	t.Helper()

	s, err := shardstore.OpenShard(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	s.Adopt(ranges)

	return s
}

// whole is the range covering everything, for tests that are not about
// partitioning.
var whole = rangemap.Range{Start: "", End: "", Owner: "n0"}

func entry(bucket, key string, size, seq int64) metastore.Entry {
	return metastoretest.Entry(bucket, key, size, seq)
}

// TestShardServesNothingUntilAdopted: a shard that guessed would answer for
// keys the map has given to someone else, and two nodes answering for one key
// is how a listing returns a superseded record.
func TestShardServesNothingUntilAdopted(t *testing.T) {
	s := openShard(t)

	require.ErrorIs(t, s.Put(t.Context(), entry("photos", "a.jpg", 100, 1)), shardstore.ErrNotOwned)

	_, _, err := s.Get(t.Context(), "photos", "a.jpg")
	require.ErrorIs(t, err, shardstore.ErrNotOwned)

	require.ErrorIs(t, s.Delete(t.Context(), "photos", "a.jpg"), shardstore.ErrNotOwned)
}

// TestShardRefusesKeysOutsideItsRanges is the ownership boundary. A shard that
// served a key outside its ranges would be answering for data the real owner
// also holds, and the two would diverge from the moment either took a write.
func TestShardRefusesKeysOutsideItsRanges(t *testing.T) {
	// Serves buckets sorting before "m" only.
	s := openShard(t, rangemap.Range{Start: "", End: "om", Owner: "n0"})

	require.NoError(t, s.Put(t.Context(), entry("apples", "a.jpg", 10, 1)))

	err := s.Put(t.Context(), entry("zebras", "a.jpg", 10, 1))
	require.ErrorIs(t, err, shardstore.ErrNotOwned)

	_, found, err := s.Get(t.Context(), "apples", "a.jpg")
	require.NoError(t, err)
	assert.True(t, found)
}

// TestShardCountersAreItsOwn is the resolution of how a bucket's counters
// survive being spread across shards: each shard counts only what it holds, in
// the same batch as the entry, and the total is the sum.
//
// The alternative — one home for a bucket's counters — makes every write a
// cross-shard write, with no atomic batch and counters that drift from the rows
// behind them.
func TestShardCountersAreItsOwn(t *testing.T) {
	low := openShard(t, rangemap.Range{Start: "", End: "om", Owner: "n0"})
	high := openShard(t, rangemap.Range{Start: "om", End: "", Owner: "n1"})

	// One bucket, one key on each side of the boundary. Both shards hold part
	// of "photos"... except "photos" sorts under 'p', so use two buckets that
	// straddle instead — the point is the same: a bucket's counters are local.
	require.NoError(t, low.Put(t.Context(), entry("apples", "a", 100, 1)))
	require.NoError(t, high.Put(t.Context(), entry("zebras", "a", 250, 1)))

	got, err := low.Usage(t.Context(), "apples")
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 1, Bytes: 100}, got)

	// The other shard knows nothing about a bucket it holds none of, which is
	// what makes the sum correct rather than double-counted.
	got, err = low.Usage(t.Context(), "zebras")
	require.NoError(t, err)
	assert.Zero(t, got.Objects)

	got, err = high.Usage(t.Context(), "zebras")
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 1, Bytes: 250}, got)
}

// TestShardScanIsBoundedToItsRanges: a shard returns exactly its share of a
// bucket, in key order, so the layer above can concatenate shards' answers
// without sorting. Returning a key it does not serve would duplicate one the
// real owner also returns.
func TestShardScanIsBoundedToItsRanges(t *testing.T) {
	// Two disjoint ranges on one shard, with a gap it does not serve.
	s := openShard(t,
		rangemap.Range{Start: "", End: "ob", Owner: "n0"},
		rangemap.Range{Start: "oc", End: "od", Owner: "n0"},
	)

	// "a…" is in the first range, "cats" in the second, "bees" in the gap.
	require.NoError(t, s.Put(t.Context(), entry("apples", "k", 1, 1)))
	require.NoError(t, s.Put(t.Context(), entry("cats", "k", 1, 1)))
	require.ErrorIs(t, s.Put(t.Context(), entry("bees", "k", 1, 1)), shardstore.ErrNotOwned)

	assert.Equal(t, []string{"k"}, scanKeys(t, s, "apples"))
	assert.Equal(t, []string{"k"}, scanKeys(t, s, "cats"))
	assert.Empty(t, scanKeys(t, s, "bees"), "a bucket in the gap yields nothing here")
}

// TestShardScanKeepsKeyOrderAcrossRanges: the served ranges are disjoint and
// sorted, so walking them in turn yields key order — which is what lets the
// layer above concatenate rather than merge.
func TestShardScanKeepsKeyOrderAcrossRanges(t *testing.T) {
	s := openShard(t, whole)

	for _, key := range []string{"c", "a", "b", "e", "d"} {
		require.NoError(t, s.Put(t.Context(), entry("photos", key, 1, 1)))
	}

	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, scanKeys(t, s, "photos"))
}

// TestShardScanRespectsPrefixCursorAndLimit — the shapes a listing page needs,
// on a shard that serves the whole space.
func TestShardScanRespectsPrefixCursorAndLimit(t *testing.T) {
	s := openShard(t, whole)

	for _, key := range []string{"a.txt", "docs/one", "docs/two", "z.txt"} {
		require.NoError(t, s.Put(t.Context(), entry("photos", key, 1, 1)))
	}

	assert.Equal(t, []string{"docs/one", "docs/two"}, scanWith(t, s, "docs/", "", 0))
	assert.Equal(t, []string{"docs/two", "z.txt"}, scanWith(t, s, "", "docs/one", 0))
	assert.Equal(t, []string{"a.txt", "docs/one"}, scanWith(t, s, "", "", 2))
	assert.Empty(t, scanWith(t, s, "", "z.txt", 0), "the cursor is exclusive")
}

// TestShardSupersedesRule: a late arrival must not undo a newer write, in
// either the entry or the counters — the same total order the sidecars use.
func TestShardSupersedesRule(t *testing.T) {
	s := openShard(t, whole)

	require.NoError(t, s.Put(t.Context(), entry("photos", "a.jpg", 500, 5)))
	require.NoError(t, s.Put(t.Context(), entry("photos", "a.jpg", 100, 2)))

	got, _, err := s.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	assert.Equal(t, int64(500), got.Size, "the newer record stands")

	usage, err := s.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 1, Bytes: 500}, usage, "and the counters did not move")
}

// TestShardVerificationSurvivesAWrite: only the scrub knows when an object was
// checked and only the write path knows its size; neither may erase the other.
func TestShardVerificationSurvivesAWrite(t *testing.T) {
	s := openShard(t, whole)

	require.NoError(t, s.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))

	at := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	require.NoError(t, s.SetVerified(t.Context(),
		[]metastore.Verification{{Bucket: "photos", Key: "a.jpg", At: at}}))

	require.NoError(t, s.Put(t.Context(), entry("photos", "a.jpg", 55, 2)))

	got, _, err := s.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	assert.True(t, at.Equal(got.VerifiedAt))
	assert.Equal(t, int64(55), got.Size)
}

// TestShardSetVerifiedSkipsWhatItDoesNotServe: a stamp for a key another shard
// owns would be an entry with no object behind it, which would then be listed.
func TestShardSetVerifiedSkipsWhatItDoesNotServe(t *testing.T) {
	s := openShard(t, rangemap.Range{Start: "", End: "om", Owner: "n0"})

	require.NoError(t, s.SetVerified(t.Context(), []metastore.Verification{
		{Bucket: "zebras", Key: "a", At: time.Now()},
	}))

	assert.Empty(t, scanKeys(t, s, "zebras"))
}

// TestShardCoverageCoversWhatItHolds. Partial, like Usage.
func TestShardCoverageCoversWhatItHolds(t *testing.T) {
	s := openShard(t, whole)

	for _, key := range []string{"a", "b"} {
		require.NoError(t, s.Put(t.Context(), entry("photos", key, 1, 1)))
	}

	cov, err := s.Coverage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(2), cov.Objects)
	assert.Equal(t, int64(2), cov.Never)

	at := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	require.NoError(t, s.SetVerified(t.Context(),
		[]metastore.Verification{{Bucket: "photos", Key: "a", At: at}}))

	cov, err = s.Coverage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), cov.Never)
	assert.True(t, at.Equal(cov.Oldest))
}

// TestShardAdoptReplaces: a range this node no longer owns must stop being
// served the moment the map says so. The new owner is already serving it.
func TestShardAdoptReplaces(t *testing.T) {
	s := openShard(t, whole)

	require.NoError(t, s.Put(t.Context(), entry("zebras", "a", 1, 1)))

	s.Adopt([]rangemap.Range{{Start: "", End: "om", Owner: "n0"}})

	_, _, err := s.Get(t.Context(), "zebras", "a")
	require.ErrorIs(t, err, shardstore.ErrNotOwned, "a released range stops being served at once")
}

// TestShardScanBoundsIndependentlyOfWrites is the case the previous test
// cannot reach.
//
// There, keys outside the served ranges were never written, because Put refused
// them — so the scan could have ignored its bounds entirely and still looked
// right. Here the data is written while the shard serves everything and the
// ranges are narrowed afterwards, which is exactly what a map change does. A
// scan that did not bound would now return keys another node is also returning,
// and the listing above would show them twice.
func TestShardScanBoundsIndependentlyOfWrites(t *testing.T) {
	s := openShard(t, whole)

	require.NoError(t, s.Put(t.Context(), entry("apples", "a", 1, 1)))
	require.NoError(t, s.Put(t.Context(), entry("zebras", "a", 1, 1)))

	require.Equal(t, []string{"a"}, scanKeys(t, s, "zebras"), "served while it owned everything")

	s.Adopt([]rangemap.Range{{Start: "", End: "om", Owner: "n0"}})

	assert.Equal(t, []string{"a"}, scanKeys(t, s, "apples"), "what it still serves is unchanged")
	assert.Empty(t, scanKeys(t, s, "zebras"),
		"the released range is not scanned, even though its data is still on disk")
}

// TestDropUnownedReclaimsTheSpace: releasing a range stops it being served
// immediately, but the bytes stay until they are swept — separated because
// doing the sweep inside a map change makes every ownership change as slow as
// the data it moves.
func TestDropUnownedReclaimsTheSpace(t *testing.T) {
	s := openShard(t, whole)

	require.NoError(t, s.Put(t.Context(), entry("apples", "a", 1, 1)))
	require.NoError(t, s.Put(t.Context(), entry("zebras", "a", 1, 1)))

	s.Adopt([]rangemap.Range{{Start: "", End: "om", Owner: "n0"}})
	require.NoError(t, s.DropUnowned(t.Context()))

	// What it still serves is untouched.
	_, found, err := s.Get(t.Context(), "apples", "a")
	require.NoError(t, err)
	assert.True(t, found)

	// And the released range is gone rather than merely hidden: re-adopting it
	// must not resurrect what the new owner now holds.
	s.Adopt([]rangemap.Range{whole})

	_, found, err = s.Get(t.Context(), "zebras", "a")
	require.NoError(t, err)
	assert.False(t, found, "released data is dropped, not hidden")
}

// TestDropUnownedKeepsCountersForWhatItStillServes is the bug this replaced.
//
// A shard's usage rows are keyed 'u' + bucket, which sorts above every object
// key — so a sweep bounded at the end of the usage prefix deleted them all, and
// a shard that narrowed its ranges reported *zero* usage for buckets it was
// still serving. The objects were still there and still listed; only the
// counters were gone, which is the kind of wrong that reads as an accounting
// bug somewhere else entirely.
func TestDropUnownedKeepsCountersForWhatItStillServes(t *testing.T) {
	s := openShard(t, whole)

	require.NoError(t, s.Put(t.Context(), entry("apples", "a", 100, 1)))

	s.Adopt([]rangemap.Range{{Start: "", End: "om", Owner: "n0"}})
	require.NoError(t, s.DropUnowned(t.Context()))

	usage, err := s.Usage(t.Context(), "apples")
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 1, Bytes: 100}, usage,
		"a bucket this shard still serves must still be counted")
}

// TestDropUnownedRecountsWhatItReleased is the other half. Sweeping the entries
// without rebuilding the counters would leave them describing objects that are
// no longer there — too high, and by an amount nothing records.
func TestDropUnownedRecountsWhatItReleased(t *testing.T) {
	s := openShard(t, whole)

	require.NoError(t, s.Put(t.Context(), entry("apples", "a", 100, 1)))
	require.NoError(t, s.Put(t.Context(), entry("zebras", "a", 250, 1)))

	s.Adopt([]rangemap.Range{{Start: "", End: "om", Owner: "n0"}})
	require.NoError(t, s.DropUnowned(t.Context()))

	usage, err := s.Usage(t.Context(), "zebras")
	require.NoError(t, err)
	assert.Zero(t, usage.Objects, "a released bucket is no longer counted here")

	// And it stops being reported as a bucket at all, or the cluster-wide list
	// would name a bucket no shard can produce an object for.
	var buckets []string

	require.NoError(t, s.Buckets(t.Context(), func(b string) error {
		buckets = append(buckets, b)

		return nil
	}))

	assert.Equal(t, []string{"apples"}, buckets)
}

// TestResetEmptiesTheShard: a rebuild that only added would keep entries for
// objects deleted while nothing was watching.
func TestResetEmptiesTheShard(t *testing.T) {
	s := openShard(t, whole)

	require.NoError(t, s.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))
	require.NoError(t, s.Reset(t.Context()))

	assert.Empty(t, scanKeys(t, s, "photos"))

	usage, err := s.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Zero(t, usage.Objects)
}

func scanKeys(t *testing.T, s *shardstore.Shard, bucket string) []string {
	t.Helper()

	var out []string

	require.NoError(t, s.Scan(t.Context(), bucket, "", "", 0, func(e metastore.Entry) error {
		out = append(out, e.Key)

		return nil
	}))

	return out
}

func scanWith(t *testing.T, s *shardstore.Shard, prefix, after string, limit int) []string {
	t.Helper()

	var out []string

	require.NoError(t, s.Scan(t.Context(), "photos", prefix, after, limit, func(e metastore.Entry) error {
		out = append(out, e.Key)

		return nil
	}))

	return out
}
