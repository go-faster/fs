package clusterstore

import (
	"sort"
	"sync"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/placement"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

// maxRememberedEpochs bounds how many distinct topology epochs the
// coordinator keeps for old-placement fallback (the current one included).
const maxRememberedEpochs = 4

// epochMemory remembers recently observed topology epochs. After a topology
// change, an object's fragments still live at the previous epoch's placement
// until relocation completes — reads and repair consult remembered epochs so
// the change never makes committed data unreachable ("consult both epochs
// until cutover"). Memory is process-local: after a restart, scrub-discovered
// local fragments (repair sources) cover what the memory would have.
type epochMemory struct {
	mu     sync.Mutex
	epochs map[uint64]rememberedEpoch

	// memoTopo/memoSig memoize the last snapshot's placement signature:
	// observe runs on every read/repair, usually with the same pointer.
	memoTopo *cluster.Topology
	memoSig  string
}

// rememberedEpoch pairs a snapshot with its placement signature.
type rememberedEpoch struct {
	topo *cluster.Topology
	sig  string
}

// observe records a topology snapshot, evicting the oldest epoch beyond the
// bound, and returns all remembered snapshots newest-epoch first.
//
// Epochs are deduplicated by placement signature: registry churn that moves
// no placement — capacity refreshes, address changes, lease re-acquisition —
// bumps the etcd revision (epoch) constantly, and remembering each would
// flush the genuinely distinct placements out of the bound. Two epochs with
// equal signatures place every object identically, so dropping the older one
// loses no relocation source.
func (m *epochMemory) observe(topo *cluster.Topology) []*cluster.Topology {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.epochs == nil {
		m.epochs = make(map[uint64]rememberedEpoch)
	}

	if m.memoTopo != topo {
		m.memoTopo, m.memoSig = topo, topo.Signature()
	}

	for e, r := range m.epochs {
		if r.sig == m.memoSig && e != topo.Epoch {
			delete(m.epochs, e)
		}
	}

	m.epochs[topo.Epoch] = rememberedEpoch{topo: topo, sig: m.memoSig}

	out := make([]*cluster.Topology, 0, len(m.epochs))
	for _, r := range m.epochs {
		out = append(out, r.topo)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Epoch > out[j].Epoch })

	for len(out) > maxRememberedEpochs {
		last := out[len(out)-1]
		delete(m.epochs, last.Epoch)

		out = out[:len(out)-1]
	}

	return out
}

// topologies returns the current topology plus remembered previous epochs,
// newest first. The first element is always the current epoch.
func (c *Coordinator) topologies() []*cluster.Topology {
	return c.epochs.observe(c.topo.Topology())
}

// candidateTarget is a placement target bound to the topology epoch it was
// computed under — dialing must resolve the node's address in that epoch (the
// node may already be gone from the current one).
type candidateTarget struct {
	target placement.Target
	topo   *cluster.Topology
}

// diskRef identifies a physical target for equality checks. placement.Target
// carries the rack label too, which may differ for the same disk across
// epochs — never compare Targets directly.
type diskRef struct {
	node cluster.NodeID
	disk cluster.DiskID
}

func targetRef(t placement.Target) diskRef {
	return diskRef{node: t.Node, disk: t.Disk}
}

// allSidecarCandidates lists the targets that may hold the object's sidecar
// across every remembered epoch, deduplicated by (node, disk), newest epoch
// first within the same candidate ordering as sidecarCandidates.
func (c *Coordinator) allSidecarCandidates(bucket, key string) []candidateTarget {
	seen := make(map[diskRef]struct{})

	var out []candidateTarget

	for _, topo := range c.topologies() {
		for _, t := range c.sidecarCandidates(topo, bucket, key) {
			if _, ok := seen[targetRef(t)]; ok {
				continue
			}

			seen[targetRef(t)] = struct{}{}

			out = append(out, candidateTarget{target: t, topo: topo})
		}
	}

	return out
}

// epochPlans computes the object's fragment plan under every remembered
// epoch, newest first. Epochs that cannot host the scheme are skipped. The
// first entry is the current epoch's plan (the authoritative layout).
type epochPlan struct {
	topo *cluster.Topology
	plan []fragment.Item
}

func (c *Coordinator) epochPlans(s scheme.Scheme, bucket, key string, size int64) []epochPlan {
	pkey := placement.ObjectKey(bucket, key)

	var out []epochPlan

	for _, topo := range c.topologies() {
		plan, err := fragment.Plan(topo, s, pkey, size)
		if err != nil {
			continue
		}

		out = append(out, epochPlan{topo: topo, plan: plan})
	}

	return out
}
