package table

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"sync"
	"time"

	"go.etcd.io/bbolt"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// Anti-entropy: replicas of a partition compare what they hold and pull what
// they lack. Writes reach a quorum and reads repair what they touch; this is
// what reaches everything else — the replica that was down, the row nobody
// read, the partition a layout change moved to a new node.
//
// Rows are fingerprinted per slot: a partition and the first byte of the
// row's key hash. A replica asks another for its digests of the partitions
// they share and pulls the rows of every slot whose digest differs, so one
// missed write costs a 256th of a partition, not all of it.
//
// ponytail: every sweep scans the whole table, on this node and on each peer
// asked; a deeper hash tree if sweeps get slow.

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
	// Pulled counts rows merged from other replicas.
	Pulled int64
	// HandedOver counts rows moved to new owners and dropped here.
	HandedOver int64
}

// syncPage bounds the rows one items call returns.
const syncPage = 1000

type syncState struct {
	mu    sync.Mutex
	stats SyncStats
	gc    GCStats
}

func (t *Table[R]) registerSync() {
	t.member.Handle("POST /v1/table/"+t.name+"/digests", handle(t.serveDigests))
	t.member.Handle("POST /v1/table/"+t.name+"/items", handle(t.serveItems))
}

// SyncStats returns the anti-entropy counters.
func (t *Table[R]) SyncStats() SyncStats {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	return t.sync.stats
}

// Sync runs one anti-entropy sweep: pull every slot that differs from another
// replica of a partition this node holds, then hand over rows of partitions it
// no longer holds.
func (t *Table[R]) Sync(ctx context.Context) error {
	l := t.member.Layout()
	if l == nil {
		return ErrNoLayout
	}

	self := t.member.ID()

	// Partitions shared with each other replica.
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

	local, err := t.digests(l, nil)
	if err != nil {
		return err
	}

	var outOfSync, unreachable int

	var pulled int64

	for id, partitions := range shared {
		n, diff, err := t.syncWith(ctx, id, partitions, local)
		pulled += n
		outOfSync += diff

		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			unreachable++
		}
	}

	handed, err := t.handOver(ctx, l)

	t.sync.mu.Lock()
	t.sync.stats.LastSweep = time.Now()
	t.sync.stats.OutOfSync = outOfSync
	t.sync.stats.Unreachable = unreachable
	t.sync.stats.Pulled += pulled
	t.sync.stats.HandedOver += handed
	t.sync.mu.Unlock()

	return err
}

// Run sweeps every interval until ctx is canceled, and right away when the
// layout version changes — a moved partition should not wait a full period.
func (t *Table[R]) Run(ctx context.Context, interval time.Duration) {
	var last uint64

	tick := time.NewTicker(interval)
	defer tick.Stop()

	check := time.NewTicker(time.Second)
	defer check.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-check.C:
			l := t.member.Layout()
			if l == nil || l.Version == last {
				continue
			}
		}

		if l := t.member.Layout(); l != nil {
			last = l.Version
		}

		// ponytail: a failed sweep is retried next period; the stats say so.
		_ = t.Sync(ctx)
	}
}

// syncWith pulls the slots of partitions that differ from replica id.
func (t *Table[R]) syncWith(
	ctx context.Context, id layout.NodeID, partitions []int, local map[Slot][32]byte,
) (pulled int64, diff int, err error) {
	var remote digestsResp
	if err := t.member.Call(ctx, id, "POST", "/v1/table/"+t.name+"/digests", digestsReq{Partitions: partitions}, &remote); err != nil {
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

	var cursor []byte

	for {
		var page itemsResp
		if err := t.member.Call(ctx, id, "POST", "/v1/table/"+t.name+"/items", itemsReq{Slots: differ, After: cursor}, &page); err != nil {
			return pulled, len(differ), err
		}

		if err := t.localInsert(page.Entries); err != nil {
			return pulled, len(differ), err
		}

		pulled += int64(len(page.Entries))

		if len(page.Next) == 0 {
			return pulled, len(differ), nil
		}

		cursor = page.Next
	}
}

// handOver moves rows of partitions this node no longer replicates to their
// replicas, and drops them here once every replica has merged them.
func (t *Table[R]) handOver(ctx context.Context, l *layout.Layout) (int64, error) {
	self := t.member.ID()

	byOwners := map[int][]wireEntry{}

	err := t.scan(func(k, v []byte, pk string) error {
		p := l.Partition([]byte(pk))
		if slices.Contains(l.Slots[p][:min(Replicas, len(l.Slots[p]))], self) {
			return nil
		}

		byOwners[p] = append(byOwners[p], wireEntry{PK: pk, SK: string(k[len(key(pk, "")):]), Row: bytes.Clone(v)})

		return nil
	})
	if err != nil {
		return 0, err
	}

	var handed int64

	for p, entries := range byOwners {
		owners := l.Slots[p][:min(Replicas, len(l.Slots[p]))]

		for chunk := range slices.Chunk(entries, syncPage) {
			req := insertReq{Entries: chunk}

			ok := true

			for _, id := range owners {
				if err := t.insertOn(ctx, id, req); err != nil {
					ok = false

					break
				}
			}

			// Keep everything until every owner has it: a partial handover
			// is retried next sweep, and merging twice is harmless.
			if !ok {
				continue
			}

			n, err := t.dropIfUnchanged(chunk)
			if err != nil {
				return handed, err
			}

			handed += n
		}
	}

	return handed, nil
}

// dropIfUnchanged deletes entries whose stored row is still the one handed
// over. A row merged with a newer write since was not fully handed over.
func (t *Table[R]) dropIfUnchanged(entries []wireEntry) (int64, error) {
	var n int64

	err := t.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(t.name))

		for _, e := range entries {
			k := key(e.PK, e.SK)
			if !bytes.Equal(b.Get(k), e.Row) {
				continue
			}

			if err := b.Delete(k); err != nil {
				return errors.Wrap(err, "delete")
			}

			n++
		}

		return nil
	})

	return n, err
}

// digests fingerprints the local rows per slot, for the given partitions or
// all of them when partitions is nil.
func (t *Table[R]) digests(l *layout.Layout, partitions []int) (map[Slot][32]byte, error) {
	var want map[int]bool
	if partitions != nil {
		want = make(map[int]bool, len(partitions))
		for _, p := range partitions {
			want[p] = true
		}
	}

	out := map[Slot][32]byte{}

	err := t.scan(func(k, v []byte, pk string) error {
		slot, ok := slotOf(l, k, pk)
		if !ok || want != nil && !want[slot.P] {
			return nil
		}

		// XOR of per-row hashes: independent of order, so two replicas
		// holding the same rows agree however they got them.
		h := sha256.New()
		h.Write(k)
		h.Write([]byte{0})
		h.Write(v)

		d := out[slot]
		for i, b := range h.Sum(nil) {
			d[i] ^= b
		}

		out[slot] = d

		return nil
	})

	return out, err
}

func slotOf(l *layout.Layout, k []byte, pk string) (Slot, bool) {
	if len(l.Slots) == 0 {
		return Slot{}, false
	}

	sum := sha256.Sum256(k)

	return Slot{P: l.Partition([]byte(pk)), S: sum[0]}, true
}

// scan calls fn for every stored row with its decoded partition key.
func (t *Table[R]) scan(fn func(k, v []byte, pk string) error) error {
	return t.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(t.name)).ForEach(func(k, v []byte) error {
			pk, _, ok := splitKey(k)
			if !ok {
				return nil // Not a row key; nothing this table wrote.
			}

			return fn(k, v, pk)
		})
	})
}

// splitKey is key's inverse.
func splitKey(k []byte) (pk, sk string, ok bool) {
	n, w := binary.Uvarint(k)
	if w <= 0 || n > uint64(len(k)-w) { //nolint:gosec // len(k)-w is not negative: w <= len(k).
		return "", "", false
	}

	end := w + int(n) //nolint:gosec // n is at most len(k), checked above.

	return string(k[w:end]), string(k[end:]), true
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
	itemsReq struct {
		Slots []Slot `json:"slots"`
		// After resumes past the last key of the previous page. Raw key
		// bytes, so not a string: JSON would mangle what is not UTF-8.
		After []byte `json:"after,omitempty"`
	}
	itemsResp struct {
		Entries []wireEntry `json:"entries"`
		// Next is the cursor of the next page; empty on the last.
		Next []byte `json:"next,omitempty"`
	}
)

func (t *Table[R]) serveDigests(req digestsReq) (any, error) {
	l := t.member.Layout()
	if l == nil {
		return nil, ErrNoLayout
	}

	d, err := t.digests(l, req.Partitions)
	if err != nil {
		return nil, err
	}

	resp := digestsResp{Digests: make([]slotDigest, 0, len(d))}
	for slot, sum := range d {
		resp.Digests = append(resp.Digests, slotDigest{Slot: slot, Sum: sum})
	}

	return resp, nil
}

func (t *Table[R]) serveItems(req itemsReq) (any, error) {
	l := t.member.Layout()
	if l == nil {
		return nil, ErrNoLayout
	}

	want := make(map[Slot]bool, len(req.Slots))
	for _, s := range req.Slots {
		want[s] = true
	}

	var (
		resp itemsResp
		last []byte
	)

	errFull := errors.New("page full")

	err := t.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket([]byte(t.name)).Cursor()

		k, v := c.First()
		if len(req.After) > 0 {
			k, v = c.Seek(req.After)
			if bytes.Equal(k, req.After) {
				k, v = c.Next()
			}
		}

		for ; k != nil; k, v = c.Next() {
			pk, sk, ok := splitKey(k)
			if !ok {
				continue
			}

			slot, ok := slotOf(l, k, pk)
			if !ok || !want[slot] {
				continue
			}

			if len(resp.Entries) == syncPage {
				resp.Next = last

				return errFull
			}

			resp.Entries = append(resp.Entries, wireEntry{PK: pk, SK: sk, Row: bytes.Clone(v)})
			last = bytes.Clone(k)
		}

		return nil
	})
	if err != nil && !errors.Is(err, errFull) {
		return nil, err
	}

	return resp, nil
}
