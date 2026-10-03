package block

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// Shard repair: every node holding slot i of a partition should hold shard i
// of every coded block in it. A node learns which coded blocks exist by
// asking the partition's other slot nodes what shards they hold, rebuilds any
// of its own that is missing from K of the others, and hands shards of slots
// it no longer holds to the node that does.
//
// ponytail: every sweep lists every shard of every shared partition, on each
// peer; per-partition digests, as replicated blocks have, if sweeps get slow.

// ShardStats describe the last shard repair sweep and the totals since start.
type ShardStats struct {
	// LastSweep is when the last sweep finished; zero before the first.
	LastSweep time.Time
	// Degraded, Critical and Lost count coded blocks short of a shard, down
	// to K shards — one more loss from unreadable — and below K, unreadable,
	// as the last sweep saw them. Each partition's first slot counts its
	// blocks, so the cluster's totals are the sum over nodes.
	Degraded, Critical, Lost int
	// Unreachable counts partitions the last sweep could not see whole.
	Unreachable int
	// Rebuilt and HandedOver count shards rebuilt here and moved to the
	// node the layout gives them.
	Rebuilt, HandedOver int64
}

// codedBlock is a coded block whatever the shard.
type codedBlock struct {
	Hash Hash
	S    Scheme
}

type shardsReq struct {
	Partitions []int `json:"partitions"`
}

type shardsResp struct {
	Shards []string `json:"shards"`
}

func (m *Manager) serveShards(req shardsReq) (any, error) {
	l := m.member.Layout()
	if l == nil {
		return nil, ErrNoLayout
	}

	want := map[int]bool{}
	for _, p := range req.Partitions {
		want[p] = true
	}

	resp := shardsResp{Shards: []string{}}

	err := m.store.WalkShards(func(sh Shard, _ time.Time) error {
		if want[l.Partition(sh.Hash[:])] {
			resp.Shards = append(resp.Shards, sh.String())
		}

		return nil
	})

	return resp, err
}

// ShardStats returns the shard repair counters.
func (m *Manager) ShardStats() ShardStats {
	m.sync.mu.Lock()
	defer m.sync.mu.Unlock()

	return m.sync.shards
}

// RepairShards runs one shard repair sweep: hand over misplaced shards, then
// rebuild this node's missing shards of blocks live reports referenced.
func (m *Manager) RepairShards(ctx context.Context, live func(context.Context, Hash) (bool, error)) error {
	l := m.member.Layout()
	if l == nil {
		return ErrNoLayout
	}

	self := m.member.ID()

	// What this node holds, by partition, and what it holds for someone else.
	held := map[int]map[codedBlock][]int{}

	var misplaced []Shard

	err := m.store.WalkShards(func(sh Shard, _ time.Time) error {
		p := l.Partition(sh.Hash[:])
		if sh.I >= len(l.Slots[p]) || l.Slots[p][sh.I] != self {
			misplaced = append(misplaced, sh)

			return nil
		}

		add(held, p, sh)

		return nil
	})
	if err != nil {
		return err
	}

	handed := m.handOverShards(ctx, l, misplaced)

	// The partitions this node has a slot in, and the peers it shares each
	// with.
	byPeer := map[layout.NodeID][]int{}
	mine := map[int]int{} // partition → this node's slot

	for p, slots := range l.Slots {
		j := slices.Index(slots, self)
		if j < 0 {
			continue
		}

		mine[p] = j

		for _, id := range slots {
			if id != self {
				byPeer[id] = append(byPeer[id], p)
			}
		}
	}

	present := map[int]map[codedBlock][]int{}
	for p := range mine {
		present[p] = map[codedBlock][]int{}
		for b, idx := range held[p] {
			present[p][b] = slices.Clone(idx)
		}
	}

	unknown := map[int]bool{}

	for id, parts := range byPeer {
		var resp shardsResp
		if err := m.member.Call(ctx, id, http.MethodPost, "/v1/shard/list", shardsReq{Partitions: parts}, &resp); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			for _, p := range parts {
				unknown[p] = true
			}

			continue
		}

		for _, name := range resp.Shards {
			sh, err := ParseShard(name)
			if err != nil {
				continue
			}

			if p := l.Partition(sh.Hash[:]); present[p] != nil {
				add(present, p, sh)
			}
		}
	}

	var (
		rebuilt                  int64
		degraded, critical, lost int
	)

	for p, blocks := range present {
		j := mine[p]

		for b, idx := range blocks {
			n := len(idx)

			if j < b.S.K+b.S.M && !slices.Contains(idx, j) {
				if ok, err := live(ctx, b.Hash); err == nil && ok {
					if m.rebuild(ctx, l.Slots[p][:b.S.K+b.S.M], b, j) == nil {
						rebuilt++
						n++
					}
				}
			}

			if j != 0 || unknown[p] {
				continue
			}

			switch {
			case n < b.S.K:
				lost++
			case n <= b.S.K:
				critical++
			case n < b.S.K+b.S.M:
				degraded++
			}
		}
	}

	unreachable := 0

	for p := range unknown {
		if _, ok := mine[p]; ok {
			unreachable++
		}
	}

	m.sync.mu.Lock()
	m.sync.shards.LastSweep = time.Now()
	m.sync.shards.Degraded, m.sync.shards.Critical, m.sync.shards.Lost = degraded, critical, lost
	m.sync.shards.Unreachable = unreachable
	m.sync.shards.Rebuilt += rebuilt
	m.sync.shards.HandedOver += handed
	m.sync.mu.Unlock()

	return nil
}

func add(to map[int]map[codedBlock][]int, p int, sh Shard) {
	if to[p] == nil {
		to[p] = map[codedBlock][]int{}
	}

	b := codedBlock{Hash: sh.Hash, S: Scheme{K: sh.K, M: sh.M}}
	if !slices.Contains(to[p][b], sh.I) {
		to[p][b] = append(to[p][b], sh.I)
	}
}

// rebuild reconstructs shard j of b from the other shards on nodes and
// stores it here.
func (m *Manager) rebuild(ctx context.Context, nodes []layout.NodeID, b codedBlock, j int) error {
	enc, err := codec(b.S)
	if err != nil {
		return err
	}

	shards := make([][]byte, len(nodes))

	var wg sync.WaitGroup

	for i, id := range nodes {
		if i == j {
			continue
		}

		wg.Go(func() {
			if data, err := m.getShardFrom(ctx, id, Shard{Hash: b.Hash, K: b.S.K, M: b.S.M, I: i}); err == nil {
				shards[i] = data
			}
		})
	}

	wg.Wait()

	if err := enc.Reconstruct(shards); err != nil {
		return errors.Wrapf(err, "rebuild shard %d of %s", j, b.Hash)
	}

	return m.store.PutShard(Shard{Hash: b.Hash, K: b.S.K, M: b.S.M, I: j}, shards[j])
}

// handOverShards moves shards to the node the layout now gives their slot,
// and drops them here once it has them.
func (m *Manager) handOverShards(ctx context.Context, l *layout.Layout, shards []Shard) int64 {
	var n int64

	for _, sh := range shards {
		slots := l.Slots[l.Partition(sh.Hash[:])]
		if sh.I >= len(slots) {
			continue // The layout is too narrow for this scheme; keep it.
		}

		data, err := m.store.GetShard(sh)
		if err != nil {
			continue
		}

		if m.putShardOn(ctx, slots[sh.I], sh, data) != nil {
			continue
		}

		if m.store.DeleteShard(sh) == nil {
			n++
		}
	}

	return n
}
