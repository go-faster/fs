package clusterstore

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/go-faster/fs/internal/cluster"
)

func TestEpochMemoryDedupBySignature(t *testing.T) {
	node := func(id string, free uint64) cluster.Node {
		return cluster.Node{ID: cluster.NodeID(id), Rack: "r-" + id, Disks: []cluster.Disk{{ID: "d0", Weight: 1, FreeBytes: free}}}
	}

	var m epochMemory

	// A genuine membership change: both epochs remembered.
	t1 := &cluster.Topology{Epoch: 1, Nodes: []cluster.Node{node("a", 100)}}
	t2 := &cluster.Topology{Epoch: 2, Nodes: []cluster.Node{node("a", 100), node("b", 100)}}

	m.observe(t1)
	out := m.observe(t2)
	assert.Len(t, out, 2)

	// Capacity-refresh churn: same membership, new epochs. Each refresh
	// replaces the previous same-placement snapshot instead of flushing the
	// genuinely distinct epoch out of the bound.
	for e := uint64(3); e < 20; e++ {
		refreshed := &cluster.Topology{Epoch: e, Nodes: []cluster.Node{node("a", 50), node("b", e)}}
		out = m.observe(refreshed)
	}

	assert.Len(t, out, 2, "usage churn must not grow the memory")
	assert.Equal(t, uint64(19), out[0].Epoch)
	assert.Equal(t, uint64(1), out[1].Epoch, "the old single-node epoch survives 17 refreshes")

	// The bound still applies to genuinely distinct placements.
	for i := range 6 {
		distinct := &cluster.Topology{Epoch: uint64(30 + i), Nodes: []cluster.Node{node("a", 1), node("n-"+string(rune('a'+i)), 1)}}
		out = m.observe(distinct)
	}

	assert.Len(t, out, maxRememberedEpochs)
}
