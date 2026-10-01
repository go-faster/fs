package layout_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/layout"
)

const tb = uint64(1) << 40

// grid builds zones×racks×nodes members of equal capacity.
func grid(zones, racks, nodes int) []layout.Node {
	var out []layout.Node

	for z := range zones {
		for r := range racks {
			for n := range nodes {
				out = append(out, layout.Node{
					ID:       layout.NodeID(fmt.Sprintf("z%d-r%d-n%d", z, r, n)),
					Zone:     fmt.Sprintf("z%d", z),
					Rack:     fmt.Sprintf("r%d", r),
					Capacity: tb,
				})
			}
		}
	}

	return out
}

func compute(t *testing.T, prev *layout.Layout, nodes []layout.Node, widths ...int) *layout.Layout {
	t.Helper()

	l, err := layout.Compute(prev, nodes, layout.Options{Widths: widths})
	require.NoError(t, err)
	requireValid(t, l)

	return l
}

// requireValid checks what every layout must satisfy: every slot filled, by a
// node with capacity, on distinct nodes within a partition.
func requireValid(t *testing.T, l *layout.Layout) {
	t.Helper()

	data := map[layout.NodeID]bool{}
	for _, n := range l.Nodes {
		data[n.ID] = n.Capacity > 0
	}

	for p, slots := range l.Slots {
		require.Len(t, slots, l.Width(), "partition %d", p)

		seen := map[layout.NodeID]bool{}

		for j, id := range slots {
			require.True(t, data[id], "partition %d slot %d: %q holds no data", p, j, id)
			require.False(t, seen[id], "partition %d: %q twice", p, id)
			seen[id] = true
		}
	}
}

// moved counts slots whose node differs between two layouts.
func moved(a, b *layout.Layout) int {
	n := 0

	for p := range a.Slots {
		for j := range a.Slots[p] {
			if a.Slots[p][j] != b.Slots[p][j] {
				n++
			}
		}
	}

	return n
}

func TestComputeSpreadsAcrossZones(t *testing.T) {
	l := compute(t, nil, grid(3, 2, 2), 3)

	zone, _ := l.MaxPerDomain(3)
	assert.Equal(t, 1, zone, "three replicas must land in three zones")
}

func TestComputeSpreadsEveryWidth(t *testing.T) {
	// ec:4,2 alongside three replicas: the first three slots span every zone,
	// and the six hold at most two per zone, so a lost zone leaves four shards.
	l := compute(t, nil, grid(3, 2, 2), 3, 6)

	zone, _ := l.MaxPerDomain(3)
	assert.Equal(t, 1, zone)

	zone, _ = l.MaxPerDomain(6)
	assert.Equal(t, 2, zone)
}

func TestComputeFallsBackToRacks(t *testing.T) {
	l := compute(t, nil, grid(1, 3, 2), 3)

	_, rack := l.MaxPerDomain(3)
	assert.Equal(t, 1, rack, "with one zone, racks are the domain")
}

func TestComputeUnevenZones(t *testing.T) {
	// The third zone has one node and cannot take two of six slots, so the
	// other zones must hold three each.
	nodes := append(grid(2, 1, 4), layout.Node{ID: "small", Zone: "z9", Rack: "r0", Capacity: tb})

	l := compute(t, nil, nodes, 6)

	zone, _ := l.MaxPerDomain(6)
	assert.Equal(t, 3, zone)

	// The constrained layout is still a fixed point.
	again := compute(t, l, slices.Concat(nodes, []layout.Node{{ID: "gw", Capacity: 0}}), 6)
	assert.Zero(t, moved(l, again))
}

func TestComputeBalance(t *testing.T) {
	for _, n := range []int{3, 6, 12, 30, 99} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			nodes := make([]layout.Node, n)
			for i := range nodes {
				nodes[i] = layout.Node{
					ID:       layout.NodeID(fmt.Sprintf("n%03d", i)),
					Zone:     fmt.Sprintf("z%d", i%3),
					Capacity: tb,
				}
			}

			l := compute(t, nil, nodes, 3)

			loads := l.Load()
			lo, hi := len(l.Slots)*3, 0

			for _, v := range loads {
				lo, hi = min(lo, v), max(hi, v)
			}

			assert.LessOrEqual(t, hi-lo, 1, "loads %v", loads)
		})
	}
}

func TestComputeSpreadBeatsBalance(t *testing.T) {
	// A zone with one node still takes one replica of every partition: losing
	// a zone must cost one copy, whatever the capacities say.
	nodes := append(grid(2, 1, 2), layout.Node{ID: "alone", Zone: "z9", Capacity: tb})

	l := compute(t, nil, nodes, 3)
	assert.Equal(t, len(l.Slots), l.Load()["alone"])
}

func TestComputeWeighted(t *testing.T) {
	nodes := grid(3, 1, 2)
	nodes[0].Capacity = 2 * tb

	l := compute(t, nil, nodes, 3)

	loads := l.Load()
	assert.InDelta(t, 2*loads[nodes[1].ID], loads[nodes[0].ID], 4, "loads %v", loads)
}

func TestComputeGateway(t *testing.T) {
	nodes := append(grid(3, 1, 1), layout.Node{ID: "gw", Zone: "z0"})

	l := compute(t, nil, nodes, 3)
	assert.Zero(t, l.Load()["gw"])
}

func TestComputeIdempotent(t *testing.T) {
	nodes := grid(3, 2, 2)
	l := compute(t, nil, nodes, 3, 6)

	again := compute(t, l, nodes, 3, 6)
	assert.Equal(t, l.Version+1, again.Version)
	assert.Equal(t, l.Slots, again.Slots)
}

func TestComputeStableOnAdd(t *testing.T) {
	nodes := grid(3, 2, 2)
	before := compute(t, nil, nodes, 3, 6)

	added := layout.Node{ID: "new", Zone: "z1", Rack: "r1", Capacity: tb}
	after := compute(t, before, append(slices.Clone(nodes), added), 3, 6)

	// The new node takes its share of its zone — each zone holds two of six
	// slots, now split five ways in z1 — and little else moves: every slot it
	// holds was moved to it, and spread may force a few more.
	got := after.Load()["new"]
	assert.InDelta(t, len(after.Slots)*2/5, got, 2)
	assert.LessOrEqual(t, moved(before, after), got+got/5)
}

func TestComputeStableOnRemove(t *testing.T) {
	nodes := grid(3, 2, 2)
	before := compute(t, nil, nodes, 3, 6)

	gone := nodes[4].ID
	after := compute(t, before, slices.Delete(slices.Clone(nodes), 4, 5), 3, 6)

	held := before.Load()[gone]
	assert.LessOrEqual(t, moved(before, after), held+held/5)

	for p := range before.Slots {
		for j, id := range before.Slots[p] {
			if id != gone {
				assert.Equal(t, id, after.Slots[p][j], "partition %d slot %d moved off a surviving node", p, j)
			}
		}
	}
}

func TestComputeNewZone(t *testing.T) {
	// Two zones hold the six slots three and three; a third zone takes two of
	// every six, and only those move.
	nodes := grid(2, 1, 4)
	before := compute(t, nil, nodes, 6)

	after := compute(t, before, slices.Concat(nodes, grid(3, 1, 4)[8:]), 6)

	zone, _ := after.MaxPerDomain(6)
	assert.Equal(t, 2, zone)
	assert.LessOrEqual(t, moved(before, after), len(after.Slots)*2+len(after.Slots)/5)
}

func TestComputePartitionsFixed(t *testing.T) {
	nodes := grid(3, 1, 1)

	l, err := layout.Compute(nil, nodes, layout.Options{Partitions: 16})
	require.NoError(t, err)
	require.Len(t, l.Slots, 16)

	next, err := layout.Compute(l, nodes, layout.Options{Partitions: 1024})
	require.NoError(t, err)
	assert.Len(t, next.Slots, 16, "the partition count is fixed at creation")
}

func TestComputeErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		nodes []layout.Node
		opts  layout.Options
	}{
		"too few nodes":       {grid(1, 1, 2), layout.Options{}},
		"gateways hold none":  {[]layout.Node{{ID: "a"}, {ID: "b"}, {ID: "c"}}, layout.Options{}},
		"not a power of two":  {grid(3, 1, 1), layout.Options{Partitions: 100}},
		"non-positive width":  {grid(3, 1, 1), layout.Options{Widths: []int{0, 3}}},
		"duplicate node":      {append(grid(3, 1, 1), grid(1, 1, 1)...), layout.Options{}},
		"empty node id":       {append(grid(3, 1, 1), layout.Node{Capacity: tb}), layout.Options{}},
		"width beyond nodes":  {grid(3, 1, 1), layout.Options{Widths: []int{3, 6}}},
		"too many partitions": {grid(3, 1, 1), layout.Options{Partitions: 1 << 17}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := layout.Compute(nil, tc.nodes, tc.opts)
			require.Error(t, err)
		})
	}
}

func TestComputeDeterministic(t *testing.T) {
	nodes := grid(3, 2, 3)
	shuffled := slices.Clone(nodes)
	slices.Reverse(shuffled)

	assert.Equal(t, compute(t, nil, nodes, 3, 6).Slots, compute(t, nil, shuffled, 3, 6).Slots,
		"node order must not matter")
}

func TestPartition(t *testing.T) {
	l := compute(t, nil, grid(3, 1, 1), 3)

	assert.Equal(t, l.Partition([]byte("bucket")), l.Partition([]byte("bucket")))

	hits := make([]int, len(l.Slots))
	for i := range 256 * 64 {
		hits[l.Partition(fmt.Appendf(nil, "key-%d", i))]++
	}

	for p, n := range hits {
		assert.InDelta(t, 64, n, 40, "partition %d", p)
	}
}

func BenchmarkCompute(b *testing.B) {
	nodes := grid(3, 4, 9) // 108 nodes

	for b.Loop() {
		_, err := layout.Compute(nil, nodes, layout.Options{Partitions: 1024, Widths: []int{3, 6}})
		require.NoError(b, err)
	}
}
