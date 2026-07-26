package etcd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// TestDiskWeightOverrideDrains is fs SPEC §11.6: taking a disk out of
// placement without touching the node's config or restarting it.
func TestDiskWeightOverrideDrains(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-weight", TTL: 5}

	node := testNode(0)

	reg, err := etcd.Register(t.Context(), client, cfg, node)
	require.NoError(t, err)

	t.Cleanup(func() { _ = reg.Close() })

	source, err := etcd.NewSource(t.Context(), client, cfg)
	require.NoError(t, err)

	t.Cleanup(func() { _ = source.Close() })

	before := waitTopology(t, source, func(tp *cluster.Topology) bool {
		return len(tp.Nodes) == 1 && tp.Nodes[0].Disks[0].Weight == 1
	})
	signatureBefore := before.Signature()

	// Drain d0. Zero, not a negative number: an override says zero and means
	// it, which is exactly what the config file cannot do.
	require.NoError(t, etcd.SetDiskWeight(t.Context(), client, cfg, node.ID, "d0", 0, "decommissioning"))

	after := waitTopology(t, source, func(tp *cluster.Topology) bool {
		return len(tp.Nodes) == 1 && tp.Nodes[0].Disks[0].Weight == 0
	})

	assert.Equal(t, 2.0, after.Nodes[0].Disks[1].Weight, "the other disk is untouched")

	// The signature has to move, or the rebalancer never notices the drain and
	// the data stays where it is — which would make the whole feature inert.
	assert.NotEqual(t, signatureBefore, after.Signature(),
		"a drain must read as a placement change")

	// Clearing restores the registered weight.
	require.NoError(t, etcd.ClearDiskWeight(t.Context(), client, cfg, node.ID, "d0"))

	restored := waitTopology(t, source, func(tp *cluster.Topology) bool {
		return len(tp.Nodes) == 1 && tp.Nodes[0].Disks[0].Weight == 1
	})
	assert.Equal(t, signatureBefore, restored.Signature(), "clearing restores placement exactly")
}

// TestDiskWeightOverrideSurvivesReregistration is why the override is its own
// key. A node republishes its registry record on every usage refresh, so an
// override written into that record would be gone within the refresh interval
// — and a drained disk would silently come back into placement.
func TestDiskWeightOverrideSurvivesReregistration(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-weight-rereg", TTL: 5}

	node := testNode(0)

	reg, err := etcd.Register(t.Context(), client, cfg, node)
	require.NoError(t, err)

	source, err := etcd.NewSource(t.Context(), client, cfg)
	require.NoError(t, err)

	t.Cleanup(func() { _ = source.Close() })

	require.NoError(t, etcd.SetDiskWeight(t.Context(), client, cfg, node.ID, "d0", 0, ""))

	waitTopology(t, source, func(tp *cluster.Topology) bool {
		return len(tp.Nodes) == 1 && tp.Nodes[0].Disks[0].Weight == 0
	})

	// The node re-registers with its configured weights, as it does on every
	// restart and every capacity refresh.
	require.NoError(t, reg.Close())

	reg2, err := etcd.Register(t.Context(), client, cfg, node)
	require.NoError(t, err)

	t.Cleanup(func() { _ = reg2.Close() })

	// Wait for the re-registration to land, then assert the drain held.
	waitTopology(t, source, func(tp *cluster.Topology) bool {
		return len(tp.Nodes) == 1 && tp.Nodes[0].Disks[1].Weight == 2
	})

	assert.Zero(t, source.Topology().Nodes[0].Disks[0].Weight,
		"re-registering must not undo a drain")
}

// TestDiskWeightOverrideLoadedAtStartup: a source starting against an etcd
// that already holds overrides must never publish a snapshot without them,
// or it would place onto a drained disk in the window before the watch fires.
func TestDiskWeightOverrideLoadedAtStartup(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-weight-startup", TTL: 5}

	node := testNode(0)

	reg, err := etcd.Register(t.Context(), client, cfg, node)
	require.NoError(t, err)

	t.Cleanup(func() { _ = reg.Close() })

	require.NoError(t, etcd.SetDiskWeight(t.Context(), client, cfg, node.ID, "d0", 0, ""))

	source, err := etcd.NewSource(t.Context(), client, cfg)
	require.NoError(t, err)

	t.Cleanup(func() { _ = source.Close() })

	// The very first snapshot, with no waiting.
	topo := source.Topology()
	require.Len(t, topo.Nodes, 1)
	assert.Zero(t, topo.Nodes[0].Disks[0].Weight,
		"the first snapshot placed onto a disk that was already drained")
}

func TestListDiskWeights(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-weight-list", TTL: 5}

	require.NoError(t, etcd.SetDiskWeight(t.Context(), client, cfg, "n0", "d0", 0, "faulty"))
	require.NoError(t, etcd.SetDiskWeight(t.Context(), client, cfg, "n1", "d1", 0.5, ""))

	overrides, err := etcd.ListDiskWeights(t.Context(), client, cfg)
	require.NoError(t, err)
	require.Len(t, overrides, 2)

	byNode := map[cluster.NodeID]etcd.DiskWeight{}
	for _, o := range overrides {
		byNode[o.Node] = o
	}

	assert.True(t, byNode["n0"].Drained(), "weight 0 is drained")
	assert.Equal(t, "faulty", byNode["n0"].Reason)
	assert.False(t, byNode["n1"].Drained(), "a positive weight is not a drain")
	assert.Equal(t, 0.5, byNode["n1"].Weight)

	require.NoError(t, etcd.ClearDiskWeight(t.Context(), client, cfg, "n0", "d0"))

	overrides, err = etcd.ListDiskWeights(t.Context(), client, cfg)
	require.NoError(t, err)
	assert.Len(t, overrides, 1)

	// Clearing something that is not there is what the caller wanted anyway.
	require.NoError(t, etcd.ClearDiskWeight(t.Context(), client, cfg, "n0", "d0"))
}
