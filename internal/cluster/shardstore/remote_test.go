package shardstore_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/metastore/metastoretest"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// peerSecret is the shared secret peers authenticate with. Any value does; the
// point is that requests and responses are signed, which the transport enforces
// whether or not a test cares.
var peerSecret = transport.Secret("shard-transport-test-secret")

// remotePlane is a plane whose shards are reached over real HTTP.
//
// Every shard gets a server and every Backend is a Peer, so nothing in the test
// resolves to a local shard. That is deliberate: a plane where some shards were
// local would let a bug in the wire hide behind the paths that never used it.
func remotePlane(t testing.TB, m *rangemap.Map) *shardstore.Store {
	t.Helper()

	require.NoError(t, m.Validate())

	nodes := map[cluster.NodeID]struct{}{}
	for _, r := range m.Ranges {
		nodes[r.Owner] = struct{}{}
	}

	peers := make(map[cluster.NodeID]*shardstore.Peer, len(nodes))

	for node := range nodes {
		shard, err := shardstore.OpenShard(t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { _ = shard.Close() })

		shard.Adopt(m.RangesFor(node))

		srv := httptest.NewServer(transport.NewServer(
			transport.NewMemStore(), peerSecret, transport.WithShard(shardstore.Serve(shard)),
		))
		t.Cleanup(srv.Close)

		client, err := transport.NewClient(srv.URL, peerSecret, node, srv.Client())
		require.NoError(t, err)

		peers[node] = shardstore.NewPeer(client)
	}

	router := shardstore.NewRouter(func(context.Context) (*rangemap.Map, error) { return m, nil })

	resolve := func(_ context.Context, node cluster.NodeID) (shardstore.Backend, error) {
		p, ok := peers[node]
		if !ok {
			return nil, assert.AnError
		}

		return p, nil
	}

	return shardstore.NewStore(router, resolve, &shardstore.MemoryReadiness{})
}

// presplitMap is the ordinary partitioning: boundaries at a bucket's first
// letter, spread across nodes.
func presplitMap(t testing.TB, ranges int, nodes ...cluster.NodeID) *rangemap.Map {
	t.Helper()

	m, err := rangemap.Initial(ranges, planeNodes(nodes...), 1)
	require.NoError(t, err)

	m.Revision = 1

	return m
}

// TestConformanceOverTheWire is the acceptance signal for the transport: the
// whole suite, with every shard on the far side of an HTTP round trip.
func TestConformanceOverTheWire(t *testing.T) {
	metastoretest.Run(t, func(tb testing.TB) metastore.Store {
		return remotePlane(tb, presplitMap(tb, 12, "n0", "n1", "n2"))
	})
}

// TestConformanceOverTheWireSplitBucket is the same, with a range boundary
// cutting the suite's test bucket in half — so every listing case pages across
// two shards over the wire, and the cursor crosses a boundary mid-page.
//
// This is the combination that exercises the most: fan-out ordering, partial
// counter summing, and the wire, all at once. A serialization bug that only
// showed up on a page assembled from two peers fails here and nowhere else.
func TestConformanceOverTheWireSplitBucket(t *testing.T) {
	metastoretest.Run(t, func(tb testing.TB) metastore.Store {
		const inside = "ophotos\x00m"

		return remotePlane(tb, &rangemap.Map{
			Revision: 1,
			Ranges: []rangemap.Range{
				{Start: "", End: inside, Owner: "n0"},
				{Start: inside, End: "", Owner: "n1"},
			},
		})
	})
}

// TestPeerCarriesOwnershipRefusal: a remote refusal must be indistinguishable
// from a local one, or the Store's retry — which turns a stale route into one
// wasted round trip instead of a wrong answer — works only for local shards.
//
// It travels as a field rather than an HTTP status because it is an answer, not
// a failure; folding it into a 4xx would make it indistinguishable from a
// request the peer could not parse.
func TestPeerCarriesOwnershipRefusal(t *testing.T) {
	shard, err := shardstore.OpenShard(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = shard.Close() })

	// Serves buckets sorting before "m" only.
	shard.Adopt([]rangemap.Range{{Start: "", End: "om", Owner: "n0"}})

	srv := httptest.NewServer(transport.NewServer(
		transport.NewMemStore(), peerSecret, transport.WithShard(shardstore.Serve(shard)),
	))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, peerSecret, "n0", srv.Client())
	require.NoError(t, err)

	peer := shardstore.NewPeer(client)

	require.NoError(t, peer.Put(t.Context(), metastoretest.Entry("apples", "k", 1, 1)))

	err = peer.Put(t.Context(), metastoretest.Entry("zebras", "k", 1, 1))
	require.ErrorIs(t, err, shardstore.ErrNotOwned, "the sentinel survives the wire")

	_, _, err = peer.Get(t.Context(), "zebras", "k")
	require.ErrorIs(t, err, shardstore.ErrNotOwned)

	err = peer.ScanRange(t.Context(),
		rangemap.Range{Start: "om", End: "", Owner: "n1"}, "zebras", "", "", 0,
		func(metastore.Entry) error { return nil })
	require.ErrorIs(t, err, shardstore.ErrNotOwned, "a range the peer does not serve is refused")
}

// TestPeerWithoutAShardIsUnsupported: a node running an older binary, or wired
// without a shard, must report so rather than erroring — a partially upgraded
// cluster should degrade, not break.
func TestPeerWithoutAShardIsUnsupported(t *testing.T) {
	srv := httptest.NewServer(transport.NewServer(transport.NewMemStore(), peerSecret))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, peerSecret, "n0", srv.Client())
	require.NoError(t, err)

	_, err = client.Shard(t.Context(), transport.ShardRequest{Op: transport.ShardOpCoverage})
	require.ErrorIs(t, err, transport.ErrUnsupported)
}

// TestPeerRejectsAnUnknownOperation: a newer caller asking an older peer for
// something it does not implement must get a clear error rather than a silently
// empty answer, which would read as "no results".
func TestPeerRejectsAnUnknownOperation(t *testing.T) {
	shard, err := shardstore.OpenShard(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = shard.Close() })

	srv := httptest.NewServer(transport.NewServer(
		transport.NewMemStore(), peerSecret, transport.WithShard(shardstore.Serve(shard)),
	))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, peerSecret, "n0", srv.Client())
	require.NoError(t, err)

	_, err = client.Shard(t.Context(), transport.ShardRequest{Op: "invent"})
	require.ErrorIs(t, err, transport.ErrUnsupported,
		"an operation the peer never heard of degrades like a peer without a shard")
}
