package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs/internal/cluster/etcd"
)

func TestAdminFlagValidation(t *testing.T) {
	cmd := Admin()
	cmd.SetArgs([]string{})
	// No config → missing etcd endpoints. Run (not RunE) exits; exercise the
	// validation path via runHeadlessAdmin's precondition instead.
	cfg := DefaultConfig()
	require.ErrorContains(t, validateClusterClientConfig(cfg), "etcd.endpoints")
}

// TestHeadlessAdminClusterStatus builds a real cluster node and reads the
// cluster-wide status the way the headless `fs admin` does — through a
// disk-less client over etcd — asserting it reports the node, its disks and
// capacity, and the agreed schema version, without being a data node itself.
func TestHeadlessAdminClusterStatus(t *testing.T) {
	endpoint := startTestEtcd(t)
	lg := zaptest.NewLogger(t)

	addr := testFreeAddr(t)

	cfg := validClusterConfig()
	cfg.Cluster.Addr = addr
	cfg.Cluster.AdvertiseAddr = addr
	cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: "/fs-headless", TTL: 2 * time.Second}
	cfg.Storage.Fsync = "none"
	cfg.Storage.Root = t.TempDir()

	rt, err := buildCluster(t.Context(), lg, cfg, cfg.Storage.Root)
	require.NoError(t, err)

	grp, grpCtx := errgroup.WithContext(t.Context())
	grp.Go(func() error { return rt.Serve(grpCtx) })

	require.Eventually(t, func() bool {
		return rt.coord.Topology().DiskCount() >= 1
	}, 15*time.Second, 20*time.Millisecond, "node must join")

	// The headless admin's view: a disk-less client + the status source.
	cl, err := dialClusterClient(t.Context(), cfg, "admin-test", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })

	require.Eventually(t, func() bool {
		return cl.coord.Topology().DiskCount() >= 1
	}, 15*time.Second, 20*time.Millisecond, "client must see the node")

	status := newClusterStatusSource(cl.coord, cl.client, cl.etcdCfg)

	st, err := status.ClusterStatus(t.Context())
	require.NoError(t, err)

	assert.Equal(t, etcd.SchemaVersion, st.SchemaVersion, "founding node stamped the schema")
	assert.Equal(t, etcd.SchemaVersion, st.BinarySchemaVersion)
	assert.False(t, st.RebalanceRunning, "no rebalance in progress")

	require.Len(t, st.Nodes, 1)
	assert.Equal(t, "n0", st.Nodes[0].ID)
	assert.Equal(t, addr, st.Nodes[0].Addr)
	require.NotEmpty(t, st.Nodes[0].Disks)
	// The node reported real filesystem capacity at registration.
	assert.Positive(t, st.Nodes[0].Disks[0].TotalBytes, "node registered with disk capacity")
}
