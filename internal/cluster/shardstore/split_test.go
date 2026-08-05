package shardstore_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/metastore/keyspace"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
)

// fill writes n objects into a bucket with padded sequential keys, and flushes,
// because pebble's size estimate reads table metadata — data still in the
// memtable is data it cannot see.
func fill(t *testing.T, s *shardstore.Shard, bucket string, n int) {
	t.Helper()

	for i := range n {
		require.NoError(t, s.Put(t.Context(),
			entry(bucket, fmt.Sprintf("%08d/%s", i, strings.Repeat("x", 200)), 1024, 1)))
	}

	require.NoError(t, s.Flush())
}

// TestSplitPointDividesTheData is what the descent exists for, and what a
// midpoint of the key space cannot do.
//
// Every key here is in one bucket, so the presplit's boundaries — spread across
// the byte band bucket names start in — put all of it in a single range. A
// split point has to come from the data.
func TestSplitPointDividesTheData(t *testing.T) {
	shard := openShard(t, whole)
	fill(t, shard, "photos", 4000)

	at, ok, err := shard.SplitPoint(whole)
	require.NoError(t, err)
	require.True(t, ok, "a range with thousands of keys has a split point")

	m := &rangemap.Map{Ranges: []rangemap.Range{whole}}

	split, err := m.Split(at)
	require.NoError(t, err, "the proposed point must actually be splittable")
	require.Len(t, split.Ranges, 2)

	left, err := shard.RangeSize(split.Ranges[0])
	require.NoError(t, err)

	right, err := shard.RangeSize(split.Ranges[1])
	require.NoError(t, err)

	total := float64(left.Bytes + right.Bytes)
	require.Positive(t, total)

	share := float64(left.Bytes) / total
	assert.InDelta(t, 0.5, share, 0.15,
		"the halves are %d and %d bytes, which is not a division of the data",
		left.Bytes, right.Bytes)
}

// TestSplitPointIsBounded: every boundary is a key stored in etcd for the life
// of the cluster, so the descent has to stop.
//
// Not *short*, though — an earlier version of this test asserted eight bytes,
// which is a cap the data cannot honor: keys are 'o' + bucket + NUL + key, so
// a boundary inside a bucket necessarily carries the bucket name before it can
// say anything about the keys. That cap is what produced a 99.9/0.1 "split".
func TestSplitPointIsBounded(t *testing.T) {
	shard := openShard(t, whole)
	fill(t, shard, "photos", 2000)

	at, ok, err := shard.SplitPoint(whole)
	require.NoError(t, err)
	require.True(t, ok)

	assert.LessOrEqual(t, len(at), 32, "boundary %q exceeds the descent depth", at)
	assert.Greater(t, len(at), len("ophotos"),
		"a boundary dividing one bucket has to get past its name")
}

// TestSplitPointRefusesAnEmptyRange: inventing a boundary for a range with
// nothing in it produces an empty half that splits again next pass, forever.
func TestSplitPointRefusesAnEmptyRange(t *testing.T) {
	shard := openShard(t, whole)

	_, ok, err := shard.SplitPoint(whole)
	require.NoError(t, err)
	assert.False(t, ok, "nothing to divide")
}

// TestSplitPointStaysInsideTheRange: a boundary below Start does not divide the
// range, and one at or above End belongs to the next one. Either is a map edit
// Split refuses, so a proposal that cannot be applied is worse than none.
//
// The data is deliberately lopsided — almost all of it below the range under
// test — so that a descent measuring the *whole* shard would answer with a key
// far outside it. An even fixture cannot tell the two apart: the global median
// happens to land inside, and the test passes without the bounds doing
// anything, which is exactly how the first version of this test passed.
func TestSplitPointStaysInsideTheRange(t *testing.T) {
	shard := openShard(t, whole)

	fill(t, shard, "alpha", 3000)
	fill(t, shard, "omega", 400)

	bounded := rangemap.Range{
		Start: string(keyspace.BucketPrefix("omega")),
		Owner: "n0",
	}

	shard.Adopt([]rangemap.Range{bounded})

	at, ok, err := shard.SplitPoint(bounded)
	require.NoError(t, err)
	require.True(t, ok)

	assert.Greater(t, at, bounded.Start,
		"boundary %q is below the range, where most of the shard happens to be", at)

	// And it really splits, which is the only test that matters to the caller.
	m := &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: bounded.Start, Owner: "n0"},
		bounded,
	}}
	require.NoError(t, m.Validate())

	split, err := m.Split(at)
	require.NoError(t, err)
	assert.Len(t, split.Ranges, 3)

	// The halves divide *this* range, not the shard: a descent that measured
	// everything would put the whole of omega on one side.
	left, err := shard.RangeSize(split.Ranges[1])
	require.NoError(t, err)

	right, err := shard.RangeSize(split.Ranges[2])
	require.NoError(t, err)

	total := float64(left.Bytes + right.Bytes)
	require.Positive(t, total)
	assert.InDelta(t, 0.5, float64(left.Bytes)/total, 0.2,
		"halves are %d and %d bytes", left.Bytes, right.Bytes)
}

// TestSplitPointRespectsAnUpperBound is the mirror: a range that ends before the
// bulk of the data must not be handed a boundary from beyond its End.
func TestSplitPointRespectsAnUpperBound(t *testing.T) {
	shard := openShard(t, whole)

	fill(t, shard, "alpha", 400)
	fill(t, shard, "omega", 3000)

	bounded := rangemap.Range{
		End:   string(keyspace.BucketPrefix("omega")),
		Owner: "n0",
	}

	shard.Adopt([]rangemap.Range{bounded})

	at, ok, err := shard.SplitPoint(bounded)
	require.NoError(t, err)
	require.True(t, ok)

	assert.Less(t, at, bounded.End,
		"boundary %q is above the range, where most of the shard happens to be", at)
	assert.Greater(t, at, "", "a boundary has to be somewhere")
}

// TestRangeSizeIsPerRange: the estimate has to describe the range it was asked
// about, not the shard.
//
// Worth its own test because the split assertions cannot tell: if every range
// reported the whole shard, the two halves would report the same number and
// look perfectly balanced.
func TestRangeSizeIsPerRange(t *testing.T) {
	shard := openShard(t, whole)

	fill(t, shard, "alpha", 2400)
	fill(t, shard, "omega", 300)

	boundary := string(keyspace.BucketPrefix("omega"))

	big, err := shard.RangeSize(rangemap.Range{End: boundary, Owner: "n0"})
	require.NoError(t, err)

	small, err := shard.RangeSize(rangemap.Range{Start: boundary, Owner: "n0"})
	require.NoError(t, err)

	all, err := shard.RangeSize(whole)
	require.NoError(t, err)

	assert.Greater(t, big.Bytes, small.Bytes*3, "the ranges hold 2400 and 300 objects")
	assert.InDelta(t, all.Bytes, big.Bytes+small.Bytes, float64(all.Bytes)*0.05,
		"the halves account for the whole")
}

// TestSplitPointGivesUpBelowItsOwnDepth: a range whose Start is longer than the
// descent can reach has no split point this method can find, and it says so.
//
// Reporting one anyway would mean a key at or below Start — which Split refuses,
// so the caller would retry it every pass forever. Ranges get boundaries this
// long by being split repeatedly, so it is a state the plane reaches on its own.
func TestSplitPointGivesUpBelowItsOwnDepth(t *testing.T) {
	shard := openShard(t, whole)

	deep := "z" + strings.Repeat("q", 64)
	fill(t, shard, deep, 400)

	bounded := rangemap.Range{Start: string(keyspace.BucketPrefix(deep)), Owner: "n0"}
	shard.Adopt([]rangemap.Range{bounded})

	at, ok, err := shard.SplitPoint(bounded)
	require.NoError(t, err)

	if ok {
		assert.Greater(t, at, bounded.Start,
			"a reported point must be one Split would accept")
	}
}

// TestSplitPointConverges: splitting repeatedly must make progress. A range
// split into halves that are then split again is what a growing cluster does,
// and a descent that kept proposing the same boundary would loop.
func TestSplitPointConverges(t *testing.T) {
	shard := openShard(t, whole)
	fill(t, shard, "photos", 6000)

	m := &rangemap.Map{Ranges: []rangemap.Range{whole}}

	seen := map[string]bool{}

	for range 3 {
		// Always split the largest range, which is what a policy would do.
		var (
			biggest rangemap.Range
			maxSize uint64
		)

		for _, r := range m.Ranges {
			size, err := shard.RangeSize(r)
			require.NoError(t, err)

			if size.Bytes >= maxSize {
				biggest, maxSize = r, size.Bytes
			}
		}

		at, ok, err := shard.SplitPoint(biggest)
		require.NoError(t, err)
		require.True(t, ok, "range [%q,%q) holding %d bytes has no split point",
			biggest.Start, biggest.End, maxSize)

		require.False(t, seen[at], "proposed the same boundary %q twice", at)
		seen[at] = true

		next, err := m.Split(at)
		require.NoError(t, err)

		m = next
		shard.Adopt(m.Ranges)
	}

	require.Len(t, m.Ranges, 4)

	// Every key is still served, which a split must never change.
	var served int

	require.NoError(t, shard.Scan(t.Context(), "photos", "", "", 0, func(e metastore.Entry) error {
		served++

		return nil
	}))

	assert.Equal(t, 6000, served)
}

// measurements answers PlanSplits from a fixed table, and records which owners
// were asked — so a test can tell "measured and declined" from "never asked".
type measurements struct {
	byStart map[string]shardstore.Measurement
	fail    map[cluster.NodeID]bool
	asked   []cluster.NodeID
}

func (m *measurements) measure(
	_ context.Context, node cluster.NodeID, r rangemap.Range,
) (shardstore.Measurement, error) {
	m.asked = append(m.asked, node)

	if m.fail[node] {
		return shardstore.Measurement{}, errors.Errorf("node %s is unreachable", node)
	}

	return m.byStart[r.Start], nil
}

// survey is what a pass hands the planners: one entry per range, nil where the
// owner could not be reached.
func (m *measurements) survey(t *testing.T, in *rangemap.Map) shardstore.Survey {
	t.Helper()

	out := make(shardstore.Survey, len(in.Ranges))

	for i, r := range in.Ranges {
		got, err := m.measure(t.Context(), r.Owner, r)
		if err != nil {
			continue
		}

		out[i] = &got
	}

	return out
}

// TestPlanSplitsTakesTheLargestFirst: the cap is reached long before the work is
// done on a cluster that has just been switched on, so finishing the alphabet
// while one enormous range waits is the wrong order to make progress in.
func TestPlanSplitsTakesTheLargestFirst(t *testing.T) {
	m := &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "ob", Owner: "n0"},
		{Start: "ob", End: "oc", Owner: "n1"},
		{Start: "oc", End: "", Owner: "n2"},
	}}
	require.NoError(t, m.Validate())

	table := &measurements{byStart: map[string]shardstore.Measurement{
		"":   {Bytes: 200, SplitAt: "oa"},
		"ob": {Bytes: 900, SplitAt: "obm"},
		"oc": {Bytes: 500, SplitAt: "ocm"},
	}}

	plan := shardstore.PlanSplits(m, table.survey(t, m), shardstore.SplitPolicy{MaxBytes: 100, MaxSplitsPerPass: 2})

	assert.Equal(t, []string{"obm", "ocm"}, plan, "the two largest, in that order")
}

// TestPlanSplitsLeavesSmallRangesAlone: a partition finer than the data warrants
// costs a boundary in etcd and a routing entry on every node, forever.
func TestPlanSplitsLeavesSmallRangesAlone(t *testing.T) {
	m := &rangemap.Map{Ranges: []rangemap.Range{{Start: "", End: "", Owner: "n0"}}}

	table := &measurements{byStart: map[string]shardstore.Measurement{
		"": {Bytes: 100, SplitAt: "om"},
	}}

	assert.Empty(t, shardstore.PlanSplits(m, table.survey(t, m), shardstore.SplitPolicy{MaxBytes: 1000}))
}

// TestPlanSplitsSkipsARangeWithNoPoint: an owner reporting no split point has a
// range that cannot be divided — empty, or already bounded deeper than the
// descent reaches. Splitting it anyway would need a boundary nobody computed.
func TestPlanSplitsSkipsARangeWithNoPoint(t *testing.T) {
	m := &rangemap.Map{Ranges: []rangemap.Range{{Start: "", End: "", Owner: "n0"}}}

	table := &measurements{byStart: map[string]shardstore.Measurement{
		"": {Bytes: 1 << 40, SplitAt: ""},
	}}

	assert.Empty(t, shardstore.PlanSplits(m, table.survey(t, m), shardstore.SplitPolicy{MaxBytes: 100}))
}

// TestPlanSplitsSkipsAnUnreachableOwner: splitting on a stale size would be a
// map edit made from a number nobody currently stands behind, and an oversized
// range is a slow problem — waiting a pass costs nothing.
func TestPlanSplitsSkipsAnUnreachableOwner(t *testing.T) {
	m := &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "ob", Owner: "gone"},
		{Start: "ob", End: "", Owner: "n1"},
	}}
	require.NoError(t, m.Validate())

	table := &measurements{
		byStart: map[string]shardstore.Measurement{
			"":   {Bytes: 1 << 40, SplitAt: "oa"},
			"ob": {Bytes: 900, SplitAt: "obm"},
		},
		fail: map[cluster.NodeID]bool{"gone": true},
	}

	plan := shardstore.PlanSplits(m, table.survey(t, m), shardstore.SplitPolicy{MaxBytes: 100})

	assert.Equal(t, []string{"obm"}, plan,
		"the unreachable owner's range is skipped, not assumed")
	assert.Contains(t, table.asked, cluster.NodeID("gone"), "it was asked, and it failed")
}

// TestTheSurveyAsksEachRangesOwner: a controller measuring its own shard would
// be measuring whichever ranges it happens to own and calling that the cluster.
//
// At the controller rather than the planner, because that is where the asking
// moved when the survey became something taken once and shared.
func TestTheSurveyAsksEachRangesOwner(t *testing.T) {
	m := &rangemap.Map{Revision: 7, Ranges: []rangemap.Range{
		{Start: "", End: "ob", Owner: "n0"},
		{Start: "ob", End: "oc", Owner: "n1"},
		{Start: "oc", End: "", Owner: "n2"},
	}}
	require.NoError(t, m.Validate())

	table := &measurements{byStart: map[string]shardstore.Measurement{}}
	ctl := &recorder{m: m, build: metastore.Ready()}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: ctl.load, Save: ctl.save,
		Live:    func() []cluster.NodeID { return []cluster.NodeID{"n0", "n1", "n2"} },
		Measure: table.measure,
		Now:     time.Now,
	})
	require.NoError(t, err)

	_, err = c.Reconcile(t.Context())
	require.NoError(t, err)

	assert.Equal(t, []cluster.NodeID{"n0", "n1", "n2"}, table.asked,
		"each range is measured by whoever holds it, once")
}

// TestPlanSplitsIsAFunctionOfTheMeasurements: an election has a window where
// two candidates both believe they hold it, so the plan has to depend on the
// measurements and nothing else — then that window produces a duplicate write
// rather than a disagreement.
//
// Two earlier versions of this test claimed more than they checked. The first
// asserted that an explicit tie-break on the boundary was what made the plan
// reproducible; removing the tie-break changed nothing, because sorting
// identical input is reproducible either way. The second tried to assert
// stability instead, which is equally unobservable: pdqsort leaves all-equal
// input alone at every size. What is actually owed is below.
func TestPlanSplitsIsAFunctionOfTheMeasurements(t *testing.T) {
	var (
		ranges []rangemap.Range
		table  = &measurements{byStart: map[string]shardstore.Measurement{}}
	)

	// Sizes that are neither sorted nor equal, so the ordering has real work to
	// do and the answer is not the input.
	sizes := []uint64{400, 9000, 700, 200, 5000, 300, 8000, 600}

	for i, size := range sizes {
		start := ""
		if i > 0 {
			start = fmt.Sprintf("o%02d", i)
		}

		end := ""
		if i < len(sizes)-1 {
			end = fmt.Sprintf("o%02d", i+1)
		}

		ranges = append(ranges, rangemap.Range{Start: start, End: end, Owner: "n0"})
		table.byStart[start] = shardstore.Measurement{Bytes: size, SplitAt: start + "m"}
	}

	m := &rangemap.Map{Ranges: ranges}
	require.NoError(t, m.Validate())

	policy := shardstore.SplitPolicy{MaxBytes: 100, MaxSplitsPerPass: 3}

	first := shardstore.PlanSplits(m, table.survey(t, m), policy)
	second := shardstore.PlanSplits(m, table.survey(t, m), policy)

	require.Equal(t, first, second, "the same measurements must give the same plan")
	assert.Equal(t, []string{"o01m", "o06m", "o04m"}, first,
		"largest first: 9000, 8000, 5000")
}

// TestPlanSplitsBoundsMapChurn: each split is a control-plane write and a map
// revision every node in the cluster refetches. A freshly switched-on cluster
// with every range over the threshold would otherwise publish thousands of
// revisions in one pass and then serve nothing but map reads.
func TestPlanSplitsBoundsMapChurn(t *testing.T) {
	var (
		ranges []rangemap.Range
		table  = &measurements{byStart: map[string]shardstore.Measurement{}}
	)

	for i := range 40 {
		start := ""
		if i > 0 {
			start = fmt.Sprintf("o%02d", i)
		}

		end := ""
		if i < 39 {
			end = fmt.Sprintf("o%02d", i+1)
		}

		ranges = append(ranges, rangemap.Range{Start: start, End: end, Owner: "n0"})
		table.byStart[start] = shardstore.Measurement{
			Bytes: 1 << 30, SplitAt: start + "m",
		}
	}

	m := &rangemap.Map{Ranges: ranges}
	require.NoError(t, m.Validate())

	plan := shardstore.PlanSplits(m, table.survey(t, m), shardstore.SplitPolicy{MaxBytes: 1})
	assert.Len(t, plan, shardstore.DefaultMaxSplitsPerPass)
}
