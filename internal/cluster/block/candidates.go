package block

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"go.etcd.io/bbolt"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// Block collection checks candidates, not the whole store. A block becomes
// garbage only when it loses its last reference, and whoever releases a
// reference knows which block it was: it tells the block's holders, and they
// keep the block as a candidate, on disk, until a collection pass has checked
// it. A pass costs a reference check per block that lost a reference since
// the last one, rather than one per block stored.
//
// A candidate is a hint. One lost — the releasing node crashed before
// sending it, a holder was unreachable, a handover brought the block after
// it — leaves the block for GCWalk, a walk of the whole store
// that runs far less often and finds what the hints missed.

var candidatesBucket = []byte("block.gc.candidates")

// Candidate is a block that lost a reference, stored under Scheme.
type Candidate struct {
	Hash   Hash   `json:"h"`
	Scheme Scheme `json:"s"`
}

// key is the candidate's key: its hash, then its scheme, so a block stored
// under two schemes is two candidates.
func (c Candidate) key() []byte {
	return append(bytes.Clone(c.Hash[:]), c.Scheme.String()...)
}

func parseCandidate(k []byte) (Candidate, error) {
	if len(k) < len(Hash{}) {
		return Candidate{}, errors.Errorf("short candidate key %x", k)
	}

	var c Candidate

	copy(c.Hash[:], k)

	s, err := ParseScheme(string(k[len(Hash{}):]))
	if err != nil {
		return Candidate{}, err
	}

	c.Scheme = s

	return c, nil
}

// names are the files a holder of c may keep: the block, or its shards.
func (c Candidate) names() []string {
	if !c.Scheme.Coded() {
		return []string{c.Hash.String()}
	}

	out := make([]string, 0, c.Scheme.K+c.Scheme.M)
	for i := range c.Scheme.K + c.Scheme.M {
		out = append(out, Shard{Hash: c.Hash, K: c.Scheme.K, M: c.Scheme.M, I: i}.String())
	}

	return out
}

// Unreferenced tells the holders of each block that it lost a reference, so
// their next collection pass checks it. This node notes its own at once;
// the others are told in the background, and one that is not told leaves
// the block to GCWalk.
func (m *Manager) Unreferenced(ctx context.Context, cs []Candidate) {
	by := map[layout.NodeID][]Candidate{}

	for _, c := range cs {
		for _, id := range m.holders(c) {
			by[id] = append(by[id], c)
		}
	}

	if own, ok := by[m.member.ID()]; ok {
		_ = m.note(own)

		delete(by, m.member.ID())
	}

	if len(by) == 0 {
		return
	}

	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundTimeout)

	var wg sync.WaitGroup

	for id, cs := range by {
		wg.Go(func() {
			_ = m.member.Call(bg, id, http.MethodPost, "/v1/gc/candidates", cs, nil)
		})
	}

	go func() {
		wg.Wait()
		cancel()
	}()
}

// holders are the nodes that may keep c: every slot it has under every
// layout retained.
func (m *Manager) holders(c Candidate) []layout.NodeID {
	if !c.Scheme.Coded() {
		nodes, _ := m.replicas(c.Hash)

		return nodes
	}

	sets, err := m.shardHolders(c.Hash, c.Scheme)
	if err != nil {
		return nil
	}

	return union(sets)
}

func (m *Manager) serveCandidates(w http.ResponseWriter, r *http.Request) {
	var cs []Candidate
	if err := json.NewDecoder(r.Body).Decode(&cs); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	if err := m.note(cs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// note keeps cs as candidates, each stamped with when it was noted.
func (m *Manager) note(cs []Candidate) error {
	stamp := binary.BigEndian.AppendUint64(nil, uint64(time.Now().UnixNano())) //nolint:gosec // Wall clock, positive.

	err := m.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(candidatesBucket)
		if err != nil {
			return err
		}

		for _, c := range cs {
			if err := b.Put(c.key(), stamp); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return errors.Wrap(err, "note candidates")
	}

	return nil
}

// candidatePage bounds the candidates a pass reads at once.
const candidatePage = 10000

// collectCandidates checks the candidates, removing the blocks and shards of
// those no longer referenced and not written or touched since cutoff. A
// candidate is dropped once checked, unless its block was too fresh to
// remove or its references could not be read: then the next pass checks it
// again.
func (m *Manager) collectCandidates(ctx context.Context, cutoff time.Time, live func(context.Context, Hash) (bool, error)) (int, error) {
	removed, pending := 0, 0

	var after []byte

	for {
		page, err := m.candidates(after)
		if err != nil {
			return removed, err
		}

		if len(page) == 0 {
			break
		}

		after = page[len(page)-1].key

		var done []stamped

		for _, sc := range page {
			if err := ctx.Err(); err != nil {
				return removed, err
			}

			if sc.bad {
				done = append(done, sc)

				continue
			}

			n, keep, err := m.collectCandidate(ctx, sc.c, cutoff, live)
			removed += n

			if err != nil {
				return removed, err
			}

			if keep {
				pending++

				continue
			}

			done = append(done, sc)
		}

		if err := m.drop(done); err != nil {
			return removed, err
		}
	}

	m.candidatesPending.Store(int64(pending))

	return removed, nil
}

// collectCandidate checks one candidate: it returns how many files it
// removed, and whether the candidate must be checked again.
func (m *Manager) collectCandidate(ctx context.Context, c Candidate, cutoff time.Time, live func(context.Context, Hash) (bool, error)) (removed int, keep bool, err error) {
	names := c.names()

	held := false

	for _, name := range names {
		if m.store.exists(name) {
			held = true

			break
		}
	}

	if !held {
		return 0, false, nil
	}

	ok, err := live(ctx, c.Hash)
	if err != nil {
		// A reference we could not check is a reference we keep, and
		// check again.
		return 0, true, nil //nolint:nilerr // Skip this candidate, keep collecting the rest.
	}

	if ok {
		// Referenced again: a later release names it again.
		return 0, false, nil
	}

	for _, name := range names {
		deleted, err := m.store.deleteIfOlder(name, cutoff)
		if err != nil {
			return removed, true, err
		}

		switch {
		case deleted:
			removed++

			m.collected.Add(1)
		case m.store.exists(name):
			keep = true // Written or touched since: too fresh to tell.
		}
	}

	return removed, keep, nil
}

type stamped struct {
	key, stamp []byte
	c          Candidate
	// bad is a key that does not parse: dropped unchecked.
	bad bool
}

// candidates returns up to candidatePage candidates after the key after.
func (m *Manager) candidates(after []byte) ([]stamped, error) {
	var out []stamped

	err := m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(candidatesBucket)
		if b == nil {
			return nil
		}

		cur := b.Cursor()

		k, v := cur.First()
		if after != nil {
			k, v = cur.Seek(after)
			if bytes.Equal(k, after) {
				k, v = cur.Next()
			}
		}

		for ; k != nil && len(out) < candidatePage; k, v = cur.Next() {
			c, err := parseCandidate(k)
			out = append(out, stamped{key: bytes.Clone(k), stamp: bytes.Clone(v), c: c, bad: err != nil})
		}

		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "read candidates")
	}

	return out, nil
}

// drop removes checked candidates, each only if it was not noted again
// since it was read: a release in between names it anew.
func (m *Manager) drop(cs []stamped) error {
	if len(cs) == 0 {
		return nil
	}

	err := m.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(candidatesBucket)
		if b == nil {
			return nil
		}

		for _, sc := range cs {
			if !bytes.Equal(b.Get(sc.key), sc.stamp) {
				continue
			}

			if err := b.Delete(sc.key); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return errors.Wrap(err, "drop candidates")
	}

	return nil
}
