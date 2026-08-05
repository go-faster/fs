package shardstore_test

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// loaded is a shard whose clock a test winds by hand.
//
// A rate is a quantity per unit of wall time, so a test that spent that time
// would either be slow or be measuring an interval too short to divide by.
type loaded struct {
	*shardstore.Shard

	at time.Time
}

func (l *loaded) advance(d time.Duration) { l.at = l.at.Add(d) }

func openLoaded(t *testing.T, ranges ...rangemap.Range) *loaded {
	t.Helper()

	l := &loaded{at: time.Unix(1700000000, 0)}

	s, err := shardstore.OpenShard(t.TempDir(),
		shardstore.WithShardClock(func() time.Time { return l.at }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	s.Adopt(ranges)
	l.Shard = s

	return l
}

// writes puts n objects into a bucket.
func writes(t *testing.T, s *loaded, bucket string, n int) {
	t.Helper()

	for i := range n {
		require.NoError(t, s.Put(t.Context(),
			entry(bucket, fmt.Sprintf("%04d.jpg", i), 100, 1)))
	}
}

// TestWriteRateMeasuresRecentWrites is what the whole thing is for: a rate that
// says what a range is taking now, so placement is a decision about the present.
func TestWriteRateMeasuresRecentWrites(t *testing.T) {
	s := openLoaded(t, whole)

	// The first read has no interval behind it, so it starts the clock rather
	// than inventing a rate.
	assert.Zero(t, s.WriteRate(whole))

	writes(t, s, "photos", 60)
	s.advance(time.Minute)

	assert.InDelta(t, 1.0, s.WriteRate(whole), 0.001,
		"sixty writes in a minute is one a second")
}

// TestWriteRateDecaysWhenTheRangeGoesQuiet: a count says what a range has ever
// had; placement is a question about now. A range that was busy and stopped must
// stop being chosen.
func TestWriteRateDecaysWhenTheRangeGoesQuiet(t *testing.T) {
	s := openLoaded(t, whole)

	require.Zero(t, s.WriteRate(whole))

	writes(t, s, "photos", 600)
	s.advance(time.Minute)

	busy := s.WriteRate(whole)
	require.Positive(t, busy)

	// Nothing written for a long while.
	for range 10 {
		s.advance(time.Minute)
		s.WriteRate(whole)
	}

	assert.Less(t, s.WriteRate(whole), busy/10,
		"the rate still describes traffic that stopped ten minutes ago")
}

// TestWriteRateIsPerRange: placement moves one range, so a rate that pooled the
// node's traffic would say nothing about which.
func TestWriteRateIsPerRange(t *testing.T) {
	left := rangemap.Range{Start: "", End: "om", Owner: "n0"}
	right := rangemap.Range{Start: "om", End: "", Owner: "n0"}

	s := openLoaded(t, left, right)

	require.Zero(t, s.WriteRate(left))
	require.Zero(t, s.WriteRate(right))

	writes(t, s, "zulu", 120)
	s.advance(time.Minute)

	assert.Zero(t, s.WriteRate(left), "the quiet range took someone else's writes")
	assert.Positive(t, s.WriteRate(right))
}

// TestWriteRateIgnoresRefusedWrites: a range this shard does not own takes no
// traffic here, and counting the refusal would make the busiest node the one
// with the most stale callers.
func TestWriteRateIgnoresRefusedWrites(t *testing.T) {
	bounded := rangemap.Range{Start: "", End: "om", Owner: "n0"}

	s := openLoaded(t, bounded)

	require.Zero(t, s.WriteRate(bounded))

	for range 60 {
		require.ErrorIs(t, s.Put(t.Context(), entry("zulu", "a.jpg", 100, 1)),
			shardstore.ErrNotOwned)
	}

	s.advance(time.Minute)
	assert.Zero(t, s.WriteRate(bounded))
}

// TestWriteRateCountsDeletes: a delete takes the same batch, counter update and
// shipping as a put. A range being emptied fast is busy, and one measured on
// puts alone would read as idle while it was.
func TestWriteRateCountsDeletes(t *testing.T) {
	s := openLoaded(t, whole)

	writes(t, s, "photos", 60)
	require.Zero(t, s.WriteRate(whole))

	s.advance(time.Minute)
	require.Positive(t, s.WriteRate(whole))

	// Quiet for long enough that the puts are forgotten.
	for range 10 {
		s.advance(time.Minute)
		s.WriteRate(whole)
	}

	quiet := s.WriteRate(whole)

	for i := range 60 {
		require.NoError(t, s.Delete(t.Context(), "photos", fmt.Sprintf("%04d.jpg", i)))
	}

	s.advance(time.Minute)
	assert.Greater(t, s.WriteRate(whole), quiet*10, "deletes were not counted")
}

// TestWriteRateSurvivesAnUnrelatedMapChange: a cluster that splits a range
// somewhere else must not reset the load of every other range, which would make
// every split look like a lull.
func TestWriteRateSurvivesAnUnrelatedMapChange(t *testing.T) {
	s := openLoaded(t, whole)

	require.Zero(t, s.WriteRate(whole))

	writes(t, s, "photos", 60)
	s.advance(time.Minute)

	before := s.WriteRate(whole)
	require.Positive(t, before)

	// Another range arrives; this one is untouched.
	s.Adopt([]rangemap.Range{whole, {Start: "oz", End: "", Owner: "n1"}})

	assert.InDelta(t, before, s.WriteRate(whole), 0.0001)
}

// TestWriteRateOfANewRangeStartsAtZero: the halves of a split are new ranges,
// and a rate inherited from the range they replaced would describe traffic split
// between them. Starting at zero defers a decision by a sample; inheriting makes
// a wrong one immediately.
func TestWriteRateOfANewRangeStartsAtZero(t *testing.T) {
	s := openLoaded(t, whole)

	require.Zero(t, s.WriteRate(whole))

	writes(t, s, "photos", 60)
	s.advance(time.Minute)
	require.Positive(t, s.WriteRate(whole))

	left := rangemap.Range{Start: "", End: "om", Owner: "n0"}
	right := rangemap.Range{Start: "om", End: "", Owner: "n0"}
	s.Adopt([]rangemap.Range{left, right})

	s.advance(time.Minute)
	assert.Zero(t, s.WriteRate(left))
	assert.Zero(t, s.WriteRate(right))
}

// TestWriteRateOfAnUnknownRangeIsZero: a range this shard does not own has no
// traffic here to report, and inventing one would have a controller compare a
// measurement against a guess.
func TestWriteRateOfAnUnknownRangeIsZero(t *testing.T) {
	s := openLoaded(t, rangemap.Range{Start: "", End: "om", Owner: "n0"})

	assert.Zero(t, s.WriteRate(rangemap.Range{Start: "om", End: "", Owner: "n1"}))
}

// TestMeasureCarriesTheRateOverTheWire: the controller measures ranges it does
// not own, so a rate that stayed on the owner would be a rate nothing could act
// on.
func TestMeasureCarriesTheRateOverTheWire(t *testing.T) {
	s := openLoaded(t, whole)

	require.Zero(t, s.WriteRate(whole))

	writes(t, s, "photos", 600)
	require.NoError(t, s.Flush())
	s.advance(time.Minute)

	srv := httptest.NewServer(transport.NewServer(
		transport.NewMemStore(), peerSecret, transport.WithShard(shardstore.Serve(s.Shard)),
	))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, peerSecret, "n0", srv.Client())
	require.NoError(t, err)

	got, err := shardstore.NewPeer(client).Measure(t.Context(), whole)
	require.NoError(t, err)

	assert.Positive(t, got.Writes, "the rate did not survive the wire")
	assert.Positive(t, got.Bytes, "and the size still does")
}

// TestLearnedWritesAreNotLoad: a learner storing a backfill is doing this node's
// own work, not answering a client. Counting it would make every destination of
// a move look like the busiest node in the cluster — and the next rebalance would
// move the range straight back off it.
func TestLearnedWritesAreNotLoad(t *testing.T) {
	owner := openLoaded(t, learned)

	learner := openLoaded(t)
	learner.Configure(nil, nil, []rangemap.Range{learned})

	writes(t, owner, "photos", 40)
	require.NoError(t, owner.Flush())

	// The learner serves the range once it is promoted, and its load is what it
	// took while owning it: nothing.
	_, err := learner.Backfill(t.Context(), learned, owner.Shard, 10)
	require.NoError(t, err)

	learner.Adopt([]rangemap.Range{learned})
	require.Zero(t, learner.WriteRate(learned))

	learner.advance(time.Minute)
	assert.Zero(t, learner.WriteRate(learned))
}

// TestFollowedWritesAreNotLoad is the same for the log: a follower applies every
// batch its owner does, and a rate that counted them would make each replica
// look exactly as busy as the node it exists to back up.
func TestFollowedWritesAreNotLoad(t *testing.T) {
	replica := openLoaded(t)
	replica.Configure(nil, []rangemap.Range{replicated}, nil)

	owner, err := shardstore.OpenShard(t.TempDir(),
		shardstore.WithShipper(ordered(t, replica.ApplyBatch)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })

	owner.Adopt([]rangemap.Range{replicated})

	for i := range 40 {
		require.NoError(t, owner.Put(t.Context(),
			entry("photos", fmt.Sprintf("%04d.jpg", i), 100, 1)))
	}

	replica.Adopt([]rangemap.Range{replicated})
	require.Zero(t, replica.WriteRate(replicated))

	replica.advance(time.Minute)
	assert.Zero(t, replica.WriteRate(replicated))
}

// TestWriteRateIgnoresAnIntervalTooShortToDivide: below a second the divisor is
// small enough that one write reads as an enormous rate, and a controller
// measuring every few seconds would see a range flare and subside on nothing.
func TestWriteRateIgnoresAnIntervalTooShortToDivide(t *testing.T) {
	s := openLoaded(t, whole)

	require.Zero(t, s.WriteRate(whole))

	writes(t, s, "photos", 1)
	s.advance(time.Millisecond)

	assert.Zero(t, s.WriteRate(whole), "one write became a rate of a thousand a second")

	// And nothing is lost: the write folds into the next interval that is long
	// enough to divide by.
	s.advance(time.Minute)
	assert.Positive(t, s.WriteRate(whole))
}
