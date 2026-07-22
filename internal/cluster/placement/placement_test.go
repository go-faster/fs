package placement_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/placement"
)

// topo builds a topology of racks×nodesPerRack×disksPerNode, weight 1 each.
func topo(racks, nodesPerRack, disksPerNode int) *cluster.Topology {
	t := &cluster.Topology{Epoch: 1}

	for r := range racks {
		for n := range nodesPerRack {
			node := cluster.Node{
				ID:   cluster.NodeID(fmt.Sprintf("r%d-n%d", r, n)),
				Rack: fmt.Sprintf("rack%d", r),
				Addr: fmt.Sprintf("10.0.%d.%d:7000", r, n),
			}
			for d := range disksPerNode {
				node.Disks = append(node.Disks, cluster.Disk{
					ID:     cluster.DiskID(fmt.Sprintf("d%d", d)),
					Weight: 1,
				})
			}

			t.Nodes = append(t.Nodes, node)
		}
	}

	return t
}

func distinct[T comparable](xs []T) map[T]struct{} {
	m := make(map[T]struct{}, len(xs))
	for _, x := range xs {
		m[x] = struct{}{}
	}

	return m
}

func racksOf(ts []placement.Target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Rack
	}

	return out
}

func nodesOf(ts []placement.Target) []cluster.NodeID {
	out := make([]cluster.NodeID, len(ts))
	for i, t := range ts {
		out[i] = t.Node
	}

	return out
}

func TestPlaceDeterministic(t *testing.T) {
	top := topo(3, 3, 4)

	for i := range 200 {
		key := placement.ObjectKey("bucket", fmt.Sprintf("obj-%d", i))
		a := placement.Place(top, key, 3)
		b := placement.Place(top, key, 3)
		require.Equal(t, a, b, "placement must be stable for the same inputs")
	}
}

func TestPlaceDistinctDisks(t *testing.T) {
	top := topo(3, 3, 4)

	for i := range 500 {
		key := placement.ObjectKey("b", fmt.Sprintf("k%d", i))
		ts := placement.Place(top, key, 6) // RS(4,2)
		require.Len(t, ts, 6)

		type pd struct {
			n cluster.NodeID
			d cluster.DiskID
		}

		seen := map[pd]struct{}{}

		for _, x := range ts {
			key := pd{x.Node, x.Disk}

			_, dup := seen[key]
			require.False(t, dup, "a disk must never appear twice: %+v", x)

			seen[key] = struct{}{}
		}
	}
}

func TestPlaceSpreadsAcrossRacks(t *testing.T) {
	// 3 racks: 3 copies must land in 3 distinct racks, every time.
	top := topo(3, 4, 2)

	for i := range 1000 {
		key := placement.ObjectKey("b", fmt.Sprintf("k%d", i))
		ts := placement.Place(top, key, 3)
		require.Len(t, ts, 3)
		assert.Len(t, distinct(racksOf(ts)), 3, "3 copies over 3 racks must be rack-distinct")
	}
}

func TestPlacePrimaryIsGlobalHRWWinner(t *testing.T) {
	top := topo(3, 3, 3)

	// The first target for count=1 and count=5 must be identical: adding copies
	// never changes the primary.
	for i := range 300 {
		key := placement.ObjectKey("b", fmt.Sprintf("k%d", i))
		one := placement.Place(top, key, 1)
		many := placement.Place(top, key, 5)

		require.Len(t, one, 1)
		require.Equal(t, one[0], many[0])
	}
}

func TestPlaceDegradesSingleRackToNodes(t *testing.T) {
	// One rack, 3 nodes, 2 disks each: 3 copies can't be rack-distinct, so they
	// must spread across the 3 distinct nodes.
	top := topo(1, 3, 2)

	for i := range 500 {
		key := placement.ObjectKey("b", fmt.Sprintf("k%d", i))
		ts := placement.Place(top, key, 3)
		require.Len(t, ts, 3)
		assert.Len(t, distinct(nodesOf(ts)), 3, "within one rack, spread across nodes")
	}
}

func TestPlaceDegradesSingleNodeToDisks(t *testing.T) {
	// One node, 4 disks: 3 copies land on 3 distinct disks of that node.
	top := topo(1, 1, 4)

	key := placement.ObjectKey("b", "k")
	ts := placement.Place(top, key, 3)
	require.Len(t, ts, 3)
	assert.Len(t, distinct([]cluster.DiskID{ts[0].Disk, ts[1].Disk, ts[2].Disk}), 3)
}

func TestPlaceShortWhenNotEnoughDisks(t *testing.T) {
	top := topo(1, 1, 2) // only 2 disks

	ts := placement.Place(top, placement.ObjectKey("b", "k"), 3)
	assert.Len(t, ts, 2, "cannot satisfy 3 copies with 2 disks; return what exists, no dupes")
}

func TestPlaceSkipsDrainedDisks(t *testing.T) {
	top := topo(2, 1, 1)
	// Drain every disk on rack1's node.
	for i := range top.Nodes {
		if top.Nodes[i].Rack == "rack1" {
			for j := range top.Nodes[i].Disks {
				top.Nodes[i].Disks[j].Weight = 0
			}
		}
	}

	for i := range 200 {
		key := placement.ObjectKey("b", fmt.Sprintf("k%d", i))
		ts := placement.Place(top, key, 2)

		for _, x := range ts {
			assert.NotEqual(t, "rack1", x.Rack, "drained disks must never be chosen")
		}
	}
}

func TestPlaceEmptyOrInvalid(t *testing.T) {
	assert.Nil(t, placement.Place(nil, "k", 3))
	assert.Nil(t, placement.Place(&cluster.Topology{}, "k", 3))
	assert.Nil(t, placement.Place(topo(1, 1, 1), "k", 0))
}

// TestPlaceBalance checks that placement spreads roughly evenly across equal
// -weight disks: no disk should own a wildly disproportionate share of primaries.
func TestPlaceBalance(t *testing.T) {
	top := topo(3, 3, 2) // 18 disks

	const keys = 90000

	counts := map[string]int{} // by (node/disk)

	for i := range keys {
		ts := placement.Place(top, placement.ObjectKey("b", fmt.Sprintf("k%d", i)), 1)
		require.Len(t, ts, 1)
		counts[string(ts[0].Node)+"/"+string(ts[0].Disk)]++
	}

	require.Len(t, counts, 18, "every disk should receive some primaries")

	expected := float64(keys) / 18
	for id, c := range counts {
		ratio := float64(c) / expected
		assert.Greater(t, ratio, 0.80, "disk %s under-loaded: %d", id, c)
		assert.Less(t, ratio, 1.20, "disk %s over-loaded: %d", id, c)
	}
}

// TestPlaceWeighted checks weighted placement: a 3× disk should attract roughly
// 3× the primaries of a 1× disk in the same node.
func TestPlaceWeighted(t *testing.T) {
	top := &cluster.Topology{
		Epoch: 1,
		Nodes: []cluster.Node{{
			ID:   "solo",
			Rack: "r0",
			Disks: []cluster.Disk{
				{ID: "light", Weight: 1},
				{ID: "heavy", Weight: 3},
			},
		}},
	}

	const keys = 80000

	c := map[cluster.DiskID]int{}

	for i := range keys {
		ts := placement.Place(top, placement.ObjectKey("b", fmt.Sprintf("k%d", i)), 1)
		c[ts[0].Disk]++
	}

	ratio := float64(c["heavy"]) / float64(c["light"])
	assert.InDelta(t, 3.0, ratio, 0.25, "heavy:light primary ratio should track weights (3:1)")
}

// TestPlaceMinimalDisruption verifies the HRW property: adding a node moves only
// a small fraction of primaries (≈ 1/newNodeCount), never a global reshuffle.
func TestPlaceMinimalDisruption(t *testing.T) {
	before := topo(3, 3, 2) // 9 nodes, 18 disks
	after := topo(3, 3, 2)
	// Add a 10th node in a new-ish position (rack0).
	after.Nodes = append(after.Nodes, cluster.Node{
		ID:    "extra",
		Rack:  "rack0",
		Disks: []cluster.Disk{{ID: "d0", Weight: 1}, {ID: "d1", Weight: 1}},
	})

	const keys = 20000

	moved := 0

	for i := range keys {
		key := placement.ObjectKey("b", fmt.Sprintf("k%d", i))
		a := placement.Place(before, key, 1)[0]
		b := placement.Place(after, key, 1)[0]

		if a != b {
			moved++
		}
	}

	frac := float64(moved) / float64(keys)
	// Adding 2 disks to 18 → ~20 disks; ideal churn ≈ 2/20 = 10%. Allow slack
	// for the rack constraint, but it must be far below a full reshuffle.
	assert.Less(t, frac, 0.20, "adding a node reshuffled too many keys: %.1f%%", frac*100)
}
