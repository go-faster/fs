package table

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"slices"
	"time"

	"github.com/go-faster/errors"
	"go.etcd.io/bbolt"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// Tombstone collection: a deleted row stays as a tombstone, so a replica that
// missed the delete cannot bring the row back. Once every replica holds the
// tombstone, it can go.
//
// A table with a compact function queues each row it stores that compacts —
// to nothing, or to something smaller — with the hash of its bytes. Collect
// takes rows queued at least the delay ago and, per partition, first writes
// them to every replica, then has every replica replace the row with its
// compacted form if the row is still exactly that. A replica that merged a
// newer write meanwhile keeps it, and anti-entropy brings the rest of the
// replicas back up to it: a collected row can come back as a tombstone, never
// as data. A replica that cannot be reached defers the whole batch.
//
// The delay is what covers the writes collection cannot see: one still in
// flight to a replica, and a node that held the partition under an earlier
// layout and has not handed it over yet. Either could carry the row from
// before its delete; long after both are settled, nothing does.

// Compaction is what collection does with a row every replica holds.
type Compaction int

const (
	// Keep leaves the row: it holds no tombstone.
	Keep Compaction = iota
	// Replace swaps the row for the smaller one compact returned.
	Replace
	// Delete removes the row.
	Delete
)

// GCStats describe tombstone collection.
type GCStats struct {
	// Queued is how many rows waited for collection at the last pass.
	Queued int
	// Collected counts rows compacted on every replica since start.
	Collected int64
	// Deferred counts rows a pass left for the next one because a replica
	// could not be reached.
	Deferred int64
}

// gcPass bounds the rows one pass holds in memory; Collect runs passes until
// nothing due is left.
const gcPass = 10 * syncPage

func (t *Table[R]) gcBucket() []byte { return []byte(t.name + ".gc") }

// GCStats returns the tombstone collection counters.
func (t *Table[R]) GCStats() GCStats {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	return t.sync.gc
}

// enqueue queues the row k now holds, v decoding to row, if it compacts.
func (t *Table[R]) enqueue(q *bbolt.Bucket, k, v []byte, row R) error {
	if q == nil {
		return nil
	}

	if _, c := t.compact(row); c == Keep {
		return nil
	}

	sum := sha256.Sum256(v)

	return q.Put(k, binary.BigEndian.AppendUint64(sum[:], uint64(time.Now().UnixNano()))) //nolint:gosec // Wall clock, positive.
}

type gcItem struct {
	key, entry []byte
	old        wireEntry
	repl       replaceEntry
}

// Collect compacts every row queued at least delay ago on every replica of it.
func (t *Table[R]) Collect(ctx context.Context, delay time.Duration) error {
	if t.compact == nil {
		return nil
	}

	for {
		more, err := t.collectPass(ctx, delay)
		if err != nil || !more {
			return err
		}
	}
}

// collectPass collects up to gcPass rows, and reports whether more are due.
func (t *Table[R]) collectPass(ctx context.Context, delay time.Duration) (more bool, err error) {
	l := t.member.Layout()
	if l == nil {
		return false, ErrNoLayout
	}

	// During a layout change a replica of an older version may still hold
	// the row from before the delete; dropping the tombstone now could let
	// handover bring that back. Collection waits for the change to retire.
	if len(t.member.Layouts()) > 1 {
		return false, nil
	}

	self := t.member.ID()
	cutoff := time.Now().Add(-delay).UnixNano()

	var (
		queued int
		due    = map[int][]gcItem{}
		n      int
		stale  []gcItem
	)

	err = t.db.View(func(tx *bbolt.Tx) error {
		rows := tx.Bucket([]byte(t.name))

		return tx.Bucket(t.gcBucket()).ForEach(func(k, e []byte) error {
			queued++

			if n >= gcPass || len(e) != sha256.Size+8 || int64(binary.BigEndian.Uint64(e[sha256.Size:])) > cutoff { //nolint:gosec // Written from a positive int64.
				more = more || n >= gcPass

				return nil
			}

			item := gcItem{key: bytes.Clone(k), entry: bytes.Clone(e)}

			// The row changed or went since it was queued: what it is now
			// was queued again if it compacts.
			v := rows.Get(k)
			if v == nil || sha256.Sum256(v) != [sha256.Size]byte(e[:sha256.Size]) {
				stale = append(stale, item)

				return nil
			}

			pk, sk, ok := splitKey(k)
			if !ok {
				return nil
			}

			// A partition this node no longer holds is handed over, not
			// collected; its new replicas queue the row again.
			p := l.Partition([]byte(pk))
			if !slices.Contains(l.Slots[p][:min(Replicas, len(l.Slots[p]))], self) {
				return nil
			}

			row, err := t.decode(v)
			if err != nil {
				return err
			}

			item.old = wireEntry{PK: pk, SK: sk, Row: bytes.Clone(v)}
			item.repl = replaceEntry{PK: pk, SK: sk, Hash: sha256.Sum256(v)}

			switch c, how := t.compact(row); how {
			case Keep:
				return nil // Queued by an older build's rules; nothing to do.
			case Replace:
				if item.repl.Row, err = json.Marshal(c); err != nil {
					return errors.Wrap(err, "encode row")
				}
			case Delete:
			}

			due[p] = append(due[p], item)
			n++

			return nil
		})
	})
	if err != nil {
		return false, err
	}

	if err := t.unqueue(stale); err != nil {
		return false, err
	}

	var collected, deferred int64

	for p, items := range due {
		owners := l.Slots[p][:min(Replicas, len(l.Slots[p]))]

		for chunk := range slices.Chunk(items, syncPage) {
			push := insertReq{Entries: make([]wireEntry, len(chunk))}
			repl := replaceReq{Entries: make([]replaceEntry, len(chunk))}

			for i, it := range chunk {
				push.Entries[i], repl.Entries[i] = it.old, it.repl
			}

			if !t.onAll(ctx, owners, func(id layout.NodeID) error { return t.insertOn(ctx, id, push) }) {
				deferred += int64(len(chunk))

				continue
			}

			t.onAll(ctx, owners, func(id layout.NodeID) error {
				return t.on(ctx, id, "replace", repl, nil, func() (any, error) {
					return nil, t.localReplace(repl.Entries)
				})
			})

			collected += int64(len(chunk))
		}
	}

	t.sync.mu.Lock()
	t.sync.gc.Queued = queued
	t.sync.gc.Collected += collected
	t.sync.gc.Deferred += deferred
	t.sync.mu.Unlock()

	return more && ctx.Err() == nil, ctx.Err()
}

// onAll runs fn on every node and reports whether all succeeded.
func (t *Table[R]) onAll(ctx context.Context, nodes []layout.NodeID, fn func(layout.NodeID) error) bool {
	_, err := quorum(nodes, len(nodes), func(id layout.NodeID) (struct{}, error) {
		return struct{}{}, fn(id)
	})

	return err == nil && ctx.Err() == nil
}

// unqueue drops queue entries that still say what they said when read: one
// replaced since is about a newer row.
func (t *Table[R]) unqueue(items []gcItem) error {
	if len(items) == 0 {
		return nil
	}

	return t.db.Update(func(tx *bbolt.Tx) error {
		q := tx.Bucket(t.gcBucket())

		for _, it := range items {
			if bytes.Equal(q.Get(it.key), it.entry) {
				if err := q.Delete(it.key); err != nil {
					return errors.Wrap(err, "unqueue")
				}
			}
		}

		return nil
	})
}

type (
	// replaceEntry replaces the row at (PK, SK) with Row, or deletes it when
	// Row is empty, if the stored row hashes to Hash.
	replaceEntry struct {
		PK   string            `json:"pk"`
		SK   string            `json:"sk"`
		Hash [sha256.Size]byte `json:"hash"`
		Row  json.RawMessage   `json:"row,omitempty"`
	}
	replaceReq struct {
		Entries []replaceEntry `json:"entries"`
	}
)

func (t *Table[R]) serveReplace(req replaceReq) (any, error) { return nil, t.localReplace(req.Entries) }

func (t *Table[R]) localReplace(entries []replaceEntry) error {
	return t.db.Update(func(tx *bbolt.Tx) error {
		b, q := tx.Bucket([]byte(t.name)), tx.Bucket(t.gcBucket())

		for _, e := range entries {
			k := key(e.PK, e.SK)

			v := b.Get(k)
			if v == nil || sha256.Sum256(v) != e.Hash {
				continue
			}

			var err error
			if len(e.Row) == 0 {
				err = b.Delete(k)
			} else {
				err = b.Put(k, e.Row)
			}

			if err != nil {
				return errors.Wrap(err, "replace")
			}

			// The queued entry is about the row just replaced.
			if q != nil {
				if err := q.Delete(k); err != nil {
					return errors.Wrap(err, "unqueue")
				}
			}
		}

		return nil
	})
}
