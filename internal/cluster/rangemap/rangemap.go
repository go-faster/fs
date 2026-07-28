// Package rangemap is the partitioning of the metadata key space into ranges:
// which node owns which interval, and who follows it.
//
// It is the control-plane half of the sharded pebble metadata plane — the data
// half is the pebble instance on each node, holding the ranges it owns as key
// intervals. This package knows nothing about pebble and nothing about etcd;
// it is the map, its invariants, and the lookup. Persistence lives in
// internal/cluster/etcd.
//
// # Ranges partition, they do not merely describe
//
// The ranges are contiguous, non-overlapping, and cover the whole key space
// from the empty key to the end. That is an invariant rather than a
// convention: a gap is a key nothing owns, and a key nothing owns is an object
// that cannot be listed and a write that has nowhere to go. Validate enforces
// it, and every path that builds a map runs through it.
//
// # The key space is already listing order
//
// Keys are encoded 'o' + bucket + NUL + key, which is byte order, which is the
// order S3 lists in. So a range is just an interval [Start, End) and a listing
// is a forward walk of one — no secondary index, no re-sorting, and a page
// crosses a range boundary only when it happens to straddle one.
package rangemap

import (
	"slices"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore/keyspace"
)

// Range is one contiguous interval of the key space and who serves it.
type Range struct {
	// Start is the first key in the range, inclusive. The first range starts
	// at the empty key.
	Start string `json:"start"`
	// End is the first key past the range, exclusive. The last range ends at
	// the empty string, which means "to the end" — no key sorts above it, so
	// there is nothing to name.
	End string `json:"end"`
	// Owner serves reads and applies writes for this range.
	Owner cluster.NodeID `json:"owner"`
	// Followers receive the owner's log and stand ready to be promoted. Empty
	// is legal and means a lost owner costs a rebuild rather than a promotion
	// — see the sizing note in findings/SHARDED-PEBBLE.md §9.
	Followers []cluster.NodeID `json:"followers,omitempty"`
}

// Contains reports whether a key falls in this range.
func (r Range) Contains(key string) bool {
	if key < r.Start {
		return false
	}

	// An empty End is the last range: everything from Start onwards.
	return r.End == "" || key < r.End
}

// Map is a snapshot of the partitioning.
type Map struct {
	// Revision is the etcd revision this snapshot was read at, and it is what
	// makes routing self-correcting: a request carries the revision it was
	// routed with, and a node holding a newer one rejects it rather than
	// serving from a map it knows is stale.
	//
	// It is etcd's revision rather than an epoch this package maintains,
	// following cluster.Topology. A separately-kept counter is one more thing
	// to be wrong, and etcd's is monotonic for free.
	Revision int64
	// Ranges are sorted by Start, contiguous, and cover the whole key space.
	Ranges []Range
}

// Lookup returns the range holding a key.
//
// Total by construction: the ranges cover everything, so a valid map always
// has an answer. It reports false only for an empty map, which is a cluster
// whose metadata plane has not been initialized — a state worth distinguishing
// from "some key is unowned", which cannot happen.
func (m *Map) Lookup(key string) (Range, bool) {
	if len(m.Ranges) == 0 {
		return Range{}, false
	}

	// The last range whose Start is <= key. BinarySearch finds the first
	// element not less than the target, so step back one unless it matched
	// exactly.
	i, exact := slices.BinarySearchFunc(m.Ranges, key, func(r Range, k string) int {
		return strings.Compare(r.Start, k)
	})

	if !exact {
		i--
	}

	if i < 0 {
		return Range{}, false
	}

	return m.Ranges[i], true
}

// Owner returns the node serving a key.
func (m *Map) Owner(key string) (cluster.NodeID, bool) {
	r, ok := m.Lookup(key)
	if !ok {
		return "", false
	}

	return r.Owner, true
}

// RangesFor returns the ranges a node owns, in key order.
func (m *Map) RangesFor(node cluster.NodeID) []Range {
	var out []Range

	for _, r := range m.Ranges {
		if r.Owner == node {
			out = append(out, r)
		}
	}

	return out
}

// Validate checks the invariants every consumer relies on.
//
// Called on the way in and on the way out — after building a map and after
// loading one — because the failure it prevents is silent. A gap does not
// error at lookup time; it routes a key to the range before it, whose owner
// does not have it, and the object reads as absent.
func (m *Map) Validate() error {
	if len(m.Ranges) == 0 {
		return errors.New("range map is empty")
	}

	for i, r := range m.Ranges {
		if r.Owner == "" {
			return errors.Errorf("range %d [%q,%q) has no owner", i, r.Start, r.End)
		}

		if r.End != "" && r.Start >= r.End {
			return errors.Errorf("range %d is empty or inverted: [%q,%q)", i, r.Start, r.End)
		}

		if i == 0 {
			if r.Start != "" {
				return errors.Errorf("first range starts at %q, not the empty key", r.Start)
			}

			continue
		}

		if prev := m.Ranges[i-1]; prev.End != r.Start {
			return errors.Errorf("gap or overlap between range %d [%q,%q) and range %d [%q,%q)",
				i-1, prev.Start, prev.End, i, r.Start, r.End)
		}
	}

	if last := m.Ranges[len(m.Ranges)-1]; last.End != "" {
		return errors.Errorf("last range ends at %q, not the end of the key space", last.End)
	}

	return nil
}

// Split bounds. Bucket names are lowercase alphanumeric with hyphens and dots,
// so the second byte of a key — the first of the bucket name — lives in a
// narrow band. Splitting evenly across the whole byte range would put almost
// every key in one or two ranges.
const (
	splitLow  = '0'
	splitHigh = 'z'
)

// Initial builds a map of n ranges over the whole key space, owned round robin
// across nodes, each with replicas-1 followers placed away from its owner.
//
// The boundaries are a **presplit**, and a crude one: they spread evenly across
// the byte band bucket names start in, because with no data there is nothing
// better to go on. Real key distributions are skewed — a deployment with one
// large bucket puts everything in one range whatever the boundaries — so this
// is a starting partition, not a good one.
//
// That is why E4's split-on-size-and-load is load-bearing rather than an
// optimization: it is what turns this into a partition that reflects the data.
// Until then, a cluster whose ranges are badly balanced is behaving as
// designed, and the fix is E4 rather than a cleverer guess here.
func Initial(n int, nodes []cluster.Node, replicas int) (*Map, error) {
	if n < 1 {
		return nil, errors.Errorf("range count %d must be at least 1", n)
	}

	if len(nodes) == 0 {
		return nil, errors.New("no nodes to own ranges")
	}

	if replicas < 1 {
		return nil, errors.Errorf("replica count %d must be at least 1", replicas)
	}

	ranges := make([]Range, 0, n)

	for i := range n {
		owner := i % len(nodes)

		r := Range{
			Owner:     nodes[owner].ID,
			Followers: followersFor(nodes, owner, replicas-1),
		}

		if i > 0 {
			r.Start = boundary(i, n)
		}

		if i < n-1 {
			r.End = boundary(i+1, n)
		}

		ranges = append(ranges, r)
	}

	m := &Map{Ranges: ranges}
	if err := m.Validate(); err != nil {
		return nil, err
	}

	return m, nil
}

// followersFor picks want replicas for the range owned by nodes[owner].
//
// # Why followers are placed at all
//
// A range with no follower costs a cluster-wide walk of every disk when its
// owner is lost — the new owner starts empty, and nothing short of rebuilding
// makes it right. A range with one costs a metadata write. Handing a fresh
// plane no followers would make every node reboot the expensive kind of
// failure, so replicas=1 is a configuration rather than a default.
//
// # Distinct racks first
//
// A follower in the owner's rack survives the owner and not the rack, which is
// the failure the label exists to describe. So the ring is walked twice: once
// taking only nodes whose failure domain nothing has yet, then again filling
// from whatever is left. A cluster too small to spread degrades to same-rack
// replicas rather than to none — a follower that shares a rack still covers
// every single-node failure, which is most of them.
//
// A node with no rack is its own failure domain, per cluster.Node: two unlabeled
// nodes do not share one. Treating them as if they did would make the first
// pass skip every unlabeled node, so the followers on a cluster that never set
// the label would be whichever nodes happen to carry one — the opposite of
// spreading.
//
// # Walking the ring rather than hashing
//
// Rendezvous hashing exists to keep placement stable as membership changes, and
// nothing here re-places on a membership change: failover moves a range to a
// follower it already has. What is wanted instead is evenness, which offsetting
// around the node list gives exactly.
func followersFor(nodes []cluster.Node, owner, want int) []cluster.NodeID {
	if want <= 0 {
		return nil
	}

	// The walk starts one past the owner and stops before wrapping onto it, so
	// the owner is never a candidate and `used` need not exclude it. Its
	// failure domain does have to be excluded, which is the point of the first
	// pass.
	used := make(map[cluster.NodeID]bool, want)
	domains := map[string]bool{domainOf(nodes[owner]): true}

	var out []cluster.NodeID

	for pass := range 2 {
		for step := 1; step < len(nodes) && len(out) < want; step++ {
			c := nodes[(owner+step)%len(nodes)]

			if used[c.ID] {
				continue
			}

			if pass == 0 && domains[domainOf(c)] {
				continue
			}

			used[c.ID] = true
			domains[domainOf(c)] = true

			out = append(out, c.ID)
		}

		if len(out) >= want {
			break
		}
	}

	return out
}

// domainOf is a node's failure domain: its rack, or the node itself when it has
// no rack label — an unlabeled node shares fate with nobody.
func domainOf(n cluster.Node) string {
	if n.Rack == "" {
		return "node:" + string(n.ID)
	}

	return "rack:" + n.Rack
}

// boundary is the i-th of n split points across the bucket-name band.
//
// Clamped rather than trusted. With i < n the arithmetic already lands inside
// the band, but the bound is what the caller relies on — boundaries that
// escaped it would be out of order, which Validate reports as an inverted
// range and which no test would otherwise reach.
func boundary(i, n int) string {
	span := int(splitHigh) - int(splitLow) + 1

	b := min(max(int(splitLow)+i*span/n, int(splitLow)), int(splitHigh))

	//nolint:gosec // Clamped to [splitLow, splitHigh] on the line above, both byte constants.
	return string([]byte{keyspace.Object, byte(b)})
}

// ErrNotSplittable reports a split point that would not produce two ranges.
var ErrNotSplittable = errors.New("split point does not divide the range")

// Split replaces the range holding at with two ranges meeting there.
//
// # A split moves nothing
//
// This is the operation's defining property and the reason it can be run
// eagerly. A shard holds its ranges as key intervals in one pebble instance, so
// the two halves of a split are already on the right sides of the new boundary
// — every key was, before anyone decided where the boundary would be. Splitting
// is a map edit and nothing else, which is why it costs a control-plane write
// and no I/O at all.
//
// Moving a range is the expensive operation, and it is deliberately separate.
//
// # Both halves keep the owner and the followers
//
// A split that also reassigned would be a split and a move at once, and the
// move is what has a cost. Rebalancing decides where the halves go afterwards,
// with the split already durable — so an interrupted rebalance leaves a
// partition that is merely uneven rather than one that is half-formed.
//
// The returned map is a new value; the receiver is not modified, so a caller
// that fails to persist it has not changed what it is routing by.
func (m *Map) Split(at string) (*Map, error) {
	if at == "" {
		// The empty key starts the first range, so nothing sorts below it and
		// the left half would be empty.
		return nil, errors.Wrap(ErrNotSplittable, "the empty key is the start of the key space")
	}

	i, err := m.indexOf(at)
	if err != nil {
		return nil, err
	}

	r := m.Ranges[i]

	if at == r.Start {
		// Already a boundary. Reported rather than ignored: a caller splitting
		// at a key it computed from data is asking for two ranges, and silently
		// returning one would leave it believing it made progress.
		return nil, errors.Wrapf(ErrNotSplittable, "%q is already a range boundary", at)
	}

	out := &Map{Revision: m.Revision, Ranges: make([]Range, 0, len(m.Ranges)+1)}
	out.Ranges = append(out.Ranges, m.Ranges[:i]...)

	left := r
	left.End = at
	left.Followers = slices.Clone(r.Followers)

	right := r
	right.Start = at
	right.Followers = slices.Clone(r.Followers)

	out.Ranges = append(out.Ranges, left, right)
	out.Ranges = append(out.Ranges, m.Ranges[i+1:]...)

	if err := out.Validate(); err != nil {
		return nil, errors.Wrap(err, "split produced an invalid map")
	}

	return out, nil
}

// Merge dissolves a boundary, replacing the range starting at it and the one
// before with a single range spanning both.
//
// The exact inverse of Split, and named the same way for that reason: whatever
// key created a boundary removes it, so `m.Split(k)` followed by `Merge(k)`
// gives back the map that went in. An API where one named the boundary and the
// other named a range would be two operations that happen to be opposites
// rather than one operation and its undo.
//
// Like Split it moves nothing — but **only when the two halves share an owner**,
// which is why it refuses otherwise. Merging ranges on different nodes would
// make one node's data unreachable, since the surviving owner holds nothing for
// the half it never had, and nothing would report it: the merged range is a
// valid partition that simply answers "no such object".
//
// Merging exists because splits are one-way otherwise. A range split during a
// burst and then emptied leaves the partition permanently finer than the data
// warrants, and a map that only ever grows is one whose per-range overhead
// grows with it.
func (m *Map) Merge(at string) (*Map, error) {
	i, err := m.indexOf(at)
	if err != nil {
		return nil, err
	}

	if m.Ranges[i].Start != at {
		return nil, errors.Errorf("%q is not a range boundary", at)
	}

	if i == 0 {
		return nil, errors.New("the start of the key space is not a boundary that can be dissolved")
	}

	left, right := m.Ranges[i-1], m.Ranges[i]

	if left.Owner != right.Owner {
		return nil, errors.Errorf(
			"ranges at %q and %q are owned by %s and %s: merge them onto one node first",
			left.Start, right.Start, left.Owner, right.Owner)
	}

	out := &Map{Revision: m.Revision, Ranges: make([]Range, 0, len(m.Ranges)-1)}
	out.Ranges = append(out.Ranges, m.Ranges[:i-1]...)

	merged := left
	merged.End = right.End
	// The surviving follower set is the intersection: a node following only one
	// half holds only one half, and calling it a follower of the whole would
	// have a promotion serve the part it never received as empty.
	merged.Followers = intersect(left.Followers, right.Followers)

	out.Ranges = append(out.Ranges, merged)
	out.Ranges = append(out.Ranges, m.Ranges[i+1:]...)

	if err := out.Validate(); err != nil {
		return nil, errors.Wrap(err, "merge produced an invalid map")
	}

	return out, nil
}

// indexOf returns the position of the range holding a key.
func (m *Map) indexOf(key string) (int, error) {
	for i, r := range m.Ranges {
		if r.Contains(key) {
			return i, nil
		}
	}

	return 0, errors.Errorf("no range holds %q", key)
}

// intersect returns the nodes in both lists, in the first's order, or nil when
// there are none.
func intersect(a, b []cluster.NodeID) []cluster.NodeID {
	var out []cluster.NodeID

	for _, n := range a {
		if slices.Contains(b, n) {
			out = append(out, n)
		}
	}

	return out
}
