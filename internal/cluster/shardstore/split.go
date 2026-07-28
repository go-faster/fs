package shardstore

import (
	"bytes"

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
