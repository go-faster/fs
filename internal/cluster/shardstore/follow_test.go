package shardstore_test

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// replicated is a range with one follower.
var replicated = rangemap.Range{Start: "", End: "", Owner: "n0", Followers: []cluster.NodeID{"n1"}}

// ordered ships batches to a follower one at a time, which is the ordering the
// Shipper contract requires — pebble applies a batch as recorded, so a
// reordered pair leaves a follower with an older value where a newer one
// belongs.
func ordered(t *testing.T, apply func(context.Context, rangemap.Range, []byte) error) shardstore.Shipper {
	t.Helper()

	var mu sync.Mutex

	return func(ctx context.Context, r rangemap.Range, repr []byte) {
		mu.Lock()
		defer mu.Unlock()

		require.NoError(t, apply(ctx, r, repr))
	}
}

// TestFollowerMirrorsTheOwner is the property replication exists for: the
// follower's state is the owner's state, not a reconstruction of it — entry and
// counter applied together, exactly as the owner committed them.
//
// That is what makes a promotion cheap. There is nothing to reconcile, only a
// range to start serving.
func TestFollowerMirrorsTheOwner(t *testing.T) {
	follower := openShard(t)
	follower.Follow([]rangemap.Range{replicated})

	owner, err := shardstore.OpenShard(t.TempDir(),
		shardstore.WithShipper(ordered(t, follower.ApplyBatch)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })

	owner.Adopt([]rangemap.Range{replicated})

	require.NoError(t, owner.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))
	require.NoError(t, owner.Put(t.Context(), entry("photos", "b.jpg", 250, 1)))
	require.NoError(t, owner.Delete(t.Context(), "photos", "a.jpg"))

	// The follower does not serve, so it is inspected by promoting it.
	follower.Adopt([]rangemap.Range{replicated})

	assert.Equal(t, []string{"b.jpg"}, scanKeys(t, follower, "photos"))

	usage, err := follower.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Equal(t, metastore.Usage{Objects: 1, Bytes: 250}, usage,
		"the counters arrived with the entries, in the same batch")
}

// TestFollowerDoesNotServe is the distinction between Adopt and Follow, and it
// is load-bearing. A follower answering reads would be serving data it has no
// way to know is current — the owner is the only node that knows what it has
// applied.
func TestFollowerDoesNotServe(t *testing.T) {
	follower := openShard(t)
	follower.Follow([]rangemap.Range{replicated})

	owner, err := shardstore.OpenShard(t.TempDir(),
		shardstore.WithShipper(ordered(t, follower.ApplyBatch)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })

	owner.Adopt([]rangemap.Range{replicated})
	require.NoError(t, owner.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))

	_, _, err = follower.Get(t.Context(), "photos", "a.jpg")
	require.ErrorIs(t, err, shardstore.ErrNotOwned, "following is not serving")

	require.ErrorIs(t, follower.Put(t.Context(), entry("photos", "c.jpg", 1, 1)),
		shardstore.ErrNotOwned, "and a follower takes no writes of its own")
}

// TestApplyRefusesARangeItDoesNotFollow: a batch for a range this node does not
// follow means the *sender's* follower set is stale. Applying it would leave
// data nothing will ever ask this shard for and nothing will clean up.
func TestApplyRefusesARangeItDoesNotFollow(t *testing.T) {
	follower := openShard(t)
	follower.Follow([]rangemap.Range{{Start: "", End: "om", Owner: "n0"}})

	err := follower.ApplyBatch(t.Context(),
		rangemap.Range{Start: "om", End: "", Owner: "n0"}, []byte("ignored"))
	require.ErrorIs(t, err, shardstore.ErrNotFollowed)
}

// TestUnreplicatedRangeShipsNothing: R=1 is legal, and it means a lost owner
// costs a rebuild rather than a promotion. A shard with no followers must not
// call the shipper at all.
func TestUnreplicatedRangeShipsNothing(t *testing.T) {
	shipped := 0

	owner, err := shardstore.OpenShard(t.TempDir(),
		shardstore.WithShipper(func(context.Context, rangemap.Range, []byte) { shipped++ }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })

	owner.Adopt([]rangemap.Range{whole}) // no followers
	require.NoError(t, owner.Put(t.Context(), entry("photos", "a.jpg", 1, 1)))

	assert.Zero(t, shipped)
}

// TestFollowerOverTheWire: the same mirroring, with the batch crossing a real
// HTTP round trip — which is what a follower on another node actually is.
func TestFollowerOverTheWire(t *testing.T) {
	follower := openShard(t)
	follower.Follow([]rangemap.Range{replicated})

	srv := httptest.NewServer(transport.NewServer(
		transport.NewMemStore(), peerSecret, transport.WithShard(shardstore.Serve(follower)),
	))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, peerSecret, "n1", srv.Client())
	require.NoError(t, err)

	peer := shardstore.NewPeer(client)

	owner, err := shardstore.OpenShard(t.TempDir(),
		shardstore.WithShipper(ordered(t, peer.ApplyBatch)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })

	owner.Adopt([]rangemap.Range{replicated})

	for _, key := range []string{"a", "b", "c"} {
		require.NoError(t, owner.Put(t.Context(), entry("photos", key, 10, 1)))
	}

	follower.Adopt([]rangemap.Range{replicated})
	assert.Equal(t, []string{"a", "b", "c"}, scanKeys(t, follower, "photos"))
}

// TestApplyRefusalSurvivesTheWire: a stale follower set must be reported as
// such rather than as a generic failure, or an owner cannot tell "you are not
// my follower" from "your disk is broken".
func TestApplyRefusalSurvivesTheWire(t *testing.T) {
	follower := openShard(t) // follows nothing

	srv := httptest.NewServer(transport.NewServer(
		transport.NewMemStore(), peerSecret, transport.WithShard(shardstore.Serve(follower)),
	))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, peerSecret, "n1", srv.Client())
	require.NoError(t, err)

	err = shardstore.NewPeer(client).ApplyBatch(t.Context(), replicated, []byte("ignored"))
	require.ErrorIs(t, err, shardstore.ErrNotFollowed)
}

// TestShippingHappensAfterTheCommit: a follower must never be told about a
// write the owner has not made. If it were, a failover could promote a replica
// holding something the disks never had — which is the one way this plane could
// invent data rather than merely lag it.
func TestShippingHappensAfterTheCommit(t *testing.T) {
	var (
		shard   *shardstore.Shard
		visible bool
	)

	s, err := shardstore.OpenShard(t.TempDir(),
		shardstore.WithShipper(func(ctx context.Context, _ rangemap.Range, repr []byte) {
			require.NotEmpty(t, repr, "the batch is what the owner actually applied")

			_, found, err := shard.Get(ctx, "photos", "a.jpg")
			require.NoError(t, err)

			visible = found
		}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	shard = s
	shard.Adopt([]rangemap.Range{replicated})

	require.NoError(t, shard.Put(t.Context(), entry("photos", "a.jpg", 1, 1)))

	assert.True(t, visible, "the owner already holds the write by the time it ships")
}

// TestLearnerReceivesTheLog: a learner is being backfilled, and the log is what
// keeps it current for everything written meanwhile.
//
// Without this a backfill copies a moving target and receives none of the
// changes, finishing with the range as it was when the copy started — which is
// a replica that looks complete and is not. A range whose only recipient is a
// learner still ships, which is exactly the state a move begins in.
func TestLearnerReceivesTheLog(t *testing.T) {
	learning := rangemap.Range{
		Start: "", End: "", Owner: "n0", Learners: []cluster.NodeID{"n1"},
	}

	learner := openShard(t)
	learner.Configure(nil, nil, []rangemap.Range{learning})

	owner, err := shardstore.OpenShard(t.TempDir(),
		shardstore.WithShipper(ordered(t, learner.ApplyBatch)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })

	owner.Adopt([]rangemap.Range{learning})

	require.NoError(t, owner.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))
	require.NoError(t, owner.Put(t.Context(), entry("photos", "b.jpg", 250, 1)))

	// Inspected by adopting, since a learner serves nothing — the same as a
	// follower, and for the same reason.
	learner.Adopt([]rangemap.Range{learning})
	assert.Equal(t, []string{"a.jpg", "b.jpg"}, scanKeys(t, learner, "photos"))
}

// TestLearnRefusesARangeItMerelyFollows is the distinction the shard used to be
// unable to make.
//
// A follower is kept current by the log and is the destination of no move, so
// backfilled entries arriving for one come from a sender working from a map
// where this node is something it is not. Storing them would leave data on a
// node that will never be asked to hold it — and, worse, would let a follower
// accumulate a half-copy that nothing distinguishes from a real one.
func TestLearnRefusesARangeItMerelyFollows(t *testing.T) {
	follower := openShard(t)
	follower.Follow([]rangemap.Range{replicated})

	err := follower.Learn(t.Context(), replicated,
		[]metastore.Entry{entry("photos", "a.jpg", 100, 1)})
	require.ErrorIs(t, err, shardstore.ErrNotLearned)

	// And the log still reaches it, so this is the narrower check rather than a
	// follower that stopped replicating.
	owner, err := shardstore.OpenShard(t.TempDir(),
		shardstore.WithShipper(ordered(t, follower.ApplyBatch)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })

	owner.Adopt([]rangemap.Range{replicated})
	require.NoError(t, owner.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))

	follower.Adopt([]rangemap.Range{replicated})
	assert.Equal(t, []string{"a.jpg"}, scanKeys(t, follower, "photos"))
}

// TestFollowingCountsLearnersToo: replicating is what a follower and a learner
// have in common — each holds a range it does not serve — and the node's own
// accounting of what it is holding for others must include both.
func TestFollowingCountsLearnersToo(t *testing.T) {
	shard := openShard(t)

	left := rangemap.Range{Start: "", End: "om", Owner: "n0", Followers: []cluster.NodeID{"n1"}}
	right := rangemap.Range{Start: "om", End: "", Owner: "n0", Learners: []cluster.NodeID{"n1"}}

	shard.Configure(nil, []rangemap.Range{left}, []rangemap.Range{right})

	assert.Len(t, shard.Following(), 2)
}
