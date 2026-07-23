package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"
)

func TestClusterMetrics(t *testing.T) {
	endpoint := startTestEtcd(t)
	lg := zaptest.NewLogger(t)

	addr := testFreeAddr(t)

	cfg := validClusterConfig()
	cfg.Cluster.Addr = addr
	cfg.Cluster.AdvertiseAddr = addr
	cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: "/fs-metrics", TTL: 2 * time.Second}
	cfg.Storage.Fsync = "none"
	cfg.Storage.Root = t.TempDir()

	rt, err := buildCluster(t.Context(), lg, cfg, cfg.Storage.Root)
	require.NoError(t, err)

	grp, grpCtx := errgroup.WithContext(t.Context())
	grp.Go(func() error { return rt.Serve(grpCtx) })

	require.Eventually(t, func() bool {
		return rt.coord.Topology().DiskCount() == 1
	}, 15*time.Second, 20*time.Millisecond)

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	require.NoError(t, rt.RegisterMetrics(provider))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	byName := make(map[string]metricdata.Metrics)

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			byName[m.Name] = m
		}
	}

	for _, name := range []string{
		"fs.cluster.disk.total_bytes",
		"fs.cluster.disk.free_bytes",
		"fs.cluster.disk.fullness",
		"fs.cluster.placement.skew",
		"fs.cluster.nodes",
		"fs.cluster.disks",
		"fs.cluster.repair.queue_depth",
		"fs.cluster.rebalance.active",
		"fs.cluster.rebalance.objects",
		"fs.cluster.scrub.passes",
		"fs.cluster.repair.rebuilt_fragments",
		"fs.cluster.repair.converted_objects",
	} {
		assert.Contains(t, byName, name)
	}

	// The disk registered with real filesystem capacity: the gauge is live.
	total, ok := byName["fs.cluster.disk.total_bytes"].Data.(metricdata.Gauge[int64])
	require.True(t, ok)
	require.Len(t, total.DataPoints, 1)
	assert.Positive(t, total.DataPoints[0].Value)

	nodes, ok := byName["fs.cluster.nodes"].Data.(metricdata.Gauge[int64])
	require.True(t, ok)
	require.Len(t, nodes.DataPoints, 1)
	assert.Equal(t, int64(1), nodes.DataPoints[0].Value)

	// withUsage fills capacity for the usage reporter path too.
	withCap := rt.withUsage(rt.node)
	require.Len(t, withCap.Disks, 1)
	assert.Positive(t, withCap.Disks[0].TotalBytes)
}
