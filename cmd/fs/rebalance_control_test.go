package main

import (
	"bytes"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// TestRebalanceController exercises the admin-API rebalance runner against
// real cluster nodes and etcd: transition guards, waiting behind an external
// leader, pause, resume from the cursor, and a full elected run.
func TestRebalanceController(t *testing.T) {
	endpoint := startTestEtcd(t)
	lg := zaptest.NewLogger(t)

	grp, grpCtx := errgroup.WithContext(t.Context())

	nodeConfig := func(i int) Config {
		addr := testFreeAddr(t)

		cfg := validClusterConfig()
		cfg.Cluster.NodeID = "n" + strconv.Itoa(i)
		cfg.Cluster.Rack = "r" + strconv.Itoa(i)
		cfg.Cluster.Addr = addr
		cfg.Cluster.AdvertiseAddr = addr
		cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: "/fs-admin-rebalance", TTL: 2 * time.Second}
		cfg.Storage.Fsync = "none"

		return cfg
	}

	nodes := make([]*clusterRuntime, 3)

	for i := range nodes {
		cfg := nodeConfig(i)
		cfg.Storage.Root = t.TempDir()

		rt, err := buildCluster(t.Context(), lg, cfg, cfg.Storage.Root)
		require.NoError(t, err)

		nodes[i] = rt

		grp.Go(func() error { return rt.Serve(grpCtx) })
	}

	require.Eventually(t, func() bool {
		return nodes[0].coord.Topology().DiskCount() == 3
	}, 15*time.Second, 20*time.Millisecond, "topology must converge")

	require.NoError(t, nodes[0].Storage.CreateBucket(t.Context(), "b"))

	for i := range 8 {
		key := "k" + strconv.Itoa(i)
		data := bytes.Repeat([]byte{byte(i)}, 3000)

		_, err := nodes[0].Storage.PutObject(t.Context(), &fs.PutObjectRequest{
			Bucket: "b", Key: key, Reader: bytes.NewReader(data), Size: int64(len(data)),
		})
		require.NoError(t, err)
	}

	ctl := nodes[0].rebalance
	require.NotNil(t, ctl)

	// Idle: nothing to pause or resume.
	assert.Equal(t, adminhandler.RebalanceIdle, ctl.Status(t.Context()).State)
	require.ErrorIs(t, ctl.Pause(t.Context()), adminhandler.ErrRebalanceConflict)
	require.ErrorIs(t, ctl.Resume(t.Context()), adminhandler.ErrRebalanceConflict)

	// Hold the cluster-wide slot externally: a started controller must wait.
	client, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	etcdCfg := etcd.Config{Prefix: "/fs-admin-rebalance", TTL: 2}

	external, err := etcd.CampaignRebalance(t.Context(), client, etcdCfg, "external-holder")
	require.NoError(t, err)

	require.NoError(t, ctl.Start(t.Context()))
	require.ErrorIs(t, ctl.Start(t.Context()), adminhandler.ErrRebalanceConflict, "double start")

	time.Sleep(300 * time.Millisecond) // The campaign must actually block.
	assert.Equal(t, adminhandler.RebalanceWaiting, ctl.Status(t.Context()).State)

	// Pause while waiting: the runner exits without ever being elected.
	require.NoError(t, ctl.Pause(t.Context()))
	assert.Equal(t, adminhandler.RebalancePaused, ctl.Status(t.Context()).State)
	require.ErrorIs(t, ctl.Pause(t.Context()), adminhandler.ErrRebalanceConflict, "double pause")

	// Release the external hold and resume: the full walk runs to done.
	require.NoError(t, external.Close())
	require.NoError(t, ctl.Resume(t.Context()))

	require.Eventually(t, func() bool {
		return ctl.Status(t.Context()).State == adminhandler.RebalanceDone
	}, 30*time.Second, 50*time.Millisecond, "rebalance must complete")

	st := ctl.Status(t.Context())
	assert.Equal(t, 8, st.Objects)
	assert.Zero(t, st.Failed)
	assert.Empty(t, st.Err)
	assert.False(t, st.StartedAt.IsZero())
	assert.False(t, st.FinishedAt.IsZero())
	assert.Empty(t, st.CursorBucket, "cursor must be cleared after completion")
	assert.GreaterOrEqual(t, st.RepairQueueDepth, 0)

	// Done is restartable; a second run over a converged cluster is a no-op.
	require.NoError(t, ctl.Start(t.Context()))

	require.Eventually(t, func() bool {
		return ctl.Status(t.Context()).State == adminhandler.RebalanceDone
	}, 30*time.Second, 50*time.Millisecond, "second rebalance must complete")

	assert.Zero(t, ctl.Status(t.Context()).Relocated, "converged cluster: nothing to relocate")

	// Resume without a pause: conflict.
	require.ErrorIs(t, ctl.Resume(t.Context()), adminhandler.ErrRebalanceConflict)
}
