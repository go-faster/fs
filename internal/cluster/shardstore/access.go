package shardstore

import (
	"slices"
	"sync"

	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// accessSample is how many recent write keys a range keeps to find the point
// that divides its traffic.
//
// Small on purpose. The question is where writes are landing *now*, so the
// window wants to be short — and the sample is walked and sorted on every
// measurement, which the reconciliation pass does for every range every few
// seconds. A larger sample would buy precision the split point cannot use: the
// boundary only has to land in the busy region, not at its exact middle.
const accessSample = 256

// minAccessSample is how many keys must be held before a median is reported.
//
// Below it the sample says more about which handful of writes arrived than
// about where the traffic is, and a split placed on that would be a data move
// decided by noise. A range with too few is not refused a split — it falls back
// to dividing by stored size, which is what every range did before this.
const minAccessSample = 32

// maxAccessKey bounds a stored key.
//
// A truncated key is still a valid position in the key space, and one that
// sorts at or below the key it came from — so a boundary taken from it divides
// the range no worse, it merely divides it slightly earlier. That is a cheap
// price for a bound on memory, since nothing stops an object key being a
// kilobyte and a range holding a thousand samples of them.
const maxAccessKey = 96

// rangeAccess is where a range's recent writes landed.
//
// # A ring of the last N keys, not a reservoir
//
// A uniform sample of everything a range has ever taken answers the wrong
// question. Placement wants to know where writes are landing now, and for the
// workload this exists for — sequential keys, every write at the top of the key
// space — "now" is the whole point: the stored median divides the bytes, which
// on that workload is nowhere near the traffic.
//
// So the sample is simply the most recent keys. It needs no random number per
// write, which matters because this runs inside the write path, and it is
// recency-biased by construction rather than by a decay rule that would have to
// be tuned.
type rangeAccess struct {
	mu   sync.Mutex
	keys [accessSample]string
	// next is where the following key goes, and filled how many slots hold one.
	next, filled int
}

// observe records a key this range just took a write for.
//
// The whole of the write-path cost: one lock, one string copy bounded by
// maxAccessKey, two integer updates. No allocation once the ring is full,
// because the slot is overwritten.
func (a *rangeAccess) observe(key []byte) {
	if len(key) > maxAccessKey {
		key = key[:maxAccessKey]
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.keys[a.next] = string(key)
	a.next = (a.next + 1) % accessSample

	if a.filled < accessSample {
		a.filled++
	}
}

// median is the key that divides this range's recent writes, or empty when too
// few have been seen to say.
//
// Computed here rather than maintained on the write path: it is wanted once per
// reconciliation pass and would otherwise cost a sorted insert per write.
func (a *rangeAccess) median() string {
	a.mu.Lock()

	if a.filled < minAccessSample {
		a.mu.Unlock()

		return ""
	}

	sample := make([]string, a.filled)
	copy(sample, a.keys[:a.filled])

	a.mu.Unlock()

	slices.Sort(sample)

	return sample[len(sample)/2]
}

// AccessPoint is the key dividing a range's recent writes in half, or empty
// when this shard has not seen enough of them to say.
//
// The *accessed* median, as against the stored one SplitPoint computes. They
// are different distributions and conflating them is the mistake this exists to
// avoid: for a sequential-key workload every write lands at the top of the key
// space, so the stored median sits far below the traffic and a split there
// leaves the upper half taking all of it.
func (s *Shard) AccessPoint(r rangemap.Range) string {
	s.mu.RLock()
	a := s.access[idOf(r)]
	s.mu.RUnlock()

	if a == nil {
		return ""
	}

	at := a.median()

	// A median that does not fall inside the range divides nothing. It happens
	// when the map moved under the sample — the range was split, and half its
	// recent writes now belong to a neighbor.
	if at == "" || !r.Contains(at) || at == r.Start {
		return ""
	}

	return at
}

// trackAccess rebuilds the per-range samples for the ranges this shard owns,
// carrying over the ones it still owns. The caller holds the lock.
//
// Kept alongside trackLoad and for the same reasons: a map change elsewhere
// must not blind every other range, and a range new to this node starts with no
// history rather than inheriting a neighbor's.
func (s *Shard) trackAccess(owned []rangemap.Range) {
	next := make(map[rangeID]*rangeAccess, len(owned))

	for _, r := range owned {
		id := idOf(r)

		if a, ok := s.access[id]; ok {
			next[id] = a

			continue
		}

		next[id] = &rangeAccess{}
	}

	s.access = next
}
