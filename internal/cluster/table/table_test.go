package table

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"

	"github.com/go-faster/fs/internal/cluster/layout"
	"github.com/go-faster/fs/internal/cluster/peer"
)

// reg is a last-writer-wins register: the higher timestamp wins, ties broken
// by value so the merge stays commutative.
type reg struct {
	V  string `json:"v"`
	TS int64  `json:"ts"`
}

func mergeReg(a, b reg) reg {
	if b.TS > a.TS || b.TS == a.TS && b.V > a.V {
		return b
	}

	return a
}

func TestMergeLaws(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	gen := func() reg { return reg{V: fmt.Sprint(r.IntN(4)), TS: r.Int64N(4)} }

	for range 1000 {
		a, b, c := gen(), gen(), gen()

		require.Equal(t, mergeReg(a, b), mergeReg(b, a), "commutative")
		require.Equal(t, mergeReg(mergeReg(a, b), c), mergeReg(a, mergeReg(b, c)), "associative")
		require.Equal(t, a, mergeReg(a, a), "idempotent")
	}
}

type node struct {
	member *peer.Member
	srv    *httptest.Server
	table  *Table[reg]
}

// cluster starts three nodes sharing one layout in which every node holds
// every partition, and lets them learn each other's addresses.
func cluster(t *testing.T) []*node {
	t.Helper()

	return clusterOf(t, Replicas, Replicas)
}

// roles are the layout roles of n nodes, each in its own zone.
func roles(n int) []layout.Node {
	var out []layout.Node
	for i := range n {
		out = append(out, layout.Node{ID: layout.NodeID(fmt.Sprint("n", i)), Zone: fmt.Sprint("z", i), Capacity: 1})
	}

	return out
}

// clusterOf starts n nodes, of which the first members hold data in the
// initial layout, and gossips until every node knows every peer by ID.
func clusterOf(t *testing.T, n, members int) []*node {
	t.Helper()

	all := roles(n)

	l, err := layout.Compute(nil, all[:members], layout.Options{Partitions: 8, Widths: []int{Replicas}})
	require.NoError(t, err)

	nodes := make([]*node, n)
	for i := range nodes {
		srv := httptest.NewUnstartedServer(nil)

		cfg := peer.Config{
			ID:     all[i].ID,
			Addr:   srv.Listener.Addr().String(),
			Secret: peer.Secret("cluster-secret-0123456789"),
			Dir:    t.TempDir(),
		}
		if i > 0 {
			cfg.Peers = []string{nodes[0].srv.Listener.Addr().String()}
		}

		m, err := peer.New(cfg)
		require.NoError(t, err)

		_, err = m.Adopt(l)
		require.NoError(t, err)

		db, err := bbolt.Open(filepath.Join(t.TempDir(), "meta.db"), 0o600, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })

		tbl, err := New("reg", db, m, mergeReg)
		require.NoError(t, err)

		srv.Config.Handler = m.Handler()
		srv.Start()
		t.Cleanup(srv.Close)

		nodes[i] = &node{member: m, srv: srv, table: tbl}
	}

	require.Eventually(t, func() bool {
		for _, nd := range nodes {
			nd.member.Round(context.Background())
		}

		for _, nd := range nodes {
			for _, other := range nodes {
				if _, ok := nd.member.Addr(other.member.ID()); !ok {
					return false
				}
			}
		}

		return true
	}, 5*time.Second, time.Millisecond)

	return nodes
}

func TestInsertGet(t *testing.T) {
	nodes := cluster(t)
	ctx := context.Background()

	require.NoError(t, nodes[0].table.Insert(ctx, "bucket", "key", reg{V: "one", TS: 1}))

	for i, nd := range nodes {
		got, ok, err := nd.table.Get(ctx, "bucket", "key")
		require.NoError(t, err, "node %d", i)
		require.True(t, ok)
		assert.Equal(t, "one", got.V)
	}

	// Concurrent writers through different nodes converge on the merge.
	require.NoError(t, nodes[1].table.Insert(ctx, "bucket", "key", reg{V: "two", TS: 2}))
	require.NoError(t, nodes[2].table.Insert(ctx, "bucket", "key", reg{V: "old", TS: 0}))

	got, _, err := nodes[0].table.Get(ctx, "bucket", "key")
	require.NoError(t, err)
	assert.Equal(t, "two", got.V)

	_, ok, err := nodes[0].table.Get(ctx, "bucket", "missing")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestRange(t *testing.T) {
	nodes := cluster(t)
	ctx := context.Background()

	for _, k := range "abcdefghij" {
		require.NoError(t, nodes[0].table.Insert(ctx, "b", string(k), reg{V: string(k), TS: 1}))
	}

	// A partition key that is a prefix of another must not leak into it.
	require.NoError(t, nodes[0].table.Insert(ctx, "bb", "a", reg{V: "other bucket"}))

	page, err := nodes[1].table.Range(ctx, "b", "c", 3)
	require.NoError(t, err)
	assert.Equal(t, []string{"c", "d", "e"}, sks(page))

	page, err = nodes[2].table.Range(ctx, "b", "h", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"h", "i", "j"}, sks(page))

	page, err = nodes[2].table.Range(ctx, "bb", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, sks(page))
}

func TestOneNodeDown(t *testing.T) {
	nodes := cluster(t)
	ctx := context.Background()

	nodes[2].srv.Close()

	require.NoError(t, nodes[0].table.Insert(ctx, "bucket", "key", reg{V: "v", TS: 1}), "write survives one replica down")

	got, ok, err := nodes[1].table.Get(ctx, "bucket", "key")
	require.NoError(t, err, "read survives one replica down")
	require.True(t, ok)
	assert.Equal(t, "v", got.V)

	page, err := nodes[1].table.Range(ctx, "bucket", "", 10)
	require.NoError(t, err)
	assert.Len(t, page, 1)
}

func TestTwoNodesDown(t *testing.T) {
	nodes := cluster(t)
	ctx := context.Background()

	nodes[1].srv.Close()
	nodes[2].srv.Close()

	require.ErrorIs(t, nodes[0].table.Insert(ctx, "bucket", "key", reg{V: "v"}), ErrQuorum)

	_, _, err := nodes[0].table.Get(ctx, "bucket", "key")
	require.ErrorIs(t, err, ErrQuorum)
}

func TestReadRepair(t *testing.T) {
	nodes := cluster(t)
	ctx := context.Background()

	// n1 missed a write and holds an older row; n2 has nothing at all.
	require.NoError(t, nodes[0].table.localInsert([]wireEntry{{PK: "b", SK: "k", Row: []byte(`{"v":"new","ts":2}`)}}))
	require.NoError(t, nodes[1].table.localInsert([]wireEntry{{PK: "b", SK: "k", Row: []byte(`{"v":"old","ts":1}`)}}))

	// Read until a quorum including n0 answers; the merge is the newest.
	require.Eventually(t, func() bool {
		got, ok, err := nodes[1].table.Get(ctx, "b", "k")

		return err == nil && ok && got.V == "new"
	}, 5*time.Second, 10*time.Millisecond)

	// Every replica that answered with less is brought up to date.
	require.Eventually(t, func() bool {
		resp, err := nodes[1].table.localGet("b", "k")

		return err == nil && string(resp.Row) == `{"v":"new","ts":2}`
	}, 5*time.Second, 10*time.Millisecond)
}

func TestNoLayout(t *testing.T) {
	m, err := peer.New(peer.Config{ID: "n0", Addr: "127.0.0.1:1", Secret: peer.Secret("cluster-secret-0123456789"), Dir: t.TempDir()})
	require.NoError(t, err)

	db, err := bbolt.Open(filepath.Join(t.TempDir(), "meta.db"), 0o600, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	tbl, err := New("reg", db, m, mergeReg)
	require.NoError(t, err)

	require.ErrorIs(t, tbl.Insert(context.Background(), "b", "k", reg{}), ErrNoLayout)
}

func sks(entries []Entry[reg]) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.SK
	}

	return out
}
