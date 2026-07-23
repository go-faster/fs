package etcd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// TestRegistrationUpdate refreshes a node's per-disk capacity in place: the
// topology reflects it, the placement signature does not change, and the
// lease survives.
func TestRegistrationUpdate(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-usage", TTL: 2}

	source, err := etcd.NewSource(t.Context(), client, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })

	node := testNode(0)

	reg, err := etcd.Register(t.Context(), client, cfg, node)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })

	topo := waitTopology(t, source, func(tp *cluster.Topology) bool { return len(tp.Nodes) == 1 })
	sig := topo.Signature()
	assert.Zero(t, topo.Nodes[0].Disks[0].TotalBytes, "no capacity reported yet")

	// Refresh with capacity.
	node.Disks[0].TotalBytes = 1 << 40
	node.Disks[0].FreeBytes = 1 << 39

	require.NoError(t, reg.Update(t.Context(), node))

	topo = waitTopology(t, source, func(tp *cluster.Topology) bool {
		return len(tp.Nodes) == 1 && tp.Nodes[0].Disks[0].TotalBytes == 1<<40
	})
	assert.Equal(t, uint64(1<<39), topo.Nodes[0].Disks[0].FreeBytes)
	assert.Equal(t, sig, topo.Signature(), "capacity refresh must not read as a membership change")
}
