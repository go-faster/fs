package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/cluster/transport"
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

	addr := testNodeAddr

	cfg := validClusterConfig()
	cfg.Cluster.Addr = addr
	cfg.Cluster.AdvertiseAddr = addr
	cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: "/fs-headless", TTL: 2 * time.Second}
	cfg.Storage.Fsync = "none"
	cfg.Storage.Root = t.TempDir()

	rt, err := buildCluster(t.Context(), lg, cfg, cfg.Storage.Root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.close() })

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

	status := newClusterStatusSource(cl.coord, cl.client, cl.etcdCfg,
		newPeerStatus(cl.self, transport.Secret(cfg.ClusterSecret())))

	st, err := status.ClusterStatus(t.Context())
	require.NoError(t, err)

	assert.Equal(t, etcd.SchemaVersion, st.SchemaVersion, "founding node stamped the schema")
	assert.Equal(t, etcd.SchemaVersion, st.BinarySchemaVersion)
	assert.False(t, st.RebalanceRunning, "no rebalance in progress")

	require.Len(t, st.Nodes, 1)
	assert.Equal(t, "n0", st.Nodes[0].ID)
	assert.Equal(t, rt.addr, st.Nodes[0].Addr,
		"the registry carries the address the node actually bound")
	require.NotEmpty(t, st.Nodes[0].Disks)
	// The node reported real filesystem capacity at registration.
	assert.Positive(t, st.Nodes[0].Disks[0].TotalBytes, "node registered with disk capacity")

	// Live state comes from the node itself over the peer transport — etcd
	// carries none of it.
	live := st.Nodes[0].Live
	require.NotNil(t, live, "node reported live state: %s", st.Nodes[0].LiveError)
	assert.Equal(t, etcd.SchemaVersion, live.SchemaVersion)
	assert.Equal(t, adminhandler.RebalanceIdle, live.RebalanceState)
	assert.Zero(t, live.RepairQueueDepth, "idle node has nothing queued")
	assert.Positive(t, live.UptimeSeconds)

	// Schema migrations, driven the way the headless admin drives them.
	mig := newMigrateController(cl.client, cl.etcdCfg, string(cl.self), clusterMigrations(cl.deps()))

	ms, err := mig.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, etcd.SchemaVersion, ms.ClusterVersion)
	assert.Equal(t, etcd.SchemaVersion, ms.BinaryVersion)
	assert.Empty(t, ms.Pending, "a founding-version cluster has nothing pending")

	// Applying with nothing pending is a no-op that never campaigns.
	ms, err = mig.Apply(t.Context())
	require.NoError(t, err)
	assert.Empty(t, ms.Pending)
	assert.Empty(t, ms.LastApplied)
}

// TestPeerStatusUnreachableNode: a node in the topology that nothing answers
// for is reported as not reporting, with a reason — never as an idle node with
// empty counters.
func TestPeerStatusUnreachableNode(t *testing.T) {
	p := newPeerStatus("admin-test", transport.Secret("test-cluster-secret-value"))
	p.timeout = time.Second

	res := p.Fetch(t.Context(), []cluster.Node{
		{ID: "gone", Addr: testFreeAddr(t)}, // Bound then released: nothing listens.
		{ID: "addrless"},
	})

	require.Len(t, res, 2)
	assert.Nil(t, res["gone"].Live)
	assert.NotEmpty(t, res["gone"].Err)
	assert.Nil(t, res["addrless"].Live)
	assert.Contains(t, res["addrless"].Err, "no address")
}
