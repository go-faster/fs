package table

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
)

// second adds a second table to every node of a cluster.
func second(t *testing.T, nodes []*node) []*Table[reg] {
	t.Helper()

	out := make([]*Table[reg], len(nodes))

	for i, n := range nodes {
		tbl, err := New("other", n.table.db, n.member, mergeReg, nil)
		require.NoError(t, err)

		out[i] = tbl
	}

	return out
}

func TestWriteAcrossTables(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t)
	others := second(t, nodes)

	a, err := nodes[0].table.Row("b", "k", reg{V: "in reg", TS: 1})
	require.NoError(t, err)

	b, err := others[0].Row("b", "k", reg{V: "in other", TS: 1})
	require.NoError(t, err)

	require.NoError(t, Write(ctx, nodes[0].member, a, b))

	require.Eventually(t, func() bool {
		for i := range nodes {
			if len(local(t, nodes[i])) != 1 {
				return false
			}

			row, ok, err := others[i].Get(ctx, "b", "k")
			if err != nil || !ok || row.V != "in other" {
				return false
			}
		}

		return true
	}, 5*time.Second, time.Millisecond)

	got, ok, err := nodes[2].table.Get(ctx, "b", "k")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "in reg", got.V)

	for i, n := range nodes {
		require.NoError(t, n.table.db.View(func(tx *bbolt.Tx) error {
			assert.NotNil(t, tx.Bucket([]byte("other")).Get(key("b", "k")), "node %d holds both rows", i)

			return nil
		}))
	}
}

func TestWriteQuorum(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t)
	others := second(t, nodes)

	row := func(v string) (Row, Row) {
		a, err := nodes[0].table.Row("b", v, reg{V: v, TS: 1})
		require.NoError(t, err)

		b, err := others[0].Row("b", v, reg{V: v, TS: 1})
		require.NoError(t, err)

		return a, b
	}

	nodes[2].srv.Close()

	a, b := row("one down")
	require.NoError(t, Write(ctx, nodes[0].member, a, b), "two of three replicas are a quorum")

	nodes[1].srv.Close()

	a, b = row("two down")
	require.ErrorIs(t, Write(ctx, nodes[0].member, a, b), ErrQuorum)
}
