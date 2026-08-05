package shardstore_test

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// control is the stand-in for etcd: one map, shared by every node, read through
// each node's own loader.
//
// Shared rather than copied per node, because the whole question these tests
// ask is what happens while nodes disagree about it — and a disagreement is
// only real if they are all reading the same thing at different times.
type control struct {
	mu sync.Mutex
	m  *rangemap.Map

	// loads counts reads, so a test can tell "the router refreshed" from "the
	// router served a cached answer".
	loads int
	// fail, when set, is what a load returns instead of the map.
	fail error
}

func (c *control) load(context.Context) (*rangemap.Map, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.loads++

	if c.fail != nil {
		return nil, c.fail
	}

	return c.m, nil
}

func (c *control) save(_ context.Context, m *rangemap.Map) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.m = m

	return nil
}

// publish installs a new partitioning at the next revision.
func (c *control) publish(t *testing.T, ranges ...rangemap.Range) {
	t.Helper()

	c.mu.Lock()
	next := &rangemap.Map{Ranges: ranges}

	if c.m != nil {
		next.Revision = c.m.Revision
	}

	next.Revision++
	c.m = next
	c.mu.Unlock()

	require.NoError(t, next.Validate())
}

func (c *control) loadCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.loads
}

// node is one member of a test cluster: its shard, its plane, and the HTTP
// server other nodes reach it through.
type node struct {
	id    cluster.NodeID
	shard *shardstore.Shard
	plane *shardstore.Plane

	// down, once set, makes this node unreachable — dialing it fails the way a
	// dead node's transport does. The shard stays open so a test can inspect
	// what it held when it went away.
	down bool
}

// testCluster is N nodes wired to one control plane, reaching each other over
// real HTTP.
type testCluster struct {
	ctl   *control
	nodes map[cluster.NodeID]*node
	ready *shardstore.MemoryReadiness
}

// newCluster builds n nodes named n0..n{n-1}, each serving its shard over the
// peer transport and routing through its own plane.
//
// Over real HTTP rather than by handing shards to each other directly: shipping
// a batch and refusing a wrong owner both have to survive serialization, and a
// test that skipped it would be exercising the half of the path that cannot
// fail.
func newCluster(t *testing.T, ids ...cluster.NodeID) *testCluster {
	t.Helper()

	c := &testCluster{
		ctl:   &control{},
		nodes: make(map[cluster.NodeID]*node, len(ids)),
		ready: &shardstore.MemoryReadiness{},
	}

	require.NoError(t, c.ready.Set(t.Context(), metastore.StateReady))

	// Two passes: every node's dialer must be able to reach every other, so no
	// node can be finished until all of them exist.
	peers := make(map[cluster.NodeID]*shardstore.Peer, len(ids))

	for _, id := range ids {
		shard, err := shardstore.OpenShard(t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { _ = shard.Close() })

		c.nodes[id] = &node{id: id, shard: shard}
	}

	for _, id := range ids {
		srv := httptest.NewServer(transport.NewServer(
			transport.NewMemStore(), peerSecret,
			transport.WithShard(shardstore.Serve(c.nodes[id].shard)),
		))
		t.Cleanup(srv.Close)

		client, err := transport.NewClient(srv.URL, peerSecret, id, srv.Client())
		require.NoError(t, err)

		peers[id] = shardstore.NewPeer(client)
	}

	for _, id := range ids {
		n := c.nodes[id]
		n.plane = shardstore.NewPlane(id, n.shard, c.ctl.load, c.dialer(peers), c.ready)
	}

	return c
}

// dialer refuses nodes marked down, which is how a test kills one.
func (c *testCluster) dialer(peers map[cluster.NodeID]*shardstore.Peer) shardstore.Dialer {
	return func(_ context.Context, id cluster.NodeID) (shardstore.Backend, error) {
		n, ok := c.nodes[id]
		if !ok {
			return nil, errors.Errorf("no such node %s", id)
		}

		if n.down {
			return nil, errors.Errorf("node %s is unreachable", id)
		}

		return peers[id], nil
	}
}

// refreshAll makes every live node pick up the current map.
func (c *testCluster) refreshAll(t *testing.T) {
	t.Helper()

	for _, n := range c.nodes {
		if n.down {
			continue
		}

		_, err := n.plane.Refresh(t.Context())
		require.NoError(t, err)
	}
}

// store is the cluster-scope metastore.Store as seen from one node. Every node
// serves the same view, which is the point of the plane.
func (c *testCluster) store(id cluster.NodeID) metastore.Store { return c.nodes[id].plane.Store() }

// TestPlaneServesTheWholeKeyspaceFromEveryNode is the composition working: a
// key written through one node is readable through any of them, because every
// node routes to the same owner rather than answering from what it happens to
// hold.
func TestPlaneServesTheWholeKeyspaceFromEveryNode(t *testing.T) {
	c := newCluster(t, "n0", "n1", "n2")
	c.ctl.publish(t,
		rangemap.Range{Start: "", End: "ob", Owner: "n0"},
		rangemap.Range{Start: "ob", End: "oc", Owner: "n1"},
		rangemap.Range{Start: "oc", End: "", Owner: "n2"},
	)
	c.refreshAll(t)

	// Bucket names chosen so the encoded keys land in different ranges: the
	// object prefix is 'o', so "a"/"b"/"c" fall either side of "ob" and "oc".
	for _, bucket := range []string{"a", "b", "c"} {
		require.NoError(t, c.store("n0").Put(t.Context(), entry(bucket, "x.jpg", 10, 1)))
	}

	// The premise first: without it the test would pass just as well with every
	// key on one node, which is the routing not happening rather than working.
	for id, bucket := range map[cluster.NodeID]string{"n0": "a", "n1": "b", "n2": "c"} {
		assert.Equal(t, []string{bucket}, shardBuckets(t, c.nodes[id].shard),
			"node %s should hold exactly the bucket its range covers", id)
	}

	for _, id := range []cluster.NodeID{"n0", "n1", "n2"} {
		for _, bucket := range []string{"a", "b", "c"} {
			_, found, err := c.store(id).Get(t.Context(), bucket, "x.jpg")
			require.NoError(t, err)
			assert.True(t, found, "node %s cannot see %s/x.jpg", id, bucket)
		}
	}
}

// planeNodes turns bare node IDs into topology members with no rack, which is
// what most of these tests want: every node its own failure domain.
func planeNodes(ids ...cluster.NodeID) []cluster.Node {
	out := make([]cluster.Node, 0, len(ids))
	for _, id := range ids {
		out = append(out, cluster.Node{ID: id})
	}

	return out
}

// shardBuckets is what one shard holds, ignoring routing.
func shardBuckets(t *testing.T, s *shardstore.Shard) []string {
	t.Helper()

	var names []string

	require.NoError(t, s.Buckets(t.Context(), func(bucket string) error {
		names = append(names, bucket)

		return nil
	}))

	return names
}

// TestOwningBeatsFollowing is the split-brain guard in the shard itself, where
// it is the only thing that can refuse.
//
// A map that named one node both owner and follower of a range is itself the
// mistake, and the shard resolves it the safe way round: it serves the range
// and takes no one else's writes for it. Deciding the other way would have a
// node replaying a former owner's state into keys it is currently answering
// for.
func TestOwningBeatsFollowing(t *testing.T) {
	s := openShard(t)
	s.Configure([]rangemap.Range{whole}, []rangemap.Range{whole}, nil)

	err := s.ApplyBatch(t.Context(), whole, []byte("from someone who thinks it owns this"))
	require.ErrorIs(t, err, shardstore.ErrNotFollowed)

	// And it is still serving: refusing the batch cost the range nothing.
	require.NoError(t, s.Put(t.Context(), entry("photos", "a.jpg", 1, 1)))
}

// TestRoutingReconfiguresTheShard is the reason the shard is configured from
// the router's load rather than by a caller applying the map by hand.
//
// A node whose shard served ranges the router no longer routed to it would
// answer for keys nobody asks it about, and keep answering, with nothing to
// notice. So adopting a map has to be one act, not two.
func TestRoutingReconfiguresTheShard(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t,
		rangemap.Range{Start: "", End: "ob", Owner: "n0"},
		rangemap.Range{Start: "ob", End: "", Owner: "n1"},
	)
	c.refreshAll(t)

	require.Equal(t, []rangemap.Range{{Start: "", End: "ob", Owner: "n0"}},
		c.nodes["n0"].shard.Ranges())

	// The whole keyspace moves to n1. Nobody tells n0's shard.
	c.ctl.publish(t, rangemap.Range{Start: "", End: "", Owner: "n1"})

	_, err := c.nodes["n0"].plane.Refresh(t.Context())
	require.NoError(t, err)

	assert.Empty(t, c.nodes["n0"].shard.Ranges(),
		"the shard stopped serving what the router stopped routing to it")
}

// TestFollowerIsConfiguredFromTheMap: replication targets come from the same
// map as routing. A node listed as a follower holds the range; it does not
// serve it.
func TestFollowerIsConfiguredFromTheMap(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t, rangemap.Range{
		Start: "", End: "", Owner: "n0", Followers: []cluster.NodeID{"n1"},
	})
	c.refreshAll(t)

	require.NoError(t, c.store("n0").Put(t.Context(), entry("photos", "a.jpg", 100, 1)))

	assert.Empty(t, c.nodes["n1"].shard.Ranges(), "a follower serves nothing")
	require.Len(t, c.nodes["n1"].shard.Following(), 1)

	_, found, err := c.nodes["n1"].shard.Get(t.Context(), "photos", "a.jpg")
	require.ErrorIs(t, err, shardstore.ErrNotOwned)
	assert.False(t, found)
}

// TestPromotionServesWithoutARebuild is the whole promote-or-rebuild split,
// end to end: owner writes, follower mirrors, owner dies, Reassign promotes,
// and the promoted node answers for keys it never took a write for.
//
// This is what replication buys. Without the follower the same failure costs a
// cluster-wide walk of every disk.
func TestPromotionServesWithoutARebuild(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t, rangemap.Range{
		Start: "", End: "", Owner: "n0", Followers: []cluster.NodeID{"n1"},
	})
	c.refreshAll(t)

	for _, key := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		require.NoError(t, c.store("n0").Put(t.Context(), entry("photos", key, 100, 1)))
	}

	require.NoError(t, c.store("n0").Delete(t.Context(), "photos", "b.jpg"))

	// n0 goes away.
	c.nodes["n0"].down = true

	current, err := c.ctl.load(t.Context())
	require.NoError(t, err)

	out, err := shardstore.Reassign(current, []cluster.NodeID{"n1"})
	require.NoError(t, err)
	require.Len(t, out.Promoted, 1, "a live follower is a promotion, not an orphan")
	require.Empty(t, out.Orphaned)

	c.ctl.publish(t, out.Map.Ranges...)

	_, err = c.nodes["n1"].plane.Refresh(t.Context())
	require.NoError(t, err)

	// n1 now answers for the whole keyspace, from what it replicated.
	var keys []string

	require.NoError(t, c.store("n1").Scan(t.Context(), "photos", "", "", 0,
		func(e metastore.Entry) error {
			keys = append(keys, e.Key)

			return nil
		}))

	assert.Equal(t, []string{"a.jpg", "c.jpg"}, keys,
		"the delete arrived with the puts, in the owner's own batches")

	usage, err := c.store("n1").Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 2, Bytes: 200}, usage)
}

// TestPromotionStopsFollowing: a promoted node must drop the range from its
// followed set, not merely add it to its owned one.
//
// The deposed owner is the node most likely to still be shipping — it went away
// with the map it had, and if it comes back before hearing otherwise it will
// keep replicating to what it believes is its follower. A promoted node that
// was still following would write a former owner's state into a range it is now
// answering for, which is the only way this plane could serve something no
// owner committed. (Owning also beats following inside the shard itself; see
// TestOwningBeatsFollowing.)
func TestPromotionStopsFollowing(t *testing.T) {
	c := newCluster(t, "n0", "n1")

	replicated := rangemap.Range{
		Start: "", End: "", Owner: "n0", Followers: []cluster.NodeID{"n1"},
	}

	c.ctl.publish(t, replicated)
	c.refreshAll(t)

	require.NoError(t, c.store("n0").Put(t.Context(), entry("photos", "a.jpg", 100, 1)))

	// n1 is promoted. n0 never hears about it.
	c.ctl.publish(t, rangemap.Range{Start: "", End: "", Owner: "n1"})

	_, err := c.nodes["n1"].plane.Refresh(t.Context())
	require.NoError(t, err)

	require.Empty(t, c.nodes["n1"].shard.Following(),
		"promotion is not an addition: what was followed is now owned")

	// n0, still on the old map, ships a batch for the range n1 now owns.
	batch := []byte("would be applied if the guard were not there")

	err = c.nodes["n1"].shard.ApplyBatch(t.Context(), replicated, batch)
	require.ErrorIs(t, err, shardstore.ErrNotFollowed,
		"a node that owns a range takes no one else's writes for it")
}

// TestOrphanedRangeIsTheExpensiveCase: with no live follower there is nothing
// to promote, so the range is assigned anyway — a key nothing serves is worse —
// and reported as orphaned, which is a caller's cue to mark the plane building
// and rebuild.
//
// A node serving an empty range answers "no such object" for objects that
// exist, and that is a wrong answer rather than a slow one.
func TestOrphanedRangeIsTheExpensiveCase(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t, rangemap.Range{Start: "", End: "", Owner: "n0"}) // R=1
	c.refreshAll(t)

	require.NoError(t, c.store("n0").Put(t.Context(), entry("photos", "a.jpg", 100, 1)))

	c.nodes["n0"].down = true

	current, err := c.ctl.load(t.Context())
	require.NoError(t, err)

	out, err := shardstore.Reassign(current, []cluster.NodeID{"n1"})
	require.NoError(t, err)
	require.Len(t, out.Orphaned, 1)
	require.Empty(t, out.Promoted)

	// The caller's obligation: building before the map, so listings fall back to
	// the sidecar walk instead of trusting an empty range.
	require.NoError(t, c.store("n1").MarkBuilding(t.Context()))
	c.ctl.publish(t, out.Map.Ranges...)

	_, err = c.nodes["n1"].plane.Refresh(t.Context())
	require.NoError(t, err)

	state, err := c.store("n1").State(t.Context())
	require.NoError(t, err)
	require.Equal(t, metastore.StateBuilding, state)

	// And the range really is empty — this is what the rebuild is for.
	_, found, err := c.store("n1").Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	assert.False(t, found, "promotion was not available; nothing was moved")
}

// TestSteadyStateDoesNotReadTheMap is the reason routing is lazy rather than
// watched. A watch on the partitioning from every node, notified on every
// split, is the etcd fan-out this plane is shaped to avoid — so a request that
// routes correctly must cost no map traffic at all.
func TestSteadyStateDoesNotReadTheMap(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t,
		rangemap.Range{Start: "", End: "ob", Owner: "n0"},
		rangemap.Range{Start: "ob", End: "", Owner: "n1"},
	)
	c.refreshAll(t)

	before := c.ctl.loadCount()

	for i := range 50 {
		require.NoError(t, c.store("n0").Put(t.Context(), entry("a", "k", int64(i), int64(i+1))))
		require.NoError(t, c.store("n0").Put(t.Context(), entry("z", "k", int64(i), int64(i+1))))
	}

	assert.Equal(t, before, c.ctl.loadCount(),
		"a correct route costs no control-plane read")
}

// TestInitializeLeavesALivePartitioningAlone: re-partitioning a running cluster
// would move every range at once. Bootstrap has to be able to run on every node
// at every start, so it has to be a no-op on all but the first.
func TestInitializeLeavesALivePartitioningAlone(t *testing.T) {
	ctl := &control{}
	nodes := []cluster.NodeID{"n0", "n1", "n2"}

	created, err := shardstore.Initialize(t.Context(), ctl.load, ctl.save, 6, planeNodes(nodes...), 1)
	require.NoError(t, err)
	require.True(t, created)

	first, err := ctl.load(t.Context())
	require.NoError(t, err)
	require.NoError(t, first.Validate())

	created, err = shardstore.Initialize(t.Context(), ctl.load, ctl.save, 6, planeNodes(nodes...), 1)
	require.NoError(t, err)
	assert.False(t, created)

	again, err := ctl.load(t.Context())
	require.NoError(t, err)
	assert.Equal(t, first, again, "the second start changed nothing")
}

// TestInitializeRefusesToGuessFromAFailedRead: a load failure is not "there is
// no map". Treating it as one would have a node that briefly could not reach
// etcd repartition a live cluster from scratch, which is the most expensive
// mistake available here.
func TestInitializeRefusesToGuessFromAFailedRead(t *testing.T) {
	ctl := &control{fail: errors.New("etcd is unreachable")}

	created, err := shardstore.Initialize(t.Context(), ctl.load, ctl.save, 6,
		planeNodes("n0"), 1)
	require.Error(t, err)
	assert.False(t, created)

	ctl.fail = nil

	m, err := ctl.load(t.Context())
	require.NoError(t, err)
	assert.Nil(t, m, "nothing was written")
}

// TestPlaneHoldsALearnerRange: a learner is configured from the map exactly as a
// follower is — it holds the range and serves nothing — because holding is what
// receiving the log means.
//
// What separates the two is promotion, which reads Followers and never this.
// A node that did not hold its learner ranges would receive the owner's batches
// and refuse every one of them, so a move could never make progress.
func TestPlaneHoldsALearnerRange(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t, rangemap.Range{
		Start: "", End: "", Owner: "n0", Learners: []cluster.NodeID{"n1"},
	})
	c.refreshAll(t)

	require.Len(t, c.nodes["n1"].shard.Following(), 1,
		"a learner holds the range it is being backfilled with")
	assert.Empty(t, c.nodes["n1"].shard.Ranges(), "and serves none of it")

	require.NoError(t, c.store("n0").Put(t.Context(), entry("photos", "a.jpg", 100, 1)))

	// The batch was accepted rather than refused, which is what lets a backfill
	// keep up with writes arriving while it runs.
	c.nodes["n1"].shard.Adopt(c.nodes["n1"].shard.Following())
	assert.Equal(t, []string{"a.jpg"}, scanKeys(t, c.nodes["n1"].shard, "photos"))
}
