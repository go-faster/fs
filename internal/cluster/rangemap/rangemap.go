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

// Initial builds a map of n ranges over the whole key space, assigned round
// robin across nodes.
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
func Initial(n int, nodes []cluster.NodeID) (*Map, error) {
	if n < 1 {
		return nil, errors.Errorf("range count %d must be at least 1", n)
	}

	if len(nodes) == 0 {
		return nil, errors.New("no nodes to own ranges")
	}

	ranges := make([]Range, 0, n)

	for i := range n {
		r := Range{Owner: nodes[i%len(nodes)]}

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
