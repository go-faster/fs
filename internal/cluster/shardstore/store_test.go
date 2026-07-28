package shardstore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/metastore/metastoretest"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
)

// cluster3 is a plane whose ranges are split across three shards, all in this
// process. What is missing is only the wire: the composition being tested —
// which shard to ask, in what order, how to combine the answers — is the same
// whether a Backend is a local shard or a peer client.
type planeCluster struct {
	store  *shardstore.Store
	shards map[cluster.NodeID]*shardstore.Shard
}

// newPlane builds a plane over n ranges spread across the given nodes.
func newPlane(t testing.TB, ranges int, nodes ...cluster.NodeID) *planeCluster {
	t.Helper()

	if len(nodes) == 0 {
		nodes = []cluster.NodeID{"n0"}
	}

	m, err := rangemap.Initial(ranges, nodes)
	require.NoError(t, err)

	m.Revision = 1

	shards := make(map[cluster.NodeID]*shardstore.Shard, len(nodes))

	for _, node := range nodes {
		s, err := shardstore.OpenShard(t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		s.Adopt(m.RangesFor(node))
		shards[node] = s
	}

	router := shardstore.NewRouter(func(context.Context) (*rangemap.Map, error) { return m, nil })

	resolve := func(_ context.Context, node cluster.NodeID) (shardstore.Backend, error) {
		s, ok := shards[node]
		if !ok {
			return nil, assert.AnError
		}

		return s, nil
	}

	return &planeCluster{
		store:  shardstore.NewStore(router, resolve, &shardstore.MemoryReadiness{}),
		shards: shards,
	}
}

// TestConformanceOneRange holds the plane to the same contract as every other
// backend, with the partitioning out of the way.
func TestConformanceOneRange(t *testing.T) {
	metastoretest.Run(t, func(tb testing.TB) metastore.Store {
		return newPlane(tb, 1, "n0").store
	})
}

// TestConformanceSharded is the acceptance signal for E3: the identical suite,
// against a plane whose ranges are split across three nodes.
//
// Every case that passes here passes through the fan-out — keys routed to
// different shards, listings walked across range boundaries, counters summed
// from partials, coverage combined. A plane that answered differently from a
// single-node store would be one no caller could be written against.
func TestConformanceSharded(t *testing.T) {
	metastoretest.Run(t, func(tb testing.TB) metastore.Store {
		return newPlane(tb, 12, "n0", "n1", "n2").store
	})
}

// splitBucketPlane puts a range boundary *inside* the conformance suite's test
// bucket, so its objects land on two shards.
//
// The presplit cannot do this: its boundaries fall at a bucket's first letter,
// so a whole bucket lands in one range and TestConformanceSharded — despite the
// name — exercises the fan-out only across buckets. A boundary inside a bucket
// is what a real split produces once one bucket outgrows a range, and it is the
// case ScanRange-per-range exists for.
func splitBucketPlane(t testing.TB) *planeCluster {
	t.Helper()

	// The suite's testBucket is "photos"; its keys encode as
	// 'o' + "photos" + NUL + key, so this boundary cuts it at "m".
	const inside = "ophotos\x00m"

	m := &rangemap.Map{
		Revision: 1,
		Ranges: []rangemap.Range{
			{Start: "", End: inside, Owner: "n0"},
			{Start: inside, End: "", Owner: "n1"},
		},
	}
	require.NoError(t, m.Validate())

	shards := make(map[cluster.NodeID]*shardstore.Shard, 2)

	for _, node := range []cluster.NodeID{"n0", "n1"} {
		s, err := shardstore.OpenShard(t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		s.Adopt(m.RangesFor(node))
		shards[node] = s
	}

	router := shardstore.NewRouter(func(context.Context) (*rangemap.Map, error) { return m, nil })

	resolve := func(_ context.Context, node cluster.NodeID) (shardstore.Backend, error) {
		s, ok := shards[node]
		if !ok {
			return nil, assert.AnError
		}

		return s, nil
	}

	return &planeCluster{
		store:  shardstore.NewStore(router, resolve, &shardstore.MemoryReadiness{}),
		shards: shards,
	}
}

// TestConformanceSplitBucket is the strongest signal available: the whole
// suite, with a range boundary cutting the test bucket in half.
//
// Every listing case now walks two shards, every counter is a sum of two
// partials, and the paging cursor crosses a boundary mid-page. A plane that got
// the ordering or the summing wrong fails here and passes everywhere else.
func TestConformanceSplitBucket(t *testing.T) {
	metastoretest.Run(t, func(tb testing.TB) metastore.Store {
		return splitBucketPlane(tb).store
	})
}

// TestSplitBucketReallySplits guards the fixture above. If both halves landed
// on one shard, the conformance run would be green for the wrong reason — and
// nothing else in the suite would notice.
func TestSplitBucketReallySplits(t *testing.T) {
	p := splitBucketPlane(t)

	for _, key := range []string{"a", "z"} {
		require.NoError(t, p.store.Put(t.Context(), metastoretest.Entry("photos", key, 1, 1)))
	}

	held := make(map[cluster.NodeID][]string)

	for node, s := range p.shards {
		require.NoError(t, s.Scan(t.Context(), "photos", "", "", 0, func(e metastore.Entry) error {
			held[node] = append(held[node], e.Key)

			return nil
		}))
	}

	assert.Equal(t, []string{"a"}, held["n0"], "keys below the boundary")
	assert.Equal(t, []string{"z"}, held["n1"], "keys above it")
}

// TestScanCrossesRangeBoundariesInOrder: a bucket large enough to span ranges
// must still list in key order, and the walk is by range rather than by owner
// precisely because one node can own ranges either side of another's.
func TestScanCrossesRangeBoundariesInOrder(t *testing.T) {
	p := newPlane(t, 12, "n0", "n1", "n2")

	// Bucket names chosen to land in different ranges: the presplit boundaries
	// spread across the band bucket names start in.
	buckets := []string{"alpha", "delta", "kilo", "romeo", "yankee"}
	for _, b := range buckets {
		require.NoError(t, p.store.Put(t.Context(), metastoretest.Entry(b, "k", 1, 1)))
	}

	var got []string

	require.NoError(t, p.store.Buckets(t.Context(), func(bucket string) error {
		got = append(got, bucket)

		return nil
	}))

	assert.Equal(t, buckets, got, "buckets come back in name order across shards")

	// And each bucket's own keys list in order from whichever shard holds them.
	for _, key := range []string{"c", "a", "b"} {
		require.NoError(t, p.store.Put(t.Context(), metastoretest.Entry("alpha", key, 1, 1)))
	}

	var keys []string

	require.NoError(t, p.store.Scan(t.Context(), "alpha", "", "", 0, func(e metastore.Entry) error {
		keys = append(keys, e.Key)

		return nil
	}))

	assert.Equal(t, []string{"a", "b", "c", "k"}, keys)
}

// TestUsageSumsPartials is the counters resolution end to end: each shard
// counts what it holds, and the bucket's total is the sum. Asked once per
// distinct owner, because a shard's counters are per bucket rather than per
// range — asking per range would double an owner holding two of them.
func TestUsageSumsPartials(t *testing.T) {
	p := newPlane(t, 12, "n0", "n1", "n2")

	for _, b := range []string{"alpha", "romeo", "yankee"} {
		require.NoError(t, p.store.Put(t.Context(), metastoretest.Entry(b, "k", 100, 1)))
	}

	// Each bucket's total is its own, wherever it lives.
	for _, b := range []string{"alpha", "romeo", "yankee"} {
		usage, err := p.store.Usage(t.Context(), b)
		require.NoError(t, err)
		assert.Equal(t, metastore.Usage{Objects: 1, Bytes: 100}, usage, "bucket %s", b)
	}

	// A bucket nobody holds is zero, not an error.
	usage, err := p.store.Usage(t.Context(), "absent")
	require.NoError(t, err)
	assert.Zero(t, usage.Objects)
}

// TestCoverageCombinesPartials: coverage is only as good as the least recently
// checked object anywhere, not as good as the best shard.
func TestCoverageCombinesPartials(t *testing.T) {
	p := newPlane(t, 12, "n0", "n1", "n2")

	for _, b := range []string{"alpha", "yankee"} {
		require.NoError(t, p.store.Put(t.Context(), metastoretest.Entry(b, "k", 1, 1)))
	}

	cov, err := p.store.Coverage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(2), cov.Objects, "objects on both shards are counted once each")
	assert.Equal(t, int64(2), cov.Never)
}

// TestReadinessIsClusterWide: the plane is usable only when all of it is. A
// per-shard flag would let one node report ready while the cluster is half
// rebuilt, and a listing served then reports a fraction as the whole.
func TestReadinessIsClusterWide(t *testing.T) {
	p := newPlane(t, 12, "n0", "n1", "n2")

	state, err := p.store.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state, "an unbuilt plane is not trusted")

	require.NoError(t, p.store.MarkReady(t.Context()))

	state, err = p.store.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateReady, state)
}

// TestResetMarksBuildingBeforeEmptying: a reset that emptied first would leave
// a window where the plane reads ready and holds nothing, and a listing served
// in it reports an empty cluster as the truth.
func TestResetMarksBuildingBeforeEmptying(t *testing.T) {
	p := newPlane(t, 12, "n0", "n1", "n2")

	require.NoError(t, p.store.Put(t.Context(), metastoretest.Entry("alpha", "k", 1, 1)))
	require.NoError(t, p.store.MarkReady(t.Context()))

	require.NoError(t, p.store.Reset(t.Context()))

	state, err := p.store.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state)

	usage, err := p.store.Usage(t.Context(), "alpha")
	require.NoError(t, err)
	assert.Zero(t, usage.Objects, "every shard was emptied, not just the first")
}

// TestKeysReachTheShardThatOwnsThem: the routing is real, not a single shard
// pretending. Each key must be on exactly one shard, or the plane is storing
// duplicates that will diverge.
func TestKeysReachTheShardThatOwnsThem(t *testing.T) {
	p := newPlane(t, 12, "n0", "n1", "n2")

	buckets := []string{"alpha", "delta", "kilo", "romeo", "yankee"}
	for _, b := range buckets {
		require.NoError(t, p.store.Put(t.Context(), metastoretest.Entry(b, "k", 1, 1)))
	}

	for _, b := range buckets {
		holders := 0

		for _, s := range p.shards {
			// Asked directly, bypassing routing: only the owner may have it.
			if _, found, err := s.Get(t.Context(), b, "k"); err == nil && found {
				holders++
			}
		}

		assert.Equal(t, 1, holders, "bucket %s must live on exactly one shard", b)
	}
}
