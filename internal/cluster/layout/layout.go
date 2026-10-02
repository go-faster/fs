// Package layout assigns the cluster's partitions to nodes.
//
// The key space is cut into a fixed number of partitions, chosen when the
// cluster is created. Every partition has the same number of ordered slots,
// each naming a distinct node: replicated data uses the first three, an
// erasure scheme ec:k,m the first k+m, with shard i in slot i. Every node holds
// a copy of the layout, so finding where a key lives is a hash and an index,
// with no coordinator to ask.
//
// Computing a layout is pure: no I/O, no randomness, the same inputs give the
// same layout. Three properties drive it, in this order:
//
//   - Spread. Within each configured width, no failure domain holds more than
//     its even share of a partition's slots — zones if there are several, else
//     racks. That is what lets a scheme survive the loss of a whole domain.
//   - Stability. A recomputed layout keeps every slot whose node is still
//     valid, so a node change moves only the data it has to. Slots are
//     positions, not a set: a recomputed order would renumber shards.
//   - Balance. Nodes hold slots in proportion to their capacity.
package layout

import (
	"cmp"
	"hash/fnv"
	"math"
	"slices"
	"sort"

	"github.com/go-faster/errors"
)

// DefaultPartitions is the partition count of a new cluster. It bounds how
// evenly data spreads: 256 partitions of three slots balance well up to about a
// hundred nodes. Create a larger cluster with more.
const DefaultPartitions = 256

// NodeID identifies a cluster member.
type NodeID string

// Node is a cluster member's role in the layout.
type Node struct {
	ID NodeID `json:"id"`
	// Zone is the coarsest failure domain, e.g. a datacenter.
	Zone string `json:"zone,omitempty"`
	// Rack is the failure domain within a zone.
	Rack string `json:"rack,omitempty"`
	// Capacity is the node's share of data, in bytes. Zero makes it a gateway
	// that serves requests and holds no slots.
	Capacity uint64 `json:"capacity"`
}

// Layout is one version of the cluster's partition assignment.
type Layout struct {
	// Version increases with every applied change; nodes adopt the highest.
	Version uint64 `json:"version"`
	// Widths are the slot prefixes the layout spreads for, ascending. The last
	// is the slot count of every partition.
	Widths []int `json:"widths"`
	// Nodes are the members, sorted by ID.
	Nodes []Node `json:"nodes"`
	// Slots[p] is partition p's ordered node list.
	Slots [][]NodeID `json:"slots"`
}

// Options configures Compute.
type Options struct {
	// Partitions is the partition count, a power of two. It is fixed when the
	// cluster is created and ignored afterwards. Zero means DefaultPartitions.
	Partitions int
	// Widths are the slot prefixes to spread for: 3 for replicated data, plus
	// k+m for each erasure scheme in use. Zero means [3].
	Widths []int
}

// Compute returns the layout for nodes that follows prev, or a fresh one when
// prev is nil.
func Compute(prev *Layout, nodes []Node, opts Options) (*Layout, error) {
	partitions := cmp.Or(opts.Partitions, DefaultPartitions)
	if prev != nil {
		partitions = len(prev.Slots)
	}

	if partitions <= 0 || partitions > 1<<16 || partitions&(partitions-1) != 0 {
		return nil, errors.Errorf("partitions must be a power of two up to 65536, got %d", partitions)
	}

	widths := slices.Clone(opts.Widths)
	if len(widths) == 0 {
		widths = []int{3}
	}

	slices.Sort(widths)
	widths = slices.Compact(widths)

	if widths[0] <= 0 {
		return nil, errors.Errorf("widths must be positive, got %v", widths)
	}

	nodes = slices.Clone(nodes)
	slices.SortFunc(nodes, func(a, b Node) int { return cmp.Compare(a.ID, b.ID) })

	c, err := newComputation(nodes, partitions, widths)
	if err != nil {
		return nil, err
	}

	l := &Layout{Version: 1, Widths: widths, Nodes: nodes}
	if prev != nil {
		l.Version = prev.Version + 1
	}

	// Nothing that placement depends on changed: keep every slot. Recomputing
	// could still move data where spread forced a node past its balance limit.
	if prev != nil && slices.Equal(prev.Nodes, nodes) && slices.Equal(prev.Widths, widths) {
		l.Slots = clone(prev.Slots)

		return l, nil
	}

	c.inherit(prev)
	c.enforceSpread()
	c.shedOverload()
	c.fill()

	l.Slots = c.slots

	return l, nil
}

// Validate checks the structure every layout has, for one that arrived from
// elsewhere: a power-of-two partition count, ascending widths, and every slot
// filled by a distinct member that holds data.
func (l *Layout) Validate() error {
	n := len(l.Slots)
	if n == 0 || n > 1<<16 || n&(n-1) != 0 {
		return errors.Errorf("partition count %d is not a power of two up to 65536", n)
	}

	if len(l.Widths) == 0 || l.Widths[0] <= 0 || !slices.IsSorted(l.Widths) {
		return errors.Errorf("bad widths %v", l.Widths)
	}

	data := make(map[NodeID]bool, len(l.Nodes))
	for _, node := range l.Nodes {
		data[node.ID] = node.Capacity > 0
	}

	for p, slots := range l.Slots {
		if len(slots) != l.Width() {
			return errors.Errorf("partition %d has %d slots, want %d", p, len(slots), l.Width())
		}

		for j, id := range slots {
			if !data[id] {
				return errors.Errorf("partition %d slot %d: %q is not a member with capacity", p, j, id)
			}

			if slices.Contains(slots[:j], id) {
				return errors.Errorf("partition %d: %q holds two slots", p, id)
			}
		}
	}

	return nil
}

// Width is the slot count of every partition.
func (l *Layout) Width() int { return l.Widths[len(l.Widths)-1] }

// Partition maps a key to its partition.
func (l *Layout) Partition(key []byte) int {
	h := fnv.New64a()
	_, _ = h.Write(key)

	return int(mix(h.Sum64()) % uint64(len(l.Slots))) //nolint:gosec // Below the partition count, at most 1<<16.
}

// Moved counts the slots whose node differs between two layouts of the same
// shape: the data a change sends over the network.
func Moved(a, b *Layout) int {
	n := 0

	for p := range min(len(a.Slots), len(b.Slots)) {
		for j := range min(len(a.Slots[p]), len(b.Slots[p])) {
			if a.Slots[p][j] != b.Slots[p][j] {
				n++
			}
		}
	}

	return n
}

// Load reports how many slots each node holds.
func (l *Layout) Load() map[NodeID]int {
	load := make(map[NodeID]int, len(l.Nodes))
	for _, slots := range l.Slots {
		for _, id := range slots {
			load[id]++
		}
	}

	return load
}

// MaxPerDomain reports the most slots any single zone and any single rack
// holds within the first width slots of a partition, over all partitions. A
// scheme over width slots survives losing a whole zone while zone is at most
// its tolerated loss: m for ec:k,m, one for three replicas written at quorum
// two.
func (l *Layout) MaxPerDomain(width int) (zone, rack int) {
	byID := make(map[NodeID]Node, len(l.Nodes))
	for _, n := range l.Nodes {
		byID[n.ID] = n
	}

	for _, slots := range l.Slots {
		zones := map[string]int{}
		racks := map[string]int{}

		for _, id := range slots[:min(width, len(slots))] {
			n := byID[id]
			zones[n.Zone]++
			racks[n.Zone+"\x00"+n.Rack]++
		}

		for _, v := range zones {
			zone = max(zone, v)
		}

		for _, v := range racks {
			rack = max(rack, v)
		}
	}

	return zone, rack
}

// computation is the working state of one Compute call.
type computation struct {
	widths []int
	width  int
	slots  [][]NodeID

	data   []Node // nodes with capacity, sorted by ID
	byID   map[NodeID]Node
	domain func(Node) string // the failure domain spread is enforced over
	sub    func(Node) string // the finer domain spread is preferred over
	caps   map[int]int       // per width, the most slots one domain may hold
	load   map[NodeID]int
	limit  map[NodeID]int // most slots a node should hold
	target map[NodeID]float64
}

func newComputation(nodes []Node, partitions int, widths []int) (*computation, error) {
	c := &computation{
		widths: widths,
		width:  widths[len(widths)-1],
		byID:   make(map[NodeID]Node, len(nodes)),
		load:   map[NodeID]int{},
		limit:  map[NodeID]int{},
		target: map[NodeID]float64{},
	}

	zones := map[string]struct{}{}
	racks := map[string]struct{}{}

	for _, n := range nodes {
		if n.ID == "" {
			return nil, errors.New("node with empty id")
		}

		if _, dup := c.byID[n.ID]; dup {
			return nil, errors.Errorf("duplicate node %q", n.ID)
		}

		c.byID[n.ID] = n

		if n.Capacity == 0 {
			continue
		}

		c.data = append(c.data, n)
		zones[n.Zone] = struct{}{}
		racks[n.Zone+"\x00"+n.Rack] = struct{}{}
	}

	if len(c.data) < c.width {
		return nil, errors.Errorf("%d slots per partition need at least %d nodes with capacity, have %d",
			c.width, c.width, len(c.data))
	}

	switch {
	case len(zones) > 1:
		c.domain = func(n Node) string { return n.Zone }
		c.sub = func(n Node) string { return n.Rack }
	case len(racks) > 1:
		c.domain = func(n Node) string { return n.Rack }
		c.sub = func(Node) string { return "" }
	default:
		// One domain: nodes are the only spread there is, and slots of a
		// partition are always on distinct nodes.
		c.domain = func(n Node) string { return string(n.ID) }
		c.sub = func(Node) string { return "" }
	}

	size := map[string]int{}
	for _, n := range c.data {
		size[c.domain(n)]++
	}

	c.caps = make(map[int]int, len(widths))
	for _, w := range widths {
		c.caps[w] = tightestCap(size, w)
	}

	c.targets(partitions, size)

	c.slots = make([][]NodeID, partitions)
	for p := range c.slots {
		c.slots[p] = make([]NodeID, c.width)
	}

	return c, nil
}

// targets sets how many slots each node should hold: its capacity share,
// first between domains and then within each, never more than the spread lets
// a domain hold or more than one slot per partition per node.
func (c *computation) targets(partitions int, size map[string]int) {
	capOf := map[string]float64{}
	for _, n := range c.data {
		capOf[c.domain(n)] += float64(n.Capacity)
	}

	domains := make([]string, 0, len(capOf))
	for d := range capOf {
		domains = append(domains, d)
	}

	slices.Sort(domains)

	weight := make([]float64, len(domains))
	ceiling := make([]float64, len(domains))

	for i, d := range domains {
		weight[i] = capOf[d]
		ceiling[i] = float64(partitions * min(c.cap(c.width), size[d]))
	}

	share := waterfill(float64(partitions*c.width), weight, ceiling)

	for i, d := range domains {
		var members []Node

		for _, n := range c.data {
			if c.domain(n) == d {
				members = append(members, n)
			}
		}

		weight := make([]float64, len(members))
		ceiling := make([]float64, len(members))

		for j, n := range members {
			weight[j] = float64(n.Capacity)
			ceiling[j] = float64(partitions)
		}

		for j, t := range waterfill(share[i], weight, ceiling) {
			c.target[members[j].ID] = t
			c.limit[members[j].ID] = int(math.Ceil(t))
		}
	}
}

// waterfill splits total in proportion to weight without giving any entry
// more than its ceiling: an entry that would exceed it is held there and the
// rest is split again among the others.
func waterfill(total float64, weight, ceiling []float64) []float64 {
	out := make([]float64, len(weight))
	held := make([]bool, len(weight))

	for {
		rest, sum := total, 0.0

		for i := range weight {
			if held[i] {
				rest -= out[i]
			} else {
				sum += weight[i]
			}
		}

		changed := false

		for i := range weight {
			if held[i] {
				continue
			}

			out[i] = rest * weight[i] / sum
			if out[i] > ceiling[i] {
				out[i], held[i], changed = ceiling[i], true, true
			}
		}

		if !changed {
			return out
		}
	}
}

// tightestCap is the smallest per-domain cap that still fills width slots
// on distinct nodes, given how many nodes each domain has. It is the even share
// when every domain has enough nodes; a small domain cannot take its share, so
// the others must take more.
func tightestCap(size map[string]int, width int) int {
	for c := 1; ; c++ {
		total := 0
		for _, n := range size {
			total += min(c, n)
		}

		if total >= width {
			return c
		}
	}
}

func (c *computation) cap(width int) int { return c.caps[width] }

// inherit copies prev's slots whose node still holds data.
func (c *computation) inherit(prev *Layout) {
	if prev == nil {
		return
	}

	for p, slots := range prev.Slots {
		for j, id := range slots[:min(len(slots), c.width)] {
			if n, ok := c.byID[id]; ok && n.Capacity > 0 {
				c.slots[p][j] = id
				c.load[id]++
			}
		}
	}
}

// enforceSpread vacates slots that put more than a domain's share of a prefix
// in one domain — the topology changed under them, e.g. a zone was added.
func (c *computation) enforceSpread() {
	for p, slots := range c.slots {
		for _, w := range c.widths {
			for {
				over := c.overfull(slots[:w], w)
				if over < 0 {
					break
				}

				c.vacate(p, over)
			}
		}
	}
}

// overfull returns the last slot of prefix whose domain exceeds its share of
// width, or -1.
func (c *computation) overfull(prefix []NodeID, width int) int {
	count := map[string]int{}

	for _, id := range prefix {
		if id != "" {
			count[c.domain(c.byID[id])]++
		}
	}

	for j := len(prefix) - 1; j >= 0; j-- {
		if id := prefix[j]; id != "" && count[c.domain(c.byID[id])] > c.cap(width) {
			return j
		}
	}

	return -1
}

// shedOverload vacates slots of nodes holding more than their limit, the ones
// with the weakest affinity first, so a new or grown node has slots to take.
// It prefers partitions with no vacancy yet: a node can take only one slot of
// a partition, so two vacancies in one would hand the second straight back to
// an overloaded node.
func (c *computation) shedOverload() {
	type slot struct{ p, j int }

	held := map[NodeID][]slot{}

	for p, slots := range c.slots {
		for j, id := range slots {
			if id != "" {
				held[id] = append(held[id], slot{p, j})
			}
		}
	}

	vacant := map[int]bool{}

	for _, n := range c.data {
		excess := c.load[n.ID] - c.limit[n.ID]
		if excess <= 0 {
			continue
		}

		s := held[n.ID]
		sort.SliceStable(s, func(a, b int) bool {
			return affinity(s[a].p, n.ID) < affinity(s[b].p, n.ID)
		})

		for _, crowded := range []bool{false, true} {
			for _, v := range s {
				if excess == 0 || vacant[v.p] != crowded || c.slots[v.p][v.j] != n.ID {
					continue
				}

				c.vacate(v.p, v.j)
				vacant[v.p] = true
				excess--
			}
		}
	}
}

func (c *computation) vacate(p, j int) {
	c.load[c.slots[p][j]]--
	c.slots[p][j] = ""
}

// fill assigns every empty slot.
func (c *computation) fill() {
	for p, slots := range c.slots {
		for j := range slots {
			if slots[j] == "" {
				slots[j] = c.best(p, j)
				c.load[slots[j]]++
			}
		}
	}
}

// best picks the node for slot j of partition p. A node already in the
// partition is never eligible. The rest are ranked by, in order: keeping every
// prefix within its domain share, staying under the node's limit, spreading
// over the finer domain, the load-to-capacity ratio, and affinity — the
// deterministic tiebreak that also keeps choices stable between versions.
//
// There is always a node: Compute checked there are at least width of them.
func (c *computation) best(p, j int) NodeID {
	slots := c.slots[p]

	used := map[NodeID]bool{}
	sub := map[string]int{}

	for _, id := range slots {
		if id != "" {
			used[id] = true
			sub[c.sub(c.byID[id])]++
		}
	}

	type rank struct {
		spread, over bool
		sub          int
		ratio        float64
		aff          uint64
	}

	better := func(a, b rank) bool {
		switch {
		case a.spread != b.spread:
			return a.spread
		case a.over != b.over:
			return !a.over
		case a.sub != b.sub:
			return a.sub < b.sub
		case a.ratio != b.ratio:
			return a.ratio < b.ratio
		default:
			return a.aff > b.aff
		}
	}

	var (
		winner NodeID
		top    rank
	)

	for _, n := range c.data {
		if used[n.ID] {
			continue
		}

		r := rank{
			spread: c.keepsSpread(slots, j, n),
			over:   c.load[n.ID] >= c.limit[n.ID],
			sub:    sub[c.sub(n)],
			ratio:  float64(c.load[n.ID]) / c.target[n.ID],
			aff:    affinity(p, n.ID),
		}

		if winner == "" || better(r, top) {
			winner, top = n.ID, r
		}
	}

	return winner
}

// keepsSpread reports whether putting n in slot j keeps every prefix that
// contains the slot within its domain share.
func (c *computation) keepsSpread(slots []NodeID, j int, n Node) bool {
	d := c.domain(n)

	for _, w := range c.widths {
		if j >= w {
			continue
		}

		count := 1

		for _, id := range slots[:w] {
			if id != "" && c.domain(c.byID[id]) == d {
				count++
			}
		}

		if count > c.cap(w) {
			return false
		}
	}

	return true
}

// affinity is a node's rendezvous hash for a partition: a stable, uniform
// preference that breaks ties the same way in every version.
func affinity(p int, id NodeID) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))

	return mix(h.Sum64() ^ uint64(p)*0x9e3779b97f4a7c15) //nolint:gosec // A partition index is never negative.
}

// mix is the splitmix64 finalizer. FNV alone avalanches poorly on short,
// similar inputs such as node IDs.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31

	return x
}

func clone(slots [][]NodeID) [][]NodeID {
	out := make([][]NodeID, len(slots))
	for p, s := range slots {
		out[p] = slices.Clone(s)
	}

	return out
}
