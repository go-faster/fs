package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/metastore"
)

// planeNode boots one cluster node with the sharded metadata plane turned on.
func planeNode(t *testing.T, endpoint, prefix string, index int) *clusterRuntime {
	t.Helper()

	root := t.TempDir()

	cfg := validClusterConfig()
	cfg.Cluster.NodeID = "n" + strconv.Itoa(index)
	cfg.Cluster.Rack = "r" + strconv.Itoa(index)
	cfg.Cluster.Addr = testNodeAddr
	cfg.Cluster.AdvertiseAddr = testNodeAddr
	cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: prefix, TTL: testEtcdTTL}
	cfg.Cluster.Disks = []ClusterDiskConfig{{ID: "d0", Path: filepath.Join(root, "d0")}}
	cfg.Cluster.Metadata = MetadataConfig{Sharded: true, Ranges: 8, Replicas: 2}
	cfg.Storage.Fsync = "none"
	require.NoError(t, cfg.Validate())

	rt, err := buildCluster(t.Context(), zaptest.NewLogger(t), cfg, root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.close() })

	go func() { _ = rt.Serve(t.Context()) }()

	// Started explicitly, as the other elected loops are in these tests: in a
	// running node this is launched from the serve command alongside the usage
	// reporter and the coverage refresher.
	go rt.RunPlaneController(t.Context(), cfg)

	return rt
}

// TestShardedPlaneServesTheCluster is the whole of E3 running in a node: three
// nodes, one partitioned metadata plane, and a listing answered from it rather
// than from a merge across every node's own index.
//
// The plane is opened, partitioned by the elected controller, routed to over
// the peer transport, and handed to the coordinator as its metastore — none of
// which any single test below this one exercises together.
func TestShardedPlaneServesTheCluster(t *testing.T) {
	endpoint := startTestEtcd(t)

	nodes := make([]*clusterRuntime, 3)
	for i := range nodes {
		nodes[i] = planeNode(t, endpoint, "/fs-plane", i)
	}

	for _, rt := range nodes {
		require.NotNil(t, rt.metaPlane, "the plane is on")

		require.Eventually(t, func() bool {
			return rt.coord.Topology().DiskCount() == len(nodes)
		}, 20*time.Second, 20*time.Millisecond, "every node must join")
	}

	// The elected controller partitions the plane. Nothing here says which node
	// holds the election, which is the point.
	require.Eventually(t, func() bool {
		return planeRangeCount(t.Context(), nodes[0]) == 8
	}, 30*time.Second, 50*time.Millisecond, "the plane must be partitioned")

	// Written through one node, listed from every node. The keys are chosen to
	// land in different ranges, so this is routing rather than one shard
	// answering everything.
	store := nodes[0].Storage

	require.NoError(t, store.CreateBucket(t.Context(), "b"))

	keys := []string{"alpha", "delta", "mike", "sierra", "zulu"}
	for _, key := range keys {
		_, err := store.PutObject(t.Context(), &fs.PutObjectRequest{
			Bucket: "b", Key: key,
			Reader: bytes.NewReader([]byte(key)), Size: int64(len(key)),
		})
		require.NoError(t, err)
	}

	nodes[0].coord.Flush()

	// A fresh plane is building, so a listing falls back to the sidecar walk
	// until a rebuild has run. That is the correct lifecycle — a plane holding
	// nothing must not be trusted — and it means an S3 listing alone proves
	// nothing about the plane. Assert on the plane itself first.
	for i, rt := range nodes {
		state, err := rt.metaPlane.Store().State(t.Context())
		require.NoError(t, err)
		assert.Equal(t, metastore.StateBuilding, state,
			"node %d: a plane nobody has rebuilt is not ready", i)

		require.Eventually(t, func() bool {
			return len(planeKeys(t, rt)) == len(keys)
		}, 30*time.Second, 50*time.Millisecond,
			"node %d must see every key through the plane", i)

		assert.Equal(t, keys, planeKeys(t, rt),
			"node %d: one scan of the plane, in key order, whichever shards hold them", i)
	}

	// Now the S3 path, with the plane vouched for.
	for i, rt := range nodes {
		require.NoError(t, rt.metaPlane.state.Set(t.Context(), metastore.Ready()))

		require.Eventually(t, func() bool {
			return len(listKeys(t, rt)) == len(keys)
		}, 30*time.Second, 50*time.Millisecond, "node %d must list every key", i)

		assert.Equal(t, keys, listKeys(t, rt), "node %d lists in key order", i)
	}

	// And it is one scan, not a merge — which is the entire point of the
	// substitution, and the only part of it a correct key list does not prove.
	// A local-scope store would answer this page with one query per node.
	rt := nodes[1]

	before := rt.coord.ListingStats()
	require.Len(t, listKeys(t, rt), len(keys))

	after := rt.coord.ListingStats()
	assert.Equal(t, int64(1), after.Queries-before.Queries,
		"a cluster-scope listing page is one query, whatever the cluster size")
}

// planeKeys lists the test bucket straight out of a node's plane, bypassing the
// coordinator — so a result here is the sharded store answering, never the
// sidecar walk it falls back to.
func planeKeys(t *testing.T, rt *clusterRuntime) []string {
	t.Helper()

	var out []string

	err := rt.metaPlane.Store().Scan(t.Context(), "b", "", "", 0,
		func(e metastore.Entry) error {
			out = append(out, e.Key)

			return nil
		})
	if err != nil {
		return nil
	}

	return out
}

// TestShardedPlaneIsOffByDefault: the plane is derived, so turning it on costs
// a rebuild — which makes it an opt-in rather than something a config that
// worked yesterday quietly acquires.
func TestShardedPlaneIsOffByDefault(t *testing.T) {
	cfg := validClusterConfig()
	require.NoError(t, cfg.Validate())

	assert.False(t, cfg.Cluster.Metadata.Sharded)
	assert.Equal(t, DefaultMetadataRanges, cfg.MetadataRanges())
	assert.Equal(t, DefaultMetadataReplicas, cfg.MetadataReplicas())
}

// TestPlaneReportsClusterScope is what makes the listing one scan rather than a
// merge: the coordinator asks the store what it describes, and the sharded
// plane says the whole cluster.
func TestPlaneReportsClusterScope(t *testing.T) {
	endpoint := startTestEtcd(t)

	rt := planeNode(t, endpoint, "/fs-scope", 0)

	require.NotNil(t, rt.metaPlane)
	assert.Equal(t, metastore.ScopeCluster, rt.metaPlane.Store().Scope())
}

// planeRangeCount is how many ranges the partitioning holds, or zero if there
// is none yet.
func planeRangeCount(ctx context.Context, rt *clusterRuntime) int {
	m, err := rt.metaPlane.loadMap(ctx)
	if err != nil {
		return 0
	}

	return len(m.Ranges)
}

// listKeys lists the test bucket through a node's storage.
func listKeys(t *testing.T, rt *clusterRuntime) []string {
	t.Helper()

	res, err := rt.Storage.ListObjects(t.Context(), &fs.ListObjectsRequest{Bucket: "b"})
	if err != nil {
		return nil
	}

	out := make([]string, 0, len(res.Objects))
	for _, o := range res.Objects {
		out = append(out, o.Key)
	}

	return out
}
