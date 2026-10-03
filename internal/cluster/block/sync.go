package block

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// Anti-entropy for blocks works like the tables': replicas of a partition
// compare per-slot digests of the block hashes they hold and pull the blocks
// of every slot that differs. A node that holds blocks of a partition it no
// longer replicates pushes each to the owners missing it and drops it once
// every owner has it — after a layout change the new owners may all be new,
// and only the old holders have the data.
//
// The digest covers which blocks a replica holds, not their bytes: rot is
// caught when a block is read, which drops the bad copy and so shows here as
// a missing block.

// Slot is a partition's subdivision that digests are kept for.
type Slot struct {
	P int   `json:"p"`
	S uint8 `json:"s"`
}

// SyncStats describe the last sweep and the totals since start.
type SyncStats struct {
	// LastSweep is when the last sweep finished; zero before the first.
	LastSweep time.Time
	// OutOfSync is how many (slot, replica) pairs differed in the last sweep.
	OutOfSync int
	// Unreachable is how many replicas could not be compared in the last
	// sweep: partitions short of a healthy replica.
	Unreachable int
	// Pulled counts blocks fetched from other replicas.
	Pulled int64
	// HandedOver counts blocks moved to new owners and dropped here.
	HandedOver int64
}

const syncPage = 10000

type syncState struct {
	mu     sync.Mutex
	stats  SyncStats
	shards ShardStats
}

func (m *Manager) registerSync() {
	m.member.Handle("POST /v1/block/digests", handle(m.serveDigests))
	m.member.Handle("POST /v1/block/list", handle(m.serveList))
	m.member.Handle("HEAD /v1/block/{hash}", http.HandlerFunc(m.serveHead))
	m.member.Handle("POST /v1/shard/list", handle(m.serveShards))
}

// SyncStats returns the anti-entropy counters.
func (m *Manager) SyncStats() SyncStats {
	m.sync.mu.Lock()
	defer m.sync.mu.Unlock()

	return m.sync.stats
}

// Sync runs one anti-entropy sweep.
func (m *Manager) Sync(ctx context.Context) error {
	l := m.member.Layout()
	if l == nil {
		return ErrNoLayout
	}

	self := m.member.ID()
	shared := map[layout.NodeID][]int{}

	for p, slots := range l.Slots {
		replicas := slots[:min(Replicas, len(slots))]
		if !slices.Contains(replicas, self) {
			continue
		}

		for _, id := range replicas {
			if id != self {
				shared[id] = append(shared[id], p)
			}
		}
	}

	local, err := m.digests(l, nil)
	if err != nil {
		return err
	}

	var (
		outOfSync, unreachable int
		pulled                 int64
	)

	for id, partitions := range shared {
		n, diff, err := m.syncWith(ctx, id, partitions, local)
		pulled += n
		outOfSync += diff

		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			unreachable++
		}
	}

	handed, err := m.handOver(ctx, l)

	m.sync.mu.Lock()
	m.sync.stats.LastSweep = time.Now()
	m.sync.stats.OutOfSync = outOfSync
	m.sync.stats.Unreachable = unreachable
	m.sync.stats.Pulled += pulled
	m.sync.stats.HandedOver += handed
	m.sync.mu.Unlock()

	return err
}

func (m *Manager) syncWith(
	ctx context.Context, id layout.NodeID, partitions []int, local map[Slot][32]byte,
) (pulled int64, diff int, err error) {
	var remote digestsResp
	if err := m.member.Call(ctx, id, http.MethodPost, "/v1/block/digests", digestsReq{Partitions: partitions}, &remote); err != nil {
		return 0, 0, err
	}

	theirs := make(map[Slot][32]byte, len(remote.Digests))
	for _, d := range remote.Digests {
		theirs[d.Slot] = d.Sum
	}

	var differ []Slot

	for _, p := range partitions {
		for s := range 256 {
			slot := Slot{P: p, S: uint8(s)} //nolint:gosec // s < 256.
			if local[slot] != theirs[slot] {
				differ = append(differ, slot)
			}
		}
	}

	if len(differ) == 0 {
		return 0, 0, nil
	}

	var after Hash

	for {
		var page listResp
		if err := m.member.Call(ctx, id, http.MethodPost, "/v1/block/list", listReq{Slots: differ, After: after}, &page); err != nil {
			return pulled, len(differ), err
		}

		for _, h := range page.Hashes {
			if m.store.Has(h) {
				continue
			}

			data, err := m.Get(ctx, h)
			if err != nil {
				return pulled, len(differ), err
			}

			if err := m.store.Put(h, data); err != nil {
				return pulled, len(differ), err
			}

			pulled++
		}

		if !page.More {
			return pulled, len(differ), nil
		}

		after = page.Hashes[len(page.Hashes)-1]
	}
}

// handOver pushes blocks of partitions this node no longer replicates to the
// owners missing them, and drops each once every owner has it.
func (m *Manager) handOver(ctx context.Context, l *layout.Layout) (int64, error) {
	self := m.member.ID()

	var moving []Hash

	err := m.store.Walk(func(h Hash, tmp bool, _ time.Time, _ string) error {
		if tmp {
			return nil
		}

		slots := l.Slots[l.Partition(h[:])]
		if !slices.Contains(slots[:min(Replicas, len(slots))], self) {
			moving = append(moving, h)
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	var handed int64

	for _, h := range moving {
		if err := ctx.Err(); err != nil {
			return handed, err
		}

		slots := l.Slots[l.Partition(h[:])]

		if !m.pushTo(ctx, h, slots[:min(Replicas, len(slots))]) {
			continue // Kept, and retried next sweep.
		}

		if err := m.store.Delete(h); err != nil {
			return handed, err
		}

		handed++
	}

	return handed, nil
}

// pushTo makes sure every owner holds h, sending it to those that do not. It
// reports whether all of them do.
func (m *Manager) pushTo(ctx context.Context, h Hash, owners []layout.NodeID) bool {
	var data []byte

	for _, id := range owners {
		status, _, err := m.member.Raw(ctx, id, http.MethodHead, "/v1/block/"+h.String(), nil)
		if err != nil {
			return false
		}

		if status == http.StatusOK {
			continue
		}

		if data == nil {
			if data, err = m.store.Get(h); err != nil {
				return false
			}
		}

		if err := m.putOn(ctx, id, h, data); err != nil {
			return false
		}
	}

	return true
}

// digests fingerprints the local blocks per slot, for the given partitions or
// all of them when partitions is nil.
func (m *Manager) digests(l *layout.Layout, partitions []int) (map[Slot][32]byte, error) {
	var want map[int]bool
	if partitions != nil {
		want = make(map[int]bool, len(partitions))
		for _, p := range partitions {
			want[p] = true
		}
	}

	out := map[Slot][32]byte{}

	err := m.store.Walk(func(h Hash, tmp bool, _ time.Time, _ string) error {
		if tmp {
			return nil
		}

		slot := slotOf(l, h)
		if want != nil && !want[slot.P] {
			return nil
		}

		// XOR of hashes: the same set of blocks gives the same digest,
		// whatever order it was written in.
		d := out[slot]
		for i, b := range h {
			d[i] ^= b
		}

		out[slot] = d

		return nil
	})

	return out, err
}

// slotOf subdivides by a hash byte the partition function does not decide
// alone, so the slots of one partition split its blocks evenly.
func slotOf(l *layout.Layout, h Hash) Slot {
	return Slot{P: l.Partition(h[:]), S: h[len(h)-1]}
}

type (
	digestsReq struct {
		Partitions []int `json:"partitions"`
	}
	slotDigest struct {
		Slot Slot     `json:"slot"`
		Sum  [32]byte `json:"sum"`
	}
	digestsResp struct {
		Digests []slotDigest `json:"digests"`
	}
	listReq struct {
		Slots []Slot `json:"slots"`
		After Hash   `json:"after"`
	}
	listResp struct {
		Hashes []Hash `json:"hashes"`
		More   bool   `json:"more,omitempty"`
	}
)

func (m *Manager) serveDigests(req digestsReq) (any, error) {
	l := m.member.Layout()
	if l == nil {
		return nil, ErrNoLayout
	}

	d, err := m.digests(l, req.Partitions)
	if err != nil {
		return nil, err
	}

	resp := digestsResp{Digests: make([]slotDigest, 0, len(d))}
	for slot, sum := range d {
		resp.Digests = append(resp.Digests, slotDigest{Slot: slot, Sum: sum})
	}

	return resp, nil
}

// serveList returns the hashes held in the given slots, in hash order, a page
// at a time.
func (m *Manager) serveList(req listReq) (any, error) {
	l := m.member.Layout()
	if l == nil {
		return nil, ErrNoLayout
	}

	want := make(map[Slot]bool, len(req.Slots))
	for _, s := range req.Slots {
		want[s] = true
	}

	var hashes []Hash

	err := m.store.Walk(func(h Hash, tmp bool, _ time.Time, _ string) error {
		if !tmp && want[slotOf(l, h)] && bytes.Compare(h[:], req.After[:]) > 0 {
			hashes = append(hashes, h)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(hashes, func(i, j int) bool { return bytes.Compare(hashes[i][:], hashes[j][:]) < 0 })

	resp := listResp{Hashes: hashes}
	if len(hashes) > syncPage {
		resp.Hashes, resp.More = hashes[:syncPage], true
	}

	return resp, nil
}

func (m *Manager) serveHead(w http.ResponseWriter, r *http.Request) {
	h, err := ParseHash(r.PathValue("hash"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)

		return
	}

	if !m.store.Has(h) {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	w.WriteHeader(http.StatusOK)
}

// handle adapts a JSON request/response function to an http.Handler.
func handle[Req any](fn func(Req) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Req
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		resp, err := fn(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		writeJSON(w, resp)
	})
}

func decodeJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errors.Wrap(err, "decode request")
	}

	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
