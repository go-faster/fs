package table

import (
	"context"
	"encoding/binary"

	"github.com/go-faster/errors"
	"go.etcd.io/bbolt"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// Usage tallies: a table can keep, per partition key, a total of what its
// rows hold — how many objects a bucket has and their bytes. Each replica
// keeps its own tally of the rows it stores, updated in the transaction that
// changes them, so it is exact for that replica: a write, a merge, a
// compaction and a handover each move it by the difference they make.
// Replicas converge, and so do their tallies; a read asks a quorum.

// Usage is a partition's tally.
type Usage struct {
	Items int64 `json:"items"`
	Bytes int64 `json:"bytes"`
}

// Add returns u plus o.
func (u Usage) Add(o Usage) Usage { return Usage{Items: u.Items + o.Items, Bytes: u.Bytes + o.Bytes} }

// Sub returns u minus o.
func (u Usage) Sub(o Usage) Usage { return Usage{Items: u.Items - o.Items, Bytes: u.Bytes - o.Bytes} }

func (t *Table[R]) usageBucket() []byte { return []byte(t.name + ".usage") }

// CountUsage makes the table tally fn over its rows per partition key, and
// serves the tallies to peers; call it once, after New, before serving. A
// table that kept no tallies before has them counted from its rows first.
func (t *Table[R]) CountUsage(fn func(R) Usage) error {
	t.usage = fn

	if err := t.db.Update(func(tx *bbolt.Tx) error {
		if tx.Bucket(t.usageBucket()) != nil {
			return nil
		}

		u, err := tx.CreateBucket(t.usageBucket())
		if err != nil {
			return err
		}

		return t.backfill(tx, u)
	}); err != nil {
		return errors.Wrapf(err, "tally %s", t.name)
	}

	t.member.Handle("POST /v1/table/"+t.name+"/usage", handle(t.serveUsage))

	return nil
}

// backfill tallies every row the table holds into u.
func (t *Table[R]) backfill(tx *bbolt.Tx, u *bbolt.Bucket) error {
	b := tx.Bucket([]byte(t.name))
	if b == nil {
		return nil
	}

	sums := map[string]Usage{}

	if err := b.ForEach(func(k, v []byte) error {
		pk, _, ok := splitKey(k)
		if !ok {
			return errors.Errorf("malformed %s key %x", t.name, k)
		}

		row, err := t.decode(v)
		if err != nil {
			return err
		}

		sums[pk] = sums[pk].Add(t.usage(row))

		return nil
	}); err != nil {
		return err
	}

	for pk, d := range sums {
		if err := addUsage(u, pk, d); err != nil {
			return err
		}
	}

	return nil
}

// tally moves pk's tally by what replacing prev with row changes, within
// the transaction of the change; either may be nil.
func (t *Table[R]) tally(tx *bbolt.Tx, pk string, prev, row *R) error {
	if t.usage == nil {
		return nil
	}

	var d Usage
	if row != nil {
		d = d.Add(t.usage(*row))
	}

	if prev != nil {
		d = d.Sub(t.usage(*prev))
	}

	if d == (Usage{}) {
		return nil
	}

	return addUsage(tx.Bucket(t.usageBucket()), pk, d)
}

// addUsage adds d to pk's tally in b. Tallies are keyed as rows of pk with
// no sort key, which is never empty, as bbolt requires.
func addUsage(b *bbolt.Bucket, pk string, d Usage) error {
	k := key(pk, "")

	cur := decodeUsage(b.Get(k)).Add(d)
	if cur == (Usage{}) {
		return b.Delete(k)
	}

	v := binary.BigEndian.AppendUint64(nil, uint64(cur.Items)) //nolint:gosec // Round-trips through decodeUsage.
	v = binary.BigEndian.AppendUint64(v, uint64(cur.Bytes))    //nolint:gosec // Round-trips through decodeUsage.

	if err := b.Put(k, v); err != nil {
		return errors.Wrap(err, "put usage")
	}

	return nil
}

func decodeUsage(v []byte) Usage {
	if len(v) != 16 {
		return Usage{}
	}

	return Usage{
		Items: int64(binary.BigEndian.Uint64(v)),     //nolint:gosec // Written by addUsage.
		Bytes: int64(binary.BigEndian.Uint64(v[8:])), //nolint:gosec // Written by addUsage.
	}
}

// LocalUsage returns this replica's tally of pk.
func (t *Table[R]) LocalUsage(pk string) (Usage, error) {
	if t.usage == nil {
		return Usage{}, errors.Errorf("table %s keeps no usage", t.name)
	}

	var u Usage

	err := t.db.View(func(tx *bbolt.Tx) error {
		u = decodeUsage(tx.Bucket(t.usageBucket()).Get(key(pk, "")))

		return nil
	})

	return u, err
}

// Usage returns pk's tally as a quorum of its replicas holds it: the
// largest of their answers, field by field. A replica that missed a write
// answers less, one that missed a delete answers more; the quorum includes
// one that has every acknowledged write, so the result undercounts nothing
// acknowledged, and may still count what was deleted until anti-entropy
// reaches the rest.
func (t *Table[R]) Usage(ctx context.Context, pk string) (Usage, error) {
	nodes, err := t.readSet(pk)
	if err != nil {
		return Usage{}, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	answers, err := quorum(nodes, len(nodes)/2+1, func(id layout.NodeID) (Usage, error) {
		var resp Usage

		err := t.on(ctx, id, "usage", usageReq{PK: pk}, &resp, func() (any, error) {
			return t.LocalUsage(pk)
		})

		return resp, err
	})
	if err != nil {
		return Usage{}, err
	}

	var out Usage
	for _, a := range answers {
		out.Items = max(out.Items, a.Items)
		out.Bytes = max(out.Bytes, a.Bytes)
	}

	return out, nil
}

type usageReq struct {
	PK string `json:"pk"`
}

func (t *Table[R]) serveUsage(req usageReq) (any, error) { return t.LocalUsage(req.PK) }
