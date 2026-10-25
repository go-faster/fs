package block

import (
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// The store keeps per-slot digests of the blocks and shards it holds, so
// anti-entropy and shard repair read them rather than walking the store every
// sweep. They live in memory: blocks are plain files, with no transaction to
// move a stored digest in, and a digest kept beside them would go stale at the
// first crash between a file's rename and its update. They are built by one
// walk when first asked for, a stripe at a time under that stripe's lock, and
// moved under the same lock by every file placed or removed once its stripe
// has been walked, so each file is counted once whichever comes first.

// ShardSlot is a slot's coded blocks of one scheme, held as shard I.
type ShardSlot struct {
	Slot

	K int `json:"k"`
	M int `json:"m"`
	I int `json:"i"`
}

// shardPlace is a partition's shard index.
type shardPlace struct {
	P, I int
}

type index struct {
	// build serializes builds; mu guards the rest.
	build sync.Mutex
	mu    sync.Mutex

	// n is the partition count the digests are for; zero before a build.
	n        int
	walked   [256]bool
	complete bool

	blocks map[Slot][32]byte
	shards map[ShardSlot][32]byte
	// blockN and shardN count files per partition and per partition's
	// shard index: what tells a sweep it has something to hand over.
	blockN map[int]int
	shardN map[shardPlace]int
}

// apply moves the digests by the named file, added or removed. Names that
// are neither blocks nor shards are ignored.
func (x *index) apply(name string, added bool) {
	d := 1
	if !added {
		d = -1
	}

	if h, err := ParseHash(name); err == nil {
		slot := Slot{P: layout.PartitionOf(h[:], x.n), S: h[len(h)-1]}
		xorInto(x.blocks, slot, h)
		count(x.blockN, slot.P, d)

		return
	}

	sh, err := ParseShard(name)
	if err != nil {
		return
	}

	slot := ShardSlot{
		Slot: Slot{P: layout.PartitionOf(sh.Hash[:], x.n), S: sh.Hash[len(sh.Hash)-1]},
		K:    sh.K, M: sh.M, I: sh.I,
	}
	xorInto(x.shards, slot, sh.Hash)
	count(x.shardN, shardPlace{P: slot.P, I: sh.I}, d)
}

// xorInto moves m[k] by h: the XOR of hashes is the same for the same set
// whatever order it was built in. Empty digests are dropped, as a missing
// one reads as zero.
func xorInto[K comparable](m map[K][32]byte, k K, h Hash) {
	v := m[k]
	for i, b := range h {
		v[i] ^= b
	}

	if v == ([32]byte{}) {
		delete(m, k)

		return
	}

	m[k] = v
}

func count[K comparable](m map[K]int, k K, d int) {
	if m[k] += d; m[k] == 0 {
		delete(m, k)
	}
}

// stripeOf is the stripe of a name, which is also its top directory.
func stripeOf(name string) byte {
	b, _ := hex.DecodeString(name[:2])

	return b[0]
}

// note moves the digests by a file placed or removed; the caller holds the
// file's stripe lock and has seen the change happen.
func (s *Store) note(name string, added bool) {
	x := &s.idx

	x.mu.Lock()
	defer x.mu.Unlock()

	if x.n == 0 || !x.walked[stripeOf(name)] {
		return // The stripe's walk, still to come, will count it.
	}

	x.apply(name, added)
}

// ensureIndex builds the digests for n partitions unless they are built.
func (s *Store) ensureIndex(n int) error {
	x := &s.idx

	x.build.Lock()
	defer x.build.Unlock()

	x.mu.Lock()
	if x.n == n && x.complete {
		x.mu.Unlock()

		return nil
	}

	x.n, x.walked, x.complete = n, [256]bool{}, false
	x.blocks, x.shards = map[Slot][32]byte{}, map[ShardSlot][32]byte{}
	x.blockN, x.shardN = map[int]int{}, map[shardPlace]int{}
	x.mu.Unlock()

	for b := range 256 {
		if err := s.walkStripe(byte(b)); err != nil {
			x.mu.Lock()
			x.n = 0 // Rebuilt from scratch next time.
			x.mu.Unlock()

			return err
		}
	}

	x.mu.Lock()
	x.complete = true
	x.mu.Unlock()

	return nil
}

// walkStripe counts the files of one stripe, holding its lock so no file of
// it is placed or removed meanwhile.
func (s *Store) walkStripe(b byte) error {
	mu := &s.stripes[b]
	mu.Lock()
	defer mu.Unlock()

	top := filepath.Join(s.dir, hex.EncodeToString([]byte{b}))

	subs, err := os.ReadDir(top)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.Wrap(err, "read block dir")
	}

	var names []string

	for _, sub := range subs {
		if !sub.IsDir() {
			continue
		}

		entries, err := os.ReadDir(filepath.Join(top, sub.Name()))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			return errors.Wrap(err, "read block dir")
		}

		for _, e := range entries {
			if !e.IsDir() && !strings.HasSuffix(e.Name(), tmpSuffix) {
				names = append(names, e.Name())
			}
		}
	}

	x := &s.idx

	x.mu.Lock()
	defer x.mu.Unlock()

	for _, name := range names {
		x.apply(name, true)
	}

	x.walked[b] = true

	return nil
}

// BlockDigests returns the per-slot digests of the blocks held, by a layout
// of n partitions, for the partitions want accepts or all when want is nil.
func (s *Store) BlockDigests(n int, want func(p int) bool) (map[Slot][32]byte, error) {
	if err := s.ensureIndex(n); err != nil {
		return nil, err
	}

	x := &s.idx

	x.mu.Lock()
	defer x.mu.Unlock()

	out := make(map[Slot][32]byte, len(x.blocks))

	for slot, d := range x.blocks {
		if want == nil || want(slot.P) {
			out[slot] = d
		}
	}

	return out, nil
}

// ShardDigests is BlockDigests for shards.
func (s *Store) ShardDigests(n int, want func(p int) bool) (map[ShardSlot][32]byte, error) {
	if err := s.ensureIndex(n); err != nil {
		return nil, err
	}

	x := &s.idx

	x.mu.Lock()
	defer x.mu.Unlock()

	out := make(map[ShardSlot][32]byte, len(x.shards))

	for slot, d := range x.shards {
		if want == nil || want(slot.P) {
			out[slot] = d
		}
	}

	return out, nil
}

// BlockPartitions returns the partitions, of n, that hold a block here.
func (s *Store) BlockPartitions(n int) ([]int, error) {
	if err := s.ensureIndex(n); err != nil {
		return nil, err
	}

	x := &s.idx

	x.mu.Lock()
	defer x.mu.Unlock()

	out := make([]int, 0, len(x.blockN))
	for p := range x.blockN {
		out = append(out, p)
	}

	return out, nil
}

// shardPlaces returns the partitions' shard indexes, of n partitions, that
// hold a shard here.
func (s *Store) shardPlaces(n int) ([]shardPlace, error) {
	if err := s.ensureIndex(n); err != nil {
		return nil, err
	}

	x := &s.idx

	x.mu.Lock()
	defer x.mu.Unlock()

	out := make([]shardPlace, 0, len(x.shardN))
	for pl := range x.shardN {
		out = append(out, pl)
	}

	return out, nil
}
