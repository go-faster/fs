package shardstore_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
)

// learned is a range whose only replica is a learner, which is the state a move
// begins in.
var learned = rangemap.Range{
	Start: "", End: "", Owner: "n0", Learners: []cluster.NodeID{"n1"},
}

// copyRange runs a whole backfill, step by step, and returns how many steps it
// took — so a test can assert the cursor is doing something.
func copyRange(t *testing.T, from, to *shardstore.Shard, r rangemap.Range, limit int) int {
	t.Helper()

	cursor := ""
	steps := 0

	for {
		step, err := from.ReadBackfill(t.Context(), r, cursor, limit)
		require.NoError(t, err)
		require.NoError(t, to.Learn(t.Context(), r, step.Entries))

		steps++

		if step.Done {
			return steps
		}

		require.NotEqual(t, cursor, step.Cursor, "the cursor must advance or this never ends")
		cursor = step.Cursor
	}
}

// TestBackfillCopiesTheRange is the plain case: everything the owner holds
// reaches the learner, in key order, with its counters.
func TestBackfillCopiesTheRange(t *testing.T) {
	owner := openShard(t, learned)

	learner := openShard(t)
	learner.Configure(nil, nil, []rangemap.Range{learned})

	for i := range 20 {
		require.NoError(t, owner.Put(t.Context(),
			entry("photos", fmt.Sprintf("%03d.jpg", i), 100, 1)))
	}

	steps := copyRange(t, owner, learner, learned, 6)
	assert.Greater(t, steps, 1, "twenty entries at six a step is more than one step")

	learner.Adopt([]rangemap.Range{learned})
	assert.Equal(t, scanKeys(t, owner, "photos"), scanKeys(t, learner, "photos"))

	want, err := owner.Usage(t.Context(), "photos")
	require.NoError(t, err)

	got, err := learner.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Equal(t, want, got, "the counters moved with the entries")
}

// TestBackfillDoesNotOverwriteANewerRecord is the race this whole design exists
// to prevent, and the reason a backfill cannot use ApplyBatch.
//
// The backfill reads a key at one version; a client writes it and the log ships
// the newer one; the backfill's copy lands afterwards. A raw replay would write
// the old value over the new — a silent lost update on a replica that looks
// complete and would survive promotion holding a stale value.
func TestBackfillDoesNotOverwriteANewerRecord(t *testing.T) {
	owner := openShard(t, learned)

	learner := openShard(t)
	learner.Configure(nil, nil, []rangemap.Range{learned})

	old := entry("photos", "a.jpg", 100, 1)
	require.NoError(t, owner.Put(t.Context(), old))

	// The backfill reads the old version.
	step, err := owner.ReadBackfill(t.Context(), learned, "", 0)
	require.NoError(t, err)
	require.Len(t, step.Entries, 1)

	// Meanwhile the log delivers a newer one.
	newer := entry("photos", "a.jpg", 250, 2)
	require.NoError(t, learner.Learn(t.Context(), learned, []metastore.Entry{newer}))

	// And only now does the backfill's read land.
	require.NoError(t, learner.Learn(t.Context(), learned, step.Entries))

	learner.Adopt([]rangemap.Range{learned})

	got, found, err := learner.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	require.True(t, found)

	assert.Equal(t, int64(250), got.Size,
		"the stale copy won, which is the lost update a raw replay would cause")

	usage, err := learner.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 1, Bytes: 250}, usage,
		"and the counters followed the entry rather than the arrival order")
}

// TestBackfillIsIdempotent: once ordering against the log stops mattering, a
// cursor can be replayed over keys already copied with no effect.
//
// That is what makes a killed backfill resumable from a persisted position
// rather than from knowing where the log was at the time — which nothing
// records and nothing could.
func TestBackfillIsIdempotent(t *testing.T) {
	owner := openShard(t, learned)

	learner := openShard(t)
	learner.Configure(nil, nil, []rangemap.Range{learned})

	for i := range 10 {
		require.NoError(t, owner.Put(t.Context(),
			entry("photos", fmt.Sprintf("%02d.jpg", i), 100, 1)))
	}

	copyRange(t, owner, learner, learned, 3)

	learner.Adopt([]rangemap.Range{learned})

	before := scanKeys(t, learner, "photos")

	usageBefore, err := learner.Usage(t.Context(), "photos")
	require.NoError(t, err)

	// The whole thing again, as a resume from a lost cursor would do.
	learner.Adopt(nil)
	learner.Configure(nil, nil, []rangemap.Range{learned})
	copyRange(t, owner, learner, learned, 3)

	learner.Adopt([]rangemap.Range{learned})
	assert.Equal(t, before, scanKeys(t, learner, "photos"))

	usageAfter, err := learner.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Equal(t, usageBefore, usageAfter, "a replayed copy double-counted")
}

// TestBackfillResumesFromItsCursor: a killed backfill repeats at most one step,
// which is why a step is bounded rather than a whole range.
func TestBackfillResumesFromItsCursor(t *testing.T) {
	owner := openShard(t, learned)

	learner := openShard(t)
	learner.Configure(nil, nil, []rangemap.Range{learned})

	for i := range 12 {
		require.NoError(t, owner.Put(t.Context(),
			entry("photos", fmt.Sprintf("%02d.jpg", i), 100, 1)))
	}

	first, err := owner.ReadBackfill(t.Context(), learned, "", 5)
	require.NoError(t, err)
	require.Len(t, first.Entries, 5)
	require.False(t, first.Done)
	require.NoError(t, learner.Learn(t.Context(), learned, first.Entries))

	// The runner dies here. Another one picks up the persisted cursor.
	cursor := first.Cursor

	for {
		step, err := owner.ReadBackfill(t.Context(), learned, cursor, 5)
		require.NoError(t, err)
		require.NoError(t, learner.Learn(t.Context(), learned, step.Entries))

		if step.Done {
			break
		}

		cursor = step.Cursor
	}

	learner.Adopt([]rangemap.Range{learned})
	assert.Len(t, scanKeys(t, learner, "photos"), 12)
}

// TestLearnRefusesARangeItDoesNotReplicate: entries for a range this shard was
// never told to hold would leave data nothing asks for and nothing cleans up.
func TestLearnRefusesARangeItDoesNotReplicate(t *testing.T) {
	learner := openShard(t)

	err := learner.Learn(t.Context(), learned, []metastore.Entry{entry("photos", "a.jpg", 1, 1)})
	require.ErrorIs(t, err, shardstore.ErrNotLearned)
}

// TestLearnRefusesAnEntryOutsideTheRange: a backfill copies one range, and an
// entry from outside it is either a bug or a sender working from a different
// map. Storing it would put data on a node nothing routes there.
func TestLearnRefusesAnEntryOutsideTheRange(t *testing.T) {
	bounded := rangemap.Range{Start: "ob", End: "oc", Owner: "n0", Learners: []cluster.NodeID{"n1"}}

	learner := openShard(t)
	learner.Configure(nil, nil, []rangemap.Range{bounded})

	err := learner.Learn(t.Context(), bounded, []metastore.Entry{entry("zulu", "a.jpg", 1, 1)})
	require.ErrorContains(t, err, "outside the range")
}

// TestBackfillReadsOnlyTheOwnedRange: a node backfilling a range it does not
// serve would be shipping someone else's data, from a map it has not caught up
// with.
func TestBackfillReadsOnlyTheOwnedRange(t *testing.T) {
	shard := openShard(t, rangemap.Range{Start: "", End: "ob", Owner: "n0"})

	_, err := shard.ReadBackfill(t.Context(), rangemap.Range{Start: "ob", End: "", Owner: "n1"}, "", 0)
	require.ErrorIs(t, err, shardstore.ErrNotOwned)
}

// TestBackfillSkipsTheCounters: the usage rows sort above every object key, so a
// walk of the last range runs into them. They are not entries, and a learner
// receiving one would be told to store a record that describes nothing.
func TestBackfillSkipsTheCounters(t *testing.T) {
	owner := openShard(t, learned)

	for _, bucket := range []string{"alpha", "zulu"} {
		require.NoError(t, owner.Put(t.Context(), entry(bucket, "a.jpg", 100, 1)))
	}

	// The counters exist: the puts made them.
	usage, err := owner.Usage(t.Context(), "zulu")
	require.NoError(t, err)
	require.Equal(t, metastore.Usage{Objects: 1, Bytes: 100}, usage)

	step, err := owner.ReadBackfill(t.Context(), learned, "", 0)
	require.NoError(t, err)
	require.True(t, step.Done)

	assert.Len(t, step.Entries, 2, "two objects, and no counter rows among them")

	for _, e := range step.Entries {
		assert.NotEmpty(t, e.Key, "a counter row decoded as an entry has no key")
	}
}

// TestBackfillOfAnEmptyRangeIsDoneImmediately: a move of a range holding
// nothing must terminate rather than loop looking for a first key.
func TestBackfillOfAnEmptyRangeIsDoneImmediately(t *testing.T) {
	owner := openShard(t, learned)

	step, err := owner.ReadBackfill(t.Context(), learned, "", 0)
	require.NoError(t, err)

	assert.True(t, step.Done)
	assert.Empty(t, step.Entries)
	assert.Empty(t, step.Cursor)
}

// TestBackfillCarriesTheOrderingStamps: the supersede rule is decided by them,
// so a copy that dropped them would make every backfilled entry unorderable
// against the log — and the last writer would win by arrival rather than by age.
func TestBackfillCarriesTheOrderingStamps(t *testing.T) {
	owner := openShard(t, learned)

	e := entry("photos", "a.jpg", 100, 7)
	e.Modified = time.Unix(1700000000, 0).UTC()
	e.Generation = "gen-3"
	require.NoError(t, owner.Put(t.Context(), e))

	step, err := owner.ReadBackfill(t.Context(), learned, "", 0)
	require.NoError(t, err)
	require.Len(t, step.Entries, 1)

	got := step.Entries[0]
	assert.Equal(t, int64(7), got.Seq)
	assert.Equal(t, "gen-3", got.Generation)
	assert.Equal(t, e.Modified, got.Modified)
}

// TestALearnerShipsNothing: shipping is the owner's job, and a learner that
// shipped what it learns would send the backfill straight back to the node it
// came from — an amplification loop that grows with every replica.
//
// The distinction is the same one the whole learner concept rests on: a learner
// receives, it does not serve, and it is not a node anyone replicates from.
func TestALearnerShipsNothing(t *testing.T) {
	shipped := 0

	learner, err := shardstore.OpenShard(t.TempDir(),
		shardstore.WithShipper(func(context.Context, rangemap.Range, []byte) { shipped++ }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = learner.Close() })

	learner.Configure(nil, nil, []rangemap.Range{learned})

	require.NoError(t, learner.Learn(t.Context(), learned,
		[]metastore.Entry{entry("photos", "a.jpg", 100, 1)}))

	assert.Zero(t, shipped)

	// And an owner still does ship, so this is not the shipper being broken.
	owner, err := shardstore.OpenShard(t.TempDir(),
		shardstore.WithShipper(func(context.Context, rangemap.Range, []byte) { shipped++ }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })

	owner.Adopt([]rangemap.Range{learned})
	require.NoError(t, owner.Put(t.Context(), entry("photos", "b.jpg", 100, 1)))

	assert.Equal(t, 1, shipped)
}
