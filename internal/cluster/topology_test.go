package cluster

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTopologySignature(t *testing.T) {
	base := func() *Topology {
		return &Topology{
			Epoch: 7,
			Nodes: []Node{
				{ID: "n0", Addr: "10.0.0.1:7080", Rack: "r0", Disks: []Disk{{ID: "d0", Weight: 1}, {ID: "d1", Weight: 2}}},
				{ID: "n1", Addr: "10.0.0.2:7080", Rack: "r1", Disks: []Disk{{ID: "d0", Weight: 1}}},
			},
		}
	}

	sig := base().Signature()

	// Placement-irrelevant churn: same signature.
	t.Run("ignores epoch and addresses", func(t *testing.T) {
		topo := base()
		topo.Epoch = 99
		topo.Nodes[0].Addr = "10.9.9.9:7080"

		assert.Equal(t, sig, topo.Signature())
	})

	t.Run("ignores node and disk order", func(t *testing.T) {
		topo := base()
		topo.Nodes[0], topo.Nodes[1] = topo.Nodes[1], topo.Nodes[0]
		topo.Nodes[1].Disks[0], topo.Nodes[1].Disks[1] = topo.Nodes[1].Disks[1], topo.Nodes[1].Disks[0]

		assert.Equal(t, sig, topo.Signature())
	})

	// Placement-relevant changes: different signature.
	for name, mutate := range map[string]func(*Topology){
		"node added":     func(topo *Topology) { topo.Nodes = append(topo.Nodes, Node{ID: "n2", Rack: "r2"}) },
		"node removed":   func(topo *Topology) { topo.Nodes = topo.Nodes[:1] },
		"rack changed":   func(topo *Topology) { topo.Nodes[0].Rack = "rX" },
		"disk added":     func(topo *Topology) { topo.Nodes[1].Disks = append(topo.Nodes[1].Disks, Disk{ID: "d1", Weight: 1}) },
		"weight changed": func(topo *Topology) { topo.Nodes[0].Disks[1].Weight = 3 },
		"disk drained":   func(topo *Topology) { topo.Nodes[0].Disks[0].Weight = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			topo := base()
			mutate(topo)

			assert.NotEqual(t, sig, topo.Signature())
		})
	}
}
