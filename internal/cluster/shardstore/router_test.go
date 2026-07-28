package shardstore_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
)

// mapAt builds a three-range map at a given revision.
func mapAt(revision int64, owners ...cluster.NodeID) *rangemap.Map {
	for len(owners) < 3 {
		owners = append(owners, "n0")
	}

	return &rangemap.Map{
		Revision: revision,
		Ranges: []rangemap.Range{
			{Start: "", End: "om", Owner: owners[0]},
			{Start: "om", End: "ot", Owner: owners[1]},
			{Start: "ot", End: "", Owner: owners[2]},
		},
	}
}

// source is a controllable range-map loader that counts reads.
type source struct {
	mu     sync.Mutex
	m      *rangemap.Map
	loads  atomic.Int64
	block  chan struct{}
	failed error
}

func (s *source) load(context.Context) (*rangemap.Map, error) {
	s.loads.Add(1)

	if s.block != nil {
		<-s.block
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failed != nil {
		return nil, s.failed
	}

	return s.m, nil
}

func (s *source) set(m *rangemap.Map) {
	s.mu.Lock()
	s.m = m
	s.mu.Unlock()
}

// TestRouteLoadsLazily: nothing is watched and nothing is fetched until a key
// actually needs an answer. That is the whole point — a watch on the range map
// from every node is the etcd fan-out this design exists to avoid.
func TestRouteLoadsLazily(t *testing.T) {
	src := &source{m: mapAt(7)}
	r := shardstore.NewRouter(src.load)

	assert.Zero(t, src.loads.Load(), "constructing a router must not touch etcd")
	assert.Nil(t, r.Map())

	target, err := r.Route(t.Context(), "oa")
	require.NoError(t, err)
	assert.EqualValues(t, "n0", target.Range.Owner)
	assert.EqualValues(t, 7, target.Revision, "the revision travels with the route")
	assert.EqualValues(t, 1, src.loads.Load())
}

// TestRouteServesFromCache: steady state costs nothing. A router that refetched
// per request would put the whole cluster's read rate onto etcd.
func TestRouteServesFromCache(t *testing.T) {
	src := &source{m: mapAt(7)}
	r := shardstore.NewRouter(src.load)

	for range 50 {
		_, err := r.Route(t.Context(), "oa")
		require.NoError(t, err)
	}

	assert.EqualValues(t, 1, src.loads.Load(), "one load, however many routes")
}

// TestInvalidateRefetchesOnce: a peer reporting a newer revision is the only
// thing that makes the cache stale, and it should cost exactly one read.
func TestInvalidateRefetchesOnce(t *testing.T) {
	src := &source{m: mapAt(7)}
	r := shardstore.NewRouter(src.load)

	_, err := r.Route(t.Context(), "oa")
	require.NoError(t, err)

	src.set(mapAt(9, "n5"))
	r.Invalidate(9)

	for range 10 {
		target, err := r.Route(t.Context(), "oa")
		require.NoError(t, err)
		assert.EqualValues(t, "n5", target.Range.Owner)
	}

	assert.EqualValues(t, 2, src.loads.Load(), "one refetch, then cached again")
}

// TestInvalidateIsForwardOnly: a peer reporting an *older* revision is itself
// stale, and must not cost a refetch.
func TestInvalidateIsForwardOnly(t *testing.T) {
	src := &source{m: mapAt(7)}
	r := shardstore.NewRouter(src.load)

	_, err := r.Route(t.Context(), "oa")
	require.NoError(t, err)

	for range 10 {
		r.Invalidate(3)

		_, err := r.Route(t.Context(), "oa")
		require.NoError(t, err)
	}

	assert.EqualValues(t, 1, src.loads.Load(), "an older revision is not news")
}

// TestStaleReportDoesNotCancelAPendingRefresh is what the forward-only rule
// actually protects, and it is not the obvious case.
//
// While the cache is behind a revision a peer has reported — because the load
// has not caught up yet — a *second*, staler peer must not lower the bar. If it
// could, the node would conclude it is current, stop trying to reach the
// revision it was told about, and keep routing by a map it already knows is
// stale.
func TestStaleReportDoesNotCancelAPendingRefresh(t *testing.T) {
	// The loader keeps answering with the old map, as an etcd read that has
	// not yet observed the change would.
	src := &source{m: mapAt(7)}
	r := shardstore.NewRouter(src.load)

	_, err := r.Route(t.Context(), "oa")
	require.NoError(t, err)
	require.EqualValues(t, 1, src.loads.Load())

	// A peer is at 11; the cache cannot reach it yet.
	r.Invalidate(11)

	_, err = r.Route(t.Context(), "oa")
	require.NoError(t, err)
	require.EqualValues(t, 2, src.loads.Load(), "still behind, so it tried")

	// A staler peer chimes in. This must not be read as "you are current now".
	r.Invalidate(5)

	_, err = r.Route(t.Context(), "oa")
	require.NoError(t, err)

	assert.EqualValues(t, 3, src.loads.Load(),
		"the node must still be chasing revision 11, not settle for 5")
}

// TestConcurrentRefreshIsSingleFlighted is the property that matters under
// load: a map change must not turn into one etcd read per in-flight request.
// That would convert a single control-plane change into a burst proportional
// to traffic, aimed at the component least able to absorb it.
func TestConcurrentRefreshIsSingleFlighted(t *testing.T) {
	src := &source{m: mapAt(7), block: make(chan struct{})}
	r := shardstore.NewRouter(src.load)

	var grp errgroup.Group

	for range 64 {
		grp.Go(func() error {
			_, err := r.Route(t.Context(), "oa")

			return err
		})
	}

	// Let them all pile up on the loader before releasing it.
	close(src.block)
	require.NoError(t, grp.Wait())

	assert.LessOrEqual(t, src.loads.Load(), int64(2),
		"64 concurrent routes must collapse to one load, not 64")
}

// TestDoRetriesOnceOnWrongRange: the self-correcting half. A route from a stale
// map is refused rather than served, and the caller refetches and retries.
func TestDoRetriesOnceOnWrongRange(t *testing.T) {
	src := &source{m: mapAt(7)}
	r := shardstore.NewRouter(src.load)

	attempts := 0

	err := r.Do(t.Context(), "oa", func(target shardstore.Target) error {
		attempts++

		if attempts == 1 {
			// The owner moved; the responder is at a newer revision.
			src.set(mapAt(11, "n5"))

			return &shardstore.WrongRange{Revision: 11, Key: "oa"}
		}

		assert.EqualValues(t, "n5", target.Range.Owner, "the retry uses the fresh map")
		assert.EqualValues(t, 11, target.Revision)

		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 2, attempts)
}

// TestDoDoesNotRetryForever: an unbounded retry against a map that keeps
// changing is a livelock. A second refusal after a fresh map means the caller
// and the responder genuinely disagree, which is worth surfacing.
func TestDoDoesNotRetryForever(t *testing.T) {
	src := &source{m: mapAt(7)}
	r := shardstore.NewRouter(src.load)

	attempts := 0

	err := r.Do(t.Context(), "oa", func(shardstore.Target) error {
		attempts++
		src.set(mapAt(int64(100+attempts), "n5"))

		return &shardstore.WrongRange{Revision: int64(100 + attempts), Key: "oa"}
	})

	var wrong *shardstore.WrongRange
	require.ErrorAs(t, err, &wrong, "the second refusal is returned, not swallowed")
	assert.Equal(t, 2, attempts)
}

// TestDoPassesThroughOtherErrors: only a stale route is worth retrying. A
// genuine failure retried is a failure done twice.
func TestDoPassesThroughOtherErrors(t *testing.T) {
	src := &source{m: mapAt(7)}
	r := shardstore.NewRouter(src.load)

	boom := errors.New("boom")
	attempts := 0

	err := r.Do(t.Context(), "oa", func(shardstore.Target) error {
		attempts++

		return boom
	})

	require.ErrorIs(t, err, boom)
	assert.Equal(t, 1, attempts)
}

// TestVerifyIsTheReceivingHalf: a node asked for a key it does not own refuses
// and says which revision it refused by, so the caller can tell a stale cache
// from a real disagreement.
func TestVerifyIsTheReceivingHalf(t *testing.T) {
	src := &source{m: mapAt(7, "n0", "n1", "n2")}
	r := shardstore.NewRouter(src.load)

	require.NoError(t, r.Verify(t.Context(), "oa", "n0"))

	err := r.Verify(t.Context(), "oa", "n1")

	var wrong *shardstore.WrongRange
	require.ErrorAs(t, err, &wrong)
	assert.EqualValues(t, 7, wrong.Revision, "the responder's revision is what the caller acts on")
	assert.Equal(t, "oa", wrong.Key)
}

// TestRouteReportsAnUninitializedPlane: an empty map is not "this key is
// unowned" — a valid map covers everything. It is a cluster that has never been
// partitioned, and that is worth a different message from a routing failure.
func TestRouteReportsAnUninitializedPlane(t *testing.T) {
	src := &source{m: &rangemap.Map{}}
	r := shardstore.NewRouter(src.load)

	_, err := r.Route(t.Context(), "oa")
	require.ErrorContains(t, err, "not initialized")
}

// TestRouteSurfacesLoaderFailure: an unreachable control plane must not read as
// an empty map, which would route every key to nowhere.
func TestRouteSurfacesLoaderFailure(t *testing.T) {
	src := &source{failed: errors.New("etcd is down")}
	r := shardstore.NewRouter(src.load)

	_, err := r.Route(t.Context(), "oa")
	require.ErrorContains(t, err, "etcd is down")
}

// TestRefreshNeverGoesBackwards: two flights can overlap around a change, and
// adopting the older result would undo the newer one — leaving the node routing
// by a map it has already been told is stale.
func TestRefreshNeverGoesBackwards(t *testing.T) {
	src := &source{m: mapAt(20, "n5")}
	r := shardstore.NewRouter(src.load)

	_, err := r.Route(t.Context(), "oa")
	require.NoError(t, err)

	// A late load returning an older map.
	src.set(mapAt(9, "n0"))
	r.Invalidate(21)

	_, err = r.Route(t.Context(), "oa")
	require.NoError(t, err)

	assert.EqualValues(t, 20, r.Map().Revision, "the newer map stands")
	assert.EqualValues(t, "n5", r.Map().Ranges[0].Owner)
}
