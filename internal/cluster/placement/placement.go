// Package placement is the pure, failure-domain-aware placement function for
// go-faster/fs cluster mode (DESIGN.md FR-16). Given an epoch's topology and an
// object key it returns an ordered set of physical targets — the primary first,
// then copies/shards spread across the coarsest failure domains the topology
// affords: distinct racks first, then distinct nodes, then distinct disks,
// degrading gracefully in small clusters.
//
// It is deterministic (weighted rendezvous / HRW hashing, no process-random
// state) and does no I/O, so the same (topology, key, count) always yields the
// same ordered targets and the whole thing is unit-testable without a cluster.
package placement

import (
	"hash/fnv"
	"math"
	"sort"

	"github.com/go-faster/fs/internal/cluster"
)

// Target is a physical placement destination: a disk on a node. Rack is carried
// for observability and repair (which failure domain a copy lives in).
type Target struct {
	Node cluster.NodeID
	Disk cluster.DiskID
	Rack string
}

// candidate is a weighable disk with its precomputed rendezvous score.
type candidate struct {
	node  cluster.NodeID
	disk  cluster.DiskID
	rack  string
	score float64
}

// Place returns up to count ordered targets for key over the topology's epoch.
//
// count is the total number of physical copies/shards the scheme needs: 3 for
// the replica schemes ([A, B, C] — RF=2.5 and RF=3), or k+m for Reed-Solomon
// EC. Targets are always on distinct disks. Copies are spread across distinct
// racks first, then distinct nodes, then (only if forced) distinct disks on a
// reused node, so an object's blast radius is the coarsest failure domain the
// cluster can provide. When count exceeds the number of usable disks, every
// usable disk is returned (fewer than count) rather than duplicating a disk —
// callers must treat a short result as "cannot satisfy the scheme".
//
// key should already fold in the bucket (e.g. bucket+"\x00"+object); see
// ObjectKey.
func Place(t *cluster.Topology, key string, count int) []Target {
	if t == nil || count <= 0 {
		return nil
	}

	cands := candidatesFor(t, key)
	if len(cands) == 0 {
		return nil
	}

	// Highest score first; deterministic tiebreak so equal-weight disks and
	// hash collisions never depend on Node slice order.
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.score != b.score {
			return a.score > b.score
		}

		if a.node != b.node {
			return a.node < b.node
		}

		return a.disk < b.disk
	})

	if count > len(cands) {
		count = len(cands)
	}

	selected := make([]Target, 0, count)
	usedDisk := make(map[[2]string]struct{}, count) // (node,disk): never reuse a disk
	usedNode := make(map[cluster.NodeID]struct{}, count)
	usedRack := make(map[string]struct{}, count)

	// Three relaxation levels, strongest diversity first. Within each level we
	// scan the score-sorted candidates so the highest-scoring admissible disk
	// wins — this keeps the primary (selected[0]) the global HRW winner while
	// pushing later copies onto fresh failure domains.
	const (
		newRack = iota // copy must land in an unused rack
		newNode        // rack may repeat, node must be new
		anyDisk        // last resort: any unused disk (reused node)
	)

	for level := newRack; level <= anyDisk && len(selected) < count; level++ {
		for _, c := range cands {
			if len(selected) == count {
				break
			}

			dk := [2]string{string(c.node), string(c.disk)}
			if _, ok := usedDisk[dk]; ok {
				continue
			}

			switch level {
			case newRack:
				if _, ok := usedRack[c.rack]; ok {
					continue
				}
			case newNode:
				if _, ok := usedNode[c.node]; ok {
					continue
				}
			}

			selected = append(selected, Target{Node: c.node, Disk: c.disk, Rack: c.rack})
			usedDisk[dk] = struct{}{}
			usedNode[c.node] = struct{}{}
			usedRack[c.rack] = struct{}{}
		}
	}

	return selected
}

// candidatesFor builds the scored, placeable disks (weight > 0) for key.
func candidatesFor(t *cluster.Topology, key string) []candidate {
	cands := make([]candidate, 0, t.DiskCount())

	for i := range t.Nodes {
		n := t.Nodes[i]
		rack := n.FailureDomain()

		for _, d := range n.Disks {
			if d.Weight <= 0 {
				continue
			}

			cands = append(cands, candidate{
				node:  n.ID,
				disk:  d.ID,
				rack:  rack,
				score: weightedScore(key, n.ID, d.ID, d.Weight),
			})
		}
	}

	return cands
}

// weightedScore is the weighted rendezvous (HRW) score for a disk: a disk is
// chosen for a key with probability proportional to its weight, and adding or
// removing a disk only reshuffles the keys that touch it (minimal disruption).
// score = weight / -ln(u), with u a uniform (0,1) hash of (key, node, disk).
func weightedScore(key string, node cluster.NodeID, disk cluster.DiskID, weight float64) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(node))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(disk))

	// FNV-1a alone avalanches poorly (short, similar node/disk suffixes stay
	// correlated), skewing which disk wins the argmax; a splitmix64 finalizer
	// mixes it to near-uniform so equal-weight disks share load evenly.
	return weight / -math.Log(unitFloat(mix(h.Sum64())))
}

// mix is the splitmix64 finalizer: a bijective avalanche over 64 bits.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31

	return x
}

// unitFloat maps a 64-bit hash to a float in the open interval (0, 1). It uses
// the top 53 bits (float64 mantissa) and offsets by half a step so the result
// is never exactly 0 (which would make -ln explode) or 1.
func unitFloat(x uint64) float64 {
	const denom = float64(uint64(1) << 53)
	return (float64(x>>11) + 0.5) / denom
}

// ObjectKey folds a bucket and object key into the single placement key, so an
// object's placement depends on both (two buckets with the same key name place
// independently).
func ObjectKey(bucket, key string) string {
	return bucket + "\x00" + key
}
