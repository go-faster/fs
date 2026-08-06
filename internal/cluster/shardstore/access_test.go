package shardstore_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/metastore/keyspace"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
)

// writeAt puts one object, which is what makes a range look busy at that key.
func writeAt(t *testing.T, s *shardstore.Shard, key string) {
	t.Helper()

	require.NoError(t, s.Put(t.Context(), entry("photos", key, 100, 1)))
}

// TestASplitFollowsTheTrafficNotTheBytes is #222's acceptance criterion, and
// the case E4 exists for.
//
// Sequential keys — timestamps, ULIDs, date prefixes — all land at the top of
// the key space. Dividing by stored size halves the bytes and leaves the upper
// half taking every write, so the next pass splits that half, and the one after
// splits half of it: the boundary walks toward the hot end one control-plane
// write at a time. Dividing by traffic puts it near the hot end at once.
func TestASplitFollowsTheTrafficNotTheBytes(t *testing.T) {
	s := openShard(t, whole)

	// A range full of old data, spread evenly.
	for i := range 400 {
		writeAt(t, s, fmt.Sprintf("%04d.jpg", i))
	}

	require.NoError(t, s.Flush())

	// And an ingest landing only in its top decile, as sequential keys do.
	for i := range 200 {
		writeAt(t, s, fmt.Sprintf("9%03d.jpg", i))
	}

	access := s.AccessPoint(whole)
	require.NotEmpty(t, access, "the range took writes and reported no accessed median")

	// The premise: the two medians disagree, which is the whole reason for the
	// second one. If they landed in the same place there would be nothing here.
	stored, found, err := s.SplitPoint(whole)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEqual(t, stored, access)

	hot := string(keyspace.ObjectKey("photos", "9000.jpg"))
	assert.GreaterOrEqual(t, access, hot,
		"the accessed median landed below the traffic, which is what the stored one already does")
	assert.Less(t, stored, hot, "the premise: the stored median is nowhere near the writes")
}

// TestAnUnwrittenRangeHasNoAccessedMedian: a range nobody is writing to has no
// traffic to divide, and inventing a point from a handful of samples would be a
// data move decided by noise. It splits by size, as everything did before.
func TestAnUnwrittenRangeHasNoAccessedMedian(t *testing.T) {
	s := openShard(t, whole)

	assert.Empty(t, s.AccessPoint(whole), "a range with no writes named a point anyway")

	// Still too few to say.
	for i := range 5 {
		writeAt(t, s, fmt.Sprintf("%03d.jpg", i))
	}

	assert.Empty(t, s.AccessPoint(whole), "five writes were enough to place a boundary")
}

// TestTheAccessedMedianFollowsTheWritesAsTheyMove: it is a window on recent
// traffic, not a running total. An ingest that moves up the key space must take
// the boundary with it, or the second split lands where the first should have.
func TestTheAccessedMedianFollowsTheWritesAsTheyMove(t *testing.T) {
	s := openShard(t, whole)

	for i := range 300 {
		writeAt(t, s, fmt.Sprintf("1%03d.jpg", i))
	}

	early := s.AccessPoint(whole)
	require.NotEmpty(t, early)

	for i := range 300 {
		writeAt(t, s, fmt.Sprintf("8%03d.jpg", i))
	}

	later := s.AccessPoint(whole)
	require.NotEmpty(t, later)

	assert.Greater(t, later, early,
		"the sample still describes writes that stopped, so the split would chase them")
}

// TestTheAccessedMedianIgnoresReplicatedWrites: a follower replaying the log and
// a learner taking a backfill are doing this node's own work, not answering
// clients. Counting them would have every replica report a split point for a
// range it does not serve.
func TestTheAccessedMedianIgnoresReplicatedWrites(t *testing.T) {
	learner := openShard(t)
	learner.Configure(nil, nil, []rangemap.Range{learned})

	owner := openShard(t, learned)
	for i := range 200 {
		writeAt(t, owner, fmt.Sprintf("%04d.jpg", i))
	}

	step, err := owner.ReadBackfill(t.Context(), learned, "", 0)
	require.NoError(t, err)
	require.NoError(t, learner.Learn(t.Context(), learned, step.Entries))

	learner.Adopt([]rangemap.Range{learned})
	assert.Empty(t, learner.AccessPoint(learned),
		"a backfill made the destination look like it was taking the traffic")
}

// TestTheAccessedMedianIsForgottenWithTheRange: the halves of a split are new
// ranges, and a median inherited from the range they replaced describes traffic
// split between them.
func TestTheAccessedMedianIsForgottenWithTheRange(t *testing.T) {
	s := openShard(t, whole)

	for i := range 200 {
		writeAt(t, s, fmt.Sprintf("%04d.jpg", i))
	}

	require.NotEmpty(t, s.AccessPoint(whole))

	left := rangemap.Range{Start: "", End: "om", Owner: "n0"}
	right := rangemap.Range{Start: "om", End: "", Owner: "n0"}
	s.Adopt([]rangemap.Range{left, right})

	assert.Empty(t, s.AccessPoint(left))
	assert.Empty(t, s.AccessPoint(right))
}

// TestAMedianTruncatedBelowItsRangeIsNotAPoint is the interaction between the
// two bounds, and the reachable way a median lands outside its own range.
//
// Sampled keys are truncated so that a range holding a thousand of them cannot
// hold a megabyte. A truncated key sorts at or below the one it came from — and
// when a range *begins* at a long key whose prefix its writes share, every
// sample truncates to something below the range start. Proposed as a boundary
// that divides nothing; Split would refuse it, and the pass would be spent.
func TestAMedianTruncatedBelowItsRangeIsNotAPoint(t *testing.T) {
	// A range starting deep inside one bucket's keys, past the truncation
	// bound, so the whole sample chops back to a prefix below it.
	deep := strings.Repeat("k", 200)
	start := string(keyspace.ObjectKey("photos", deep))

	top := rangemap.Range{Start: start, End: "", Owner: "n0"}

	s := openShard(t, top)

	for i := range 200 {
		writeAt(t, s, fmt.Sprintf("%s%04d", deep, i))
	}

	assert.Empty(t, s.AccessPoint(top),
		"a truncated sample proposed a boundary below the range it was meant to divide")
}

// TestTheAccessedMedianSurvivesAnUnrelatedMapChange: a cluster that splits a
// range somewhere else must not blind every other range, which would make every
// split fall back to dividing bytes for the next few hundred writes.
func TestTheAccessedMedianSurvivesAnUnrelatedMapChange(t *testing.T) {
	s := openShard(t, whole)

	for i := range 200 {
		writeAt(t, s, fmt.Sprintf("%04d.jpg", i))
	}

	before := s.AccessPoint(whole)
	require.NotEmpty(t, before)

	// Another range arrives; this one is untouched.
	s.Adopt([]rangemap.Range{whole, {Start: "oz", End: "", Owner: "n1"}})

	assert.Equal(t, before, s.AccessPoint(whole))
}

// BenchmarkPutWithSampling is #222's third acceptance criterion: the write-path
// cost is measured, not assumed. The sample is a bounded string copy under one
// mutex, so what this has to show is that it does not dominate a put.
func BenchmarkPutWithSampling(b *testing.B) {
	s, err := shardstore.OpenShard(b.TempDir())
	require.NoError(b, err)
	b.Cleanup(func() { _ = s.Close() })

	s.Adopt([]rangemap.Range{whole})

	b.ReportAllocs()

	for i := 0; b.Loop(); i++ {
		if err := s.Put(b.Context(), entry("photos", fmt.Sprintf("%08d.jpg", i), 100, 1)); err != nil {
			b.Fatal(err)
		}
	}
}
