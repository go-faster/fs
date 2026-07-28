package main

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/metastore/memstore"
)

// countingStore counts the rebuilds that actually started.
//
// Reset is the signal, and it is exact: a fresh rebuild empties the store
// first, and a resumed one deliberately does not. Counting it needs no hook in
// production code — which is the point, because a test-only observer on the
// coordinator would be production surface existing only to be watched.
type countingStore struct {
	metastore.Store

	mu     sync.Mutex
	resets int
}

func (s *countingStore) Reset(ctx context.Context) error {
	s.mu.Lock()
	s.resets++
	s.mu.Unlock()

	return s.Store.Reset(ctx)
}

func (s *countingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.resets
}

// clusterScoped points a runtime's coordinator and runner at a cluster-scope
// store.
//
// memstore is not a deployment option — it loses everything on restart — but it
// is the only cluster-scope implementation until the sharded plane lands, and
// what is under test here is the runner, not the store. Every node in a test
// cluster shares one, which is exactly the shape a real cluster-scope store
// has: one store, many nodes, one of them elected to rebuild it.
func clusterScoped(t *testing.T, nodes []*clusterRuntime, store metastore.Store) {
	t.Helper()

	for _, rt := range nodes {
		rt.index = store
	}
}

// TestMetaRebuildElectsOneRunner is the property the second readiness check
// exists for, and the reason it is not redundant.
//
// Campaigning blocks, and what a node waits behind is another node doing this
// exact job. Without re-asking after winning, every node rebuilds the whole
// cluster in turn — each one emptying what the last one built — and a cluster
// would spend N sequential walks getting to where the first one already was.
func TestMetaRebuildElectsOneRunner(t *testing.T) {
	nodes := indexCluster(t, "/fs-metarebuild-one")
	store := &countingStore{Store: memstore.New()}
	clusterScoped(t, nodes, store)

	require.NoError(t, nodes[0].Storage.CreateBucket(t.Context(), indexBucket))
	putObjects(t, nodes[0], 32, "a.txt", "b.txt", "c.txt")

	var grp errgroup.Group

	for _, rt := range nodes {
		grp.Go(func() error { return rt.RunMetaRebuild(t.Context()) })
	}

	require.NoError(t, grp.Wait())

	assert.Equal(t, 1, store.count(),
		"exactly one node walks the cluster; the others find the work already done")

	state, err := store.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateReady, state)
}

// TestMetaRebuildFillsTheStore: the walk reads the sidecars on every node's
// disks, so the store ends up describing objects no single node holds — which
// is the whole reason a cluster-scope rebuild cannot be done per node.
func TestMetaRebuildFillsTheStore(t *testing.T) {
	nodes := indexCluster(t, "/fs-metarebuild-fill")
	store := memstore.New()
	clusterScoped(t, nodes, store)

	want := []string{"a.txt", "b.txt", "docs/one.txt"}

	require.NoError(t, nodes[0].Storage.CreateBucket(t.Context(), indexBucket))
	putObjects(t, nodes[0], 64, want...)

	require.NoError(t, nodes[0].RunMetaRebuild(t.Context()))

	var got []string

	require.NoError(t, store.Scan(t.Context(), indexBucket, "", "", 0, func(e metastore.Entry) error {
		got = append(got, e.Key)

		return nil
	}))

	assert.Equal(t, want, got)
}

// TestMetaRebuildSkippedAtLocalScope: a node-local store rebuilds itself from
// its own disks and needs no election. Running the cluster-wide walk for it
// would campaign, win, and walk every node to rebuild an index that describes
// one — wrong, and expensive in proportion to the cluster.
func TestMetaRebuildSkippedAtLocalScope(t *testing.T) {
	nodes := indexCluster(t, "/fs-metarebuild-local")

	// The node keeps its own objindex, which reports ScopeLocal. Nothing to
	// observe: the runner must return before touching any store.
	require.NoError(t, nodes[0].RunMetaRebuild(t.Context()))
}

// TestMetaRebuildNothingToDoWhenReady: a store that is ready with no cursor
// wants nothing, so a node restarting into a healthy cluster must not walk it.
func TestMetaRebuildNothingToDoWhenReady(t *testing.T) {
	nodes := indexCluster(t, "/fs-metarebuild-ready")
	store := &countingStore{Store: memstore.New()}
	clusterScoped(t, nodes, store)

	require.NoError(t, nodes[0].Storage.CreateBucket(t.Context(), indexBucket))
	putObjects(t, nodes[0], 32, "a.txt")
	require.NoError(t, store.MarkReady(t.Context()))

	require.NoError(t, nodes[0].RunMetaRebuild(t.Context()))
	assert.Zero(t, store.count(), "a ready store with no cursor wants nothing")
}
