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
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// usageNode boots one full cluster node against the given etcd endpoint.
func usageNode(t *testing.T, endpoint, prefix string, index int) *clusterRuntime {
	t.Helper()

	addr := testFreeAddr(t)

	cfg := validClusterConfig()
	cfg.Cluster.NodeID = "n" + strconv.Itoa(index)
	cfg.Cluster.Rack = "r" + strconv.Itoa(index)
	cfg.Cluster.Addr = addr
	cfg.Cluster.AdvertiseAddr = addr
	cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: prefix, TTL: 2 * time.Second}
	cfg.Cluster.Disks = []ClusterDiskConfig{
		{ID: "d0", Path: filepath.Join(t.TempDir(), "d0")},
	}
	cfg.Storage.Fsync = "none"
	require.NoError(t, cfg.Validate())

	rt, err := buildCluster(t.Context(), zaptest.NewLogger(t), cfg, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.close() })

	return rt
}

// TestBucketUsageEndToEnd drives the whole accounting path against a real
// cluster: objects written through the S3 interface, deltas flushed to etcd,
// and the recount re-deriving the same totals from the objects themselves.
func TestBucketUsageEndToEnd(t *testing.T) {
	endpoint := startTestEtcd(t)
	prefix := "/fs-usage-e2e"

	grp, grpCtx := errgroup.WithContext(t.Context())

	nodes := make([]*clusterRuntime, 3)
	for i := range nodes {
		nodes[i] = usageNode(t, endpoint, prefix, i)

		grp.Go(func() error { return nodes[i].Serve(grpCtx) })
	}

	require.Eventually(t, func() bool {
		return nodes[0].coord.Topology().DiskCount() == 3
	}, 15*time.Second, 20*time.Millisecond, "topology must converge")

	storage := nodes[0].Storage
	require.NoError(t, storage.CreateBucket(t.Context(), "photos"))

	put := func(key string, size int) {
		t.Helper()

		_, err := storage.PutObject(t.Context(), &fs.PutObjectRequest{
			Bucket: "photos", Key: key, Reader: bytes.NewReader(make([]byte, size)), Size: int64(size),
		})
		require.NoError(t, err)
	}

	put("a.jpg", 1000)
	put("b.jpg", 2000)
	// An overwrite: still two objects, 500 bytes less.
	put("a.jpg", 500)

	cfg := etcd.Config{Prefix: prefix, TTL: 2}

	// The write path only accumulates; the flush is what reaches etcd.
	require.NoError(t, nodes[0].usage.Flush(t.Context()))

	rec, present, err := etcd.LoadBucketUsage(t.Context(), nodes[0].client, cfg, "photos")
	require.NoError(t, err)
	require.True(t, present)
	assert.EqualValues(t, 2, rec.Objects)
	assert.EqualValues(t, 2500, rec.Bytes)
	assert.True(t, rec.Counted.IsZero(), "deltas alone do not anchor a total")

	require.NoError(t, storage.DeleteObject(t.Context(), "photos", "b.jpg"))
	require.NoError(t, nodes[0].usage.Flush(t.Context()))

	rec, _, err = etcd.LoadBucketUsage(t.Context(), nodes[0].client, cfg, "photos")
	require.NoError(t, err)
	assert.EqualValues(t, 1, rec.Objects)
	assert.EqualValues(t, 500, rec.Bytes)

	// Drift: a delta that never reached etcd, standing in for a node that died
	// between committing a write and flushing its accounting. The counters are
	// now wrong in the way only a recount can fix.
	require.NoError(t, etcd.AddBucketUsage(t.Context(), nodes[0].client, cfg, "photos", 7, 7_000))

	require.NoError(t, nodes[0].recountUsage(t.Context()))

	rec, _, err = etcd.LoadBucketUsage(t.Context(), nodes[0].client, cfg, "photos")
	require.NoError(t, err)
	assert.EqualValues(t, 1, rec.Objects, "the recount replaces drifted counters with the truth")
	assert.EqualValues(t, 500, rec.Bytes)
	assert.False(t, rec.Counted.IsZero(), "and stamps when it did")

	// A bucket that exists but holds nothing is counted as zero, not left to
	// read as "never counted".
	require.NoError(t, storage.CreateBucket(t.Context(), "empty"))
	require.NoError(t, nodes[0].recountUsage(t.Context()))

	rec, present, err = etcd.LoadBucketUsage(t.Context(), nodes[0].client, cfg, "empty")
	require.NoError(t, err)
	require.True(t, present)
	assert.Zero(t, rec.Objects)
	assert.False(t, rec.Counted.IsZero())

	// A deleted bucket's record does not outlive it.
	require.NoError(t, storage.DeleteBucket(t.Context(), "empty"))
	require.NoError(t, nodes[0].recountUsage(t.Context()))

	_, present, err = etcd.LoadBucketUsage(t.Context(), nodes[0].client, cfg, "empty")
	require.NoError(t, err)
	assert.False(t, present, "usage records are pruned with their bucket")

	// The admin API reads the same records.
	usage, err := newBucketUsageSource(nodes[0].client, cfg).BucketUsage(t.Context())
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, "photos", usage[0].Bucket)
	assert.EqualValues(t, 1, usage[0].Objects)
}

// TestUsageReporterKeepsUnflushedDeltas checks that a failed flush does not
// drop the accounting. A bucket's totals are a running sum, so a lost batch is
// lost permanently; a kept one merely arrives late.
func TestUsageReporterKeepsUnflushedDeltas(t *testing.T) {
	endpoint := startTestEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-usage-retry", TTL: 2}

	rt := usageNode(t, endpoint, "/fs-usage-retry", 0)

	rt.usage.Observe("photos", 3, 300)

	// A canceled context fails the flush the way an unreachable etcd would.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.Error(t, rt.usage.Flush(ctx))

	// The delta is still pending, and a later flush delivers all of it.
	rt.usage.Observe("photos", 1, 100)
	require.NoError(t, rt.usage.Flush(t.Context()))

	rec, present, err := etcd.LoadBucketUsage(t.Context(), rt.client, cfg, "photos")
	require.NoError(t, err)
	require.True(t, present)
	assert.EqualValues(t, 4, rec.Objects, "nothing was dropped by the failed flush")
	assert.EqualValues(t, 400, rec.Bytes)
}

// TestUsageObserverIgnoresCancellingDeltas checks the batching does not send a
// round trip for accounting that nets to nothing — an overwrite of the same
// size, or a create and delete inside one interval.
func TestUsageObserverIgnoresCancellingDeltas(t *testing.T) {
	endpoint := startTestEtcd(t)

	rt := usageNode(t, endpoint, "/fs-usage-net", 0)

	rt.usage.Observe("photos", 1, 500)
	rt.usage.Observe("photos", -1, -500)

	require.Empty(t, rt.usage.take(), "a delta that cancels out is not worth a write")
}
