package shardstore

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// loadHalfLife is how long it takes an idle range's measured rate to fall by
// half.
//
// Long enough that a burst is still visible a minute later, because that is the
// timescale a move runs on: a range picked for its load and then copied for
// several minutes must still be the right choice when the copy finishes. Short
// enough that a range that has genuinely gone quiet stops being chosen.
const loadHalfLife = 2 * time.Minute

// minLoadSample is the shortest interval that produces a rate.
//
// Below it the divisor is small enough that one write reads as an enormous rate,
// and a controller measuring every few seconds would see a range flare and
// subside on nothing. Samples shorter than this accumulate instead.
const minLoadSample = time.Second

// rangeLoad is how much traffic one range is taking, as a rate that decays.
//
// # Why a rate and not a count
//
// A count says what a range has ever had; placement is a question about now. A
// range that took a million writes last week and none since is not the one to
// move, and a counter cannot tell the two apart.
//
// # Why it is not persisted
//
// Load is a statement about a running process. Carrying it across a restart
// would describe traffic served by a node that no longer exists — and the safe
// direction is obvious: a freshly started node reads as quiet, so it is not
// chosen as the source of a move until it has been observed being busy. The
// mistake in the other direction is moving a range on evidence from before the
// reboot.
type rangeLoad struct {
	// writes is incremented on the write path, so it is an atomic rather than
	// something guarded by the shard's lock — the observation runs under a read
	// lock and several writers hold one at once.
	writes atomic.Uint64

	mu sync.Mutex
	// sampled is when the interval now being counted began, and rate the last
	// result. Both are touched only by a reader, which is the reconciliation
	// pass — except sampled, which is set once when the range is adopted.
	sampled time.Time
	rate    float64
	// primed reports that a real interval has been folded. The first one is
	// taken as the rate rather than blended into a zero that never happened: an
	// average has nothing to average with until it has a second sample, and
	// starting from zero would report a busy range as a third as busy for its
	// first few minutes — which is exactly when a new range is at its hottest.
	primed bool
}

// observe counts a write against the range holding a key.
//
// Called after the write succeeded, so a refused write does not make a range
// look busy. The scan is over the ranges this node owns, which is dozens at
// most — the same walk ownership already does, and the reason this is not an
// index.
func (s *Shard) observe(key []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, r := range s.owned {
		if !r.Contains(string(key)) {
			continue
		}

		if l := s.loads[idOf(r)]; l != nil {
			l.writes.Add(1)
		}

		return
	}
}

// WriteRate reports how many writes a second this shard is taking for a range.
//
// Folding happens here rather than on a timer: the rate is only ever wanted by
// whoever is about to decide something with it, and a background goroutine per
// shard to maintain a number nobody reads is a cost with no reader.
//
// The fold is time-weighted, so an irregular reader gets the same answer a
// regular one would. A pass that skipped a minute contributes a minute's worth
// of decay, not one sample's.
func (s *Shard) WriteRate(r rangemap.Range) float64 {
	s.mu.RLock()
	l := s.loads[idOf(r)]
	s.mu.RUnlock()

	if l == nil {
		return 0
	}

	return l.fold(s.now())
}

// fold turns the writes counted since the last read into a rate, and decays what
// was there before.
func (l *rangeLoad) fold(now time.Time) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	elapsed := now.Sub(l.sampled)
	if elapsed < minLoadSample {
		// Too short to divide by. The writes stay counted and fold into the next
		// read, so nothing is lost by asking often.
		return l.rate
	}

	instant := float64(l.writes.Swap(0)) / elapsed.Seconds()
	l.sampled = now

	if !l.primed {
		l.primed = true
		l.rate = instant

		return l.rate
	}

	// Time-weighted, so the weight of a sample is how much of the half-life it
	// covered. A sample taken after two half-lives all but replaces what was
	// there; one taken after a moment barely moves it.
	alpha := 1 - math.Exp2(-elapsed.Seconds()/loadHalfLife.Seconds())

	l.rate += (instant - l.rate) * alpha

	return l.rate
}

// trackLoad rebuilds the per-range counters for the ranges this shard owns. The
// caller holds the lock.
//
// Entries are carried over by identity, so a map change that leaves a range
// alone leaves its measured rate alone with it — a cluster that splits a range
// somewhere else must not reset the load of every other range, which would make
// every split look like a lull.
//
// A range that is new to this node starts with no history, including the halves
// of a split — it measures what it takes from the moment it is adopted, and
// nothing before. That is the safe direction: until it has been observed for an
// interval it reads as quiet and is not chosen as the source of a move, which
// costs a decision deferred by a sample. Inheriting instead would move a range
// on a rate belonging to a range that no longer exists.
func (s *Shard) trackLoad(owned []rangemap.Range) {
	next := make(map[rangeID]*rangeLoad, len(owned))

	for _, r := range owned {
		id := idOf(r)

		if l, ok := s.loads[id]; ok {
			next[id] = l

			continue
		}

		// The interval starts when the range is adopted, not when it is first
		// read. Otherwise the writes taken before anyone asked would either be
		// discarded — losing exactly the burst that a new range is adopted into
		// — or divided by an interval nobody recorded.
		next[id] = &rangeLoad{sampled: s.now()}
	}

	s.loads = next
}

// now is the shard's clock, so a test can measure a rate without waiting for
// one.
func (s *Shard) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}

	return time.Now()
}

// WithShardClock replaces the clock the load rates are folded against.
//
// For tests only. A rate is a quantity per unit of wall time, and a test that
// had to spend that time to observe one would either be slow or be measuring
// something too brief to divide by.
func WithShardClock(now func() time.Time) ShardOption {
	return func(s *Shard) { s.clock = now }
}
