package table

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/go-faster/errors"
	"go.etcd.io/bbolt"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// Slot digests are kept, not computed: each replica stores the digest of
// every slot it holds rows of in a <table>.digests bucket, moved in the
// transaction of every change by the hashes of the row it removes and the
// row it stores. A digest is the XOR of its rows' hashes, so a change is two
// XORs, and a sweep or a peer's request reads 32 bytes per slot instead of
// every row.
//
// Slots depend on the partition count, which the bucket is stamped with. A
// store without the stamp, or stamped for another count, has its digests
// rebuilt from its rows the first time they are read; until then writes
// leave them alone.

// digestStamp is the key of the partition count; slot keys are 3 bytes.
var digestStamp = []byte("partitions")

func (t *Table[R]) digestBucket() []byte { return []byte(t.name + ".digests") }

// slotKey is a slot's key in the digests bucket, ordered by partition.
func slotKey(s Slot) []byte {
	return []byte{byte(s.P >> 8), byte(s.P), s.S} //nolint:gosec // P < 1<<16, the partition limit.
}

func keySlot(k []byte) Slot { return Slot{P: int(k[0])<<8 | int(k[1]), S: k[2]} }

// rowHash is a row's share of its slot's digest.
func rowHash(k, v []byte) [32]byte {
	h := sha256.New()
	h.Write(k)
	h.Write([]byte{0})
	h.Write(v)

	var sum [32]byte

	h.Sum(sum[:0])

	return sum
}

// rowSlot is the slot of the row at k with partition key pk among n
// partitions.
func rowSlot(k []byte, pk string, n int) Slot {
	return Slot{P: layout.PartitionOf([]byte(pk), n), S: sha256.Sum256(k)[0]}
}

func digestCount(d *bbolt.Bucket) int {
	if d == nil {
		return 0
	}

	v := d.Get(digestStamp)
	if len(v) != 4 {
		return 0
	}

	return int(binary.BigEndian.Uint32(v))
}

// moveDigest records, within tx, replacing the row at k — old, nil if
// there was none — with row, nil for a delete. Call it before the change:
// old may be a slice of the page the change rewrites.
func (t *Table[R]) moveDigest(tx *bbolt.Tx, k []byte, pk string, old, row []byte) error {
	d := tx.Bucket(t.digestBucket())

	n := digestCount(d)
	if n == 0 {
		return nil
	}

	var delta [32]byte

	if old != nil {
		delta = rowHash(k, old)
	}

	if row != nil {
		h := rowHash(k, row)
		for i := range delta {
			delta[i] ^= h[i]
		}
	}

	return xorDigest(d, rowSlot(k, pk, n), delta)
}

// xorDigest XORs delta into slot's digest in d, dropping a digest that
// returns to zero: a slot with no rows.
func xorDigest(d *bbolt.Bucket, slot Slot, delta [32]byte) error {
	if delta == ([32]byte{}) {
		return nil
	}

	sk := slotKey(slot)

	var cur [32]byte

	copy(cur[:], d.Get(sk))

	for i := range cur {
		cur[i] ^= delta[i]
	}

	if cur == ([32]byte{}) {
		return d.Delete(sk)
	}

	if err := d.Put(sk, cur[:]); err != nil {
		return errors.Wrap(err, "put digest")
	}

	return nil
}

// ensureDigests rebuilds the digests from the rows unless they are kept for
// n partitions already.
func (t *Table[R]) ensureDigests(n int) error {
	var kept bool

	if err := t.db.View(func(tx *bbolt.Tx) error {
		kept = digestCount(tx.Bucket(t.digestBucket())) == n

		return nil
	}); err != nil {
		return err
	}

	if kept {
		return nil
	}

	return t.db.Update(func(tx *bbolt.Tx) error {
		// Checked again: a concurrent rebuild may have won.
		if digestCount(tx.Bucket(t.digestBucket())) == n {
			return nil
		}

		if tx.Bucket(t.digestBucket()) != nil {
			if err := tx.DeleteBucket(t.digestBucket()); err != nil {
				return errors.Wrap(err, "drop digests")
			}
		}

		d, err := tx.CreateBucket(t.digestBucket())
		if err != nil {
			return errors.Wrap(err, "create digests")
		}

		sums := map[Slot][32]byte{}

		if err := tx.Bucket([]byte(t.name)).ForEach(func(k, v []byte) error {
			pk, _, ok := splitKey(k)
			if !ok {
				return nil
			}

			slot, h := rowSlot(k, pk, n), rowHash(k, v)

			sum := sums[slot]
			for i := range sum {
				sum[i] ^= h[i]
			}

			sums[slot] = sum

			return nil
		}); err != nil {
			return err
		}

		for slot, sum := range sums {
			if err := xorDigest(d, slot, sum); err != nil {
				return err
			}
		}

		return d.Put(digestStamp, binary.BigEndian.AppendUint32(nil, uint32(n))) //nolint:gosec // n < 1<<16.
	})
}

// digests returns the kept digests of the given partitions, or of all of
// them when partitions is nil. A slot without rows is absent.
func (t *Table[R]) digests(l *layout.Layout, partitions []int) (map[Slot][32]byte, error) {
	n := len(l.Slots)
	if n == 0 {
		return map[Slot][32]byte{}, nil
	}

	if err := t.ensureDigests(n); err != nil {
		return nil, errors.Wrapf(err, "digests of %s", t.name)
	}

	out := map[Slot][32]byte{}

	add := func(k, v []byte) {
		var sum [32]byte

		copy(sum[:], v)
		out[keySlot(k)] = sum
	}

	err := t.db.View(func(tx *bbolt.Tx) error {
		d := tx.Bucket(t.digestBucket())

		if partitions == nil {
			return d.ForEach(func(k, v []byte) error {
				if len(k) == 3 {
					add(k, v)
				}

				return nil
			})
		}

		c := d.Cursor()

		for _, p := range partitions {
			prefix := slotKey(Slot{P: p})[:2]

			// The stamp's key may sort among a partition's slots; it is
			// skipped, not taken for the end.
			for k, v := c.Seek(prefix); len(k) >= 2 && k[0] == prefix[0] && k[1] == prefix[1]; k, v = c.Next() {
				if len(k) == 3 {
					add(k, v)
				}
			}
		}

		return nil
	})

	return out, err
}
