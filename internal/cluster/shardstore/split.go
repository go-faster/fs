package shardstore

import (
	"bytes"
	"cmp"
	"context"
	"slices"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// splitDescentDepth is the deepest a split point is refined.
//
// It has to comfortably exceed the prefix every key in a bucket shares, because
// that prefix is where the descent starts spending its depth: keys are 'o' +
// bucket + NUL + key, so a range holding one bucket has no divergence at all
// until the bucket name is behind it. A cap of eight looked reasonable and
// produced boundaries that had not yet reached the data — the split was 99.9/0.1
// and the test that measured the halves is what said so.
//
// Each byte costs a binary search over 256 values, so this is at most ~256
// estimates, all metadata arithmetic and no I/O. Depth is cheap; being wrong
// about where the data is, is not.
const splitDescentDepth = 32

// splitTolerance is how uneven a split may be before the descent stops looking.
//
// A tenth. Chasing an exact halving costs depth for a boundary that will be
// wrong within minutes anyway — the data keeps arriving — and a split that lands
// at 45/55 has done its job, which is to bound the size of a range rather than
// to be beautiful.
const splitTolerance = 0.1

// RangeSize is what a range holds, as pebble accounts for it.
type RangeSize struct {
	// Bytes is the estimated on-disk size of the range.
	//
	// An estimate rather than a count, and deliberately: it comes from table
	// metadata, so it costs no I/O and can be asked on every reconciliation
	// pass. A count would need a scan, which is the thing a split exists to
	// keep bounded.
	Bytes uint64
}

// Measurement is what a range looks like to whoever owns it: how much it holds
// and where it would divide.
//
// The two travel together because they are read from the same table metadata,
// and a caller with the size would ask for the point next anyway — on a map
// that may have changed in between.
type Measurement struct {
	// Bytes is the range's estimated size.
	Bytes uint64
	// SplitAt is where it would divide, or empty when there is no such point:
	// a range with nothing in it, or one whose boundary is already deeper than
	// the descent can reach.
	SplitAt string
	// AccessedAt is where the range's *recent writes* divide, empty when the
	// owner has not seen enough of them to say.
	//
	// The other median, and for the workload this phase exists for the only
	// useful one. Sequential keys all land at the top of the key space, so
	// SplitAt — which divides the bytes — sits far below the traffic, and a
	// split there halves the storage and leaves the upper half taking every
	// write. This divides the traffic instead.
	AccessedAt string
	// Writes is how many writes a second the owner is taking for the range.
	//
	// Size says how much a range costs to move; this says how much is gained by
	// moving it. They are different questions and a sequential-key workload
	// makes them opposite ones — every write lands at the top of the key space,
	// so the newest range is the smallest and the busiest.
	Writes float64
}

// Measure reports a range's size and split point together.
func (s *Shard) Measure(_ context.Context, r rangemap.Range) (Measurement, error) {
	size, err := s.RangeSize(r)
	if err != nil {
		return Measurement{}, err
	}

	at, _, err := s.SplitPoint(r)
	if err != nil {
		return Measurement{}, err
	}

	return Measurement{
		Bytes:      size.Bytes,
		SplitAt:    at,
		AccessedAt: s.AccessPoint(r),
		Writes:     s.WriteRate(r),
	}, nil
}

// RangeSize estimates what a range holds.
func (s *Shard) RangeSize(r rangemap.Range) (RangeSize, error) {
	size, err := s.usageBetween([]byte(r.Start), upperOf(r))
	if err != nil {
		return RangeSize{}, err
	}

	return RangeSize{Bytes: size}, nil
}

// SplitPoint proposes a key dividing a range's data roughly in half, or reports
// that none was found.
//
// # Why not the midpoint of the key space
//
// Because that is the presplit the plane already has, and real data is skewed:
// a deployment with one large bucket puts everything in one range whatever the
// boundaries. What is wanted is a key dividing the **data**, which means a
// median, which means measurement.
//
// # Descent, not sampling and not a scan
//
// The key space is descended one byte at a time. At each depth a binary search
// over the 256 possible values finds the byte the median key has there — the
// largest one still leaving at most half the range below it — using pebble's
// size estimate, which reads table metadata and touches no data at all. The
// whole descent is at most ~256 estimates and no I/O.
//
// The alternatives are worse in ways that matter here. Scanning to find the
// exact median reads the whole range, which is the cost a split exists to
// avoid. Reservoir-sampling keys as they are written puts a lock and a random
// number on every put — the hot path — to answer a question asked once per
// split.
//
// Boundaries are as short as the data allows rather than short: the descent
// stops as soon as it is close enough, but keys are 'o' + bucket + NUL + key,
// so a boundary dividing one bucket necessarily carries its name. Every
// boundary is a key stored in etcd for the life of the cluster, which is what
// the depth cap is for.
//
// # Reports false rather than guessing
//
// A range with nothing in it, or one whose data all sits under a single key,
// has no split point — and inventing one would produce an empty half that
// splits again next pass, forever.
func (s *Shard) SplitPoint(r rangemap.Range) (at string, found bool, err error) {
	lower, upper := []byte(r.Start), upperOf(r)

	total, err := s.usageBetween(lower, upper)
	if err != nil {
		return "", false, err
	}

	if total == 0 {
		return "", false, nil
	}

	half := total / 2

	// The prefix is always a lower bound of the median: at each depth it is
	// extended by the largest byte that still leaves at most half the range
	// below it, so the descent moves *into* the data rather than past it. The
	// first version took the smallest byte that crossed half, which walks off
	// the top of the range on the very first step and never comes back.
	best := []byte(nil)
	prefix := []byte(nil)

	for range splitDescentDepth {
		b, err := s.descendByte(lower, prefix, half)
		if err != nil {
			return "", false, err
		}

		prefix = append(prefix, b)

		candidate := bytes.Clone(prefix)
		if !dividesRange(candidate, lower, upper) {
			// Still inside the prefix every key here shares, so this candidate
			// sorts at or below the range start. Keep descending: the bytes
			// that diverge are further down.
			continue
		}

		at, err := s.usageBetween(lower, candidate)
		if err != nil {
			return "", false, err
		}

		best = candidate

		if closeEnough(at, total) {
			break
		}
	}

	if best == nil {
		return "", false, nil
	}

	return string(best), true, nil
}

// descendByte returns the largest byte b for which at most `half` of the
// range's bytes sort below prefix||b — the byte the median key has at this
// depth.
//
// Largest-that-fits rather than smallest-that-crosses. The two differ by one
// step and the difference is the whole algorithm: the crossing byte names the
// interval *after* the one holding the median, so descending into it descends
// into empty space, and every further depth refines a prefix with no data under
// it.
//
// A binary search rather than a sweep, because bytes below a prefix is monotonic
// in the prefix: the 256 candidates are ordered, so eight probes suffice.
//
// It needs no upper bound of its own. A probe above the range measures more
// than half of it — half being derived from the range's own total — so the
// search rejects it without being told to, and a probe below measures nothing.
func (s *Shard) descendByte(lower, prefix []byte, half uint64) (byte, error) {
	lo, hi := 0, 255

	for lo < hi {
		mid := (lo + hi + 1) / 2

		candidate := append(bytes.Clone(prefix), byte(mid))

		below, err := s.usageBetween(lower, candidate)
		if err != nil {
			return 0, err
		}

		if below <= half {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	//nolint:gosec // lo is bounded to [0, 255] by the loop above.
	return byte(lo), nil
}

// usageBetween is pebble's estimate for a key interval, with an empty upper
// bound meaning "to the end of the key space".
func (s *Shard) usageBetween(from, to []byte) (uint64, error) {
	if to != nil && bytes.Compare(from, to) >= 0 {
		return 0, nil
	}

	n, err := s.db.EstimateDiskUsage(from, orMax(to))
	if err != nil {
		return 0, errors.Wrap(err, "estimate range size")
	}

	return n, nil
}

// dividesRange reports whether a candidate would produce two non-empty halves.
func dividesRange(candidate, lower, upper []byte) bool {
	if bytes.Compare(candidate, lower) <= 0 {
		return false
	}

	return upper == nil || bytes.Compare(candidate, upper) < 0
}

// closeEnough reports whether a candidate divides the range within tolerance.
func closeEnough(at, total uint64) bool {
	if total == 0 {
		return true
	}

	share := float64(at) / float64(total)

	return share > 0.5-splitTolerance && share < 0.5+splitTolerance
}

// upperOf is a range's exclusive upper bound, or nil for the last range.
func upperOf(r rangemap.Range) []byte {
	if r.End == "" {
		return nil
	}

	return []byte(r.End)
}

// maxKey is the upper bound standing in for "the end of the key space".
//
// pebble's estimate wants a concrete bound, and the store's own keys are all
// prefixed with a small ASCII byte, so a single 0xff byte sorts above every one
// of them.
var maxKey = []byte{0xff}

// orMax substitutes the end of the key space for a nil bound.
func orMax(to []byte) []byte {
	if to == nil {
		return maxKey
	}

	return to
}

// SplitPolicy is when a range is too large to leave alone.
type SplitPolicy struct {
	// MaxBytes is the size above which a range is split. Zero means
	// DefaultMaxRangeBytes.
	//
	// The number that matters is not the size itself but what a *move* of one
	// range costs, because that is the unit a rebalance shifts and a failover
	// rebuilds. A range so large that moving it takes hours is one the cluster
	// cannot rebalance, however even the partition looks on paper.
	MaxBytes uint64

	// MaxSplitsPerPass bounds how many ranges are split in one reconciliation.
	// Zero means DefaultMaxSplitsPerPass.
	//
	// Splitting is cheap — it moves nothing — but each split is a control-plane
	// write and a map revision every node in the cluster will refetch. A
	// cluster that has just been switched on, with every range over the
	// threshold, would otherwise publish thousands of revisions in one pass and
	// spend the next minutes serving nothing but map reads.
	MaxSplitsPerPass int
}

// Split policy defaults.
const (
	// DefaultMaxRangeBytes is the size above which a range splits.
	//
	// Chosen from what a move costs rather than from what pebble prefers: at a
	// gigabyte, handing a range to another node is seconds of transfer, so a
	// rebalance is something a cluster can do continuously rather than plan.
	DefaultMaxRangeBytes = 1 << 30

	// DefaultMaxSplitsPerPass bounds the map churn one pass can cause.
	DefaultMaxSplitsPerPass = 8
)

func (p SplitPolicy) maxBytes() uint64 {
	if p.MaxBytes == 0 {
		return DefaultMaxRangeBytes
	}

	return p.MaxBytes
}

func (p SplitPolicy) maxPerPass() int {
	if p.MaxSplitsPerPass <= 0 {
		return DefaultMaxSplitsPerPass
	}

	return p.MaxSplitsPerPass
}

// SplitPlan is one boundary worth creating, and which median chose it.
//
// Which one is not bookkeeping. A split by stored size and a split by traffic
// put the boundary in very different places on the same range, and an operator
// looking at a partition that keeps getting finer needs to know which question
// the cluster was answering.
type SplitPlan struct {
	// At is the boundary to create.
	At string
	// ByAccess reports that the boundary divides the range's recent writes
	// rather than its stored bytes.
	ByAccess bool
	// Bytes is what the range held when it was measured.
	Bytes uint64
}

// PlanSplits returns the boundaries worth creating, largest range first, from a
// survey already taken.
//
// # The accessed median is preferred when there is one
//
// A range is split because it is too large, and where it divides decides
// whether that helped. Dividing the bytes halves the storage; on a
// sequential-key workload it leaves the upper half taking every write, so the
// next pass splits that half, and the one after splits half of it — the
// boundary walks toward the hot end one control-plane write at a time. Dividing
// the traffic puts it near the hot end on the first split.
//
// The stored median remains the fallback, and it is not a lesser one: a range
// nobody is writing to has no accessed median to compute, and its split point
// is exactly the question of where its bytes are.
//
// Pure, and taking the measurements rather than a way to make them, because the
// pass that splits also rebalances and both want the same numbers. Two planners
// measuring separately would ask every owner twice every five seconds, and could
// decide two things from two different readings of one cluster.
//
// # Largest first, and bounded
//
// A pass splits the ranges that most need it rather than the ones that sort
// earliest, because the cap is reached long before the work is done on a
// cluster that has just been switched on — and finishing the alphabet while the
// one enormous range waits is the wrong order to make progress in.
func PlanSplits(m *rangemap.Map, survey Survey, policy SplitPolicy) []SplitPlan {
	var found []SplitPlan

	for i := range m.Ranges {
		if i >= len(survey) || survey[i] == nil {
			// An owner that could not be reached is skipped, not guessed at.
			// Splitting on a stale size would be a map edit made from a number
			// nobody currently stands behind, and an oversized range is a slow
			// problem: it is measured again next pass and nothing is lost.
			continue
		}

		got := survey[i]

		if got.Bytes <= policy.maxBytes() {
			continue
		}

		// Size decides *whether* to split; the accessed median decides where,
		// when the owner has seen enough writes to have one.
		plan := SplitPlan{At: got.AccessedAt, ByAccess: true, Bytes: got.Bytes}
		if plan.At == "" {
			plan = SplitPlan{At: got.SplitAt, Bytes: got.Bytes}
		}

		if plan.At == "" {
			continue
		}

		found = append(found, plan)
	}

	// Sorted by size alone. What the plan owes is that two controllers racing
	// the same pass produce the same answer from the same measurements, and any
	// deterministic sort gives that — an explicit tie-break on the boundary
	// would look like it was carrying the property and would not be.
	//
	// Stable rather than not, because it costs nothing here and leaves equal
	// ranges in map order, which is key order. That is a nicety, not the
	// guarantee.
	slices.SortStableFunc(found, func(a, b SplitPlan) int {
		return cmp.Compare(b.Bytes, a.Bytes)
	})

	if len(found) > policy.maxPerPass() {
		found = found[:policy.maxPerPass()]
	}

	return found
}
