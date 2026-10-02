package engine

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/block"
	"github.com/go-faster/fs/internal/cluster/layout"
	"github.com/go-faster/fs/internal/cluster/peer"
	"github.com/go-faster/fs/internal/cluster/table"
	"github.com/go-faster/fs/storagetest"
)

// node is one engine with everything under it.
type node struct {
	member *peer.Member
	srv    *httptest.Server
	engine *Engine
}

// cluster starts n nodes, each in its own zone, sharing a layout with
// min(n, 3) slots per partition, and gossips until every node knows every
// peer. A one-node cluster is how a single server runs.
func cluster(t testing.TB, n int, cfg Config) []*node {
	t.Helper()

	var roles []layout.Node
	for i := range n {
		roles = append(roles, layout.Node{ID: layout.NodeID(fmt.Sprint("n", i)), Zone: fmt.Sprint("z", i), Capacity: 1})
	}

	l, err := layout.Compute(nil, roles, layout.Options{Partitions: 16, Widths: []int{min(n, 3)}})
	require.NoError(t, err)

	nodes := make([]*node, n)
	for i := range nodes {
		srv := httptest.NewUnstartedServer(nil)

		pc := peer.Config{
			ID:     roles[i].ID,
			Addr:   srv.Listener.Addr().String(),
			Secret: peer.Secret("cluster-secret-0123456789"),
			Dir:    t.TempDir(),
		}
		if i > 0 {
			pc.Peers = []string{nodes[0].srv.Listener.Addr().String()}
		}

		m, err := peer.New(pc)
		require.NoError(t, err)

		_, err = m.Adopt(l)
		require.NoError(t, err)

		db, err := table.OpenDB(filepath.Join(t.TempDir(), "meta.db"))
		require.NoError(t, err)

		t.Cleanup(func() { _ = db.Close() })

		store, err := block.NewStore(t.TempDir())
		require.NoError(t, err)

		c := cfg
		c.Member, c.DB, c.Blocks = m, db, block.NewManager(store, m)

		e, err := New(c)
		require.NoError(t, err)

		srv.Config.Handler = m.Handler()
		srv.Start()
		t.Cleanup(srv.Close)

		nodes[i] = &node{member: m, srv: srv, engine: e}
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

// small makes every non-trivial object span several blocks, so the
// conformance suite exercises the block path, not only inline content.
var small = Config{BlockSize: 64, InlineLimit: 16}

func TestConformanceSingleNode(t *testing.T) {
	storagetest.Run(t, func(t testing.TB) fs.Storage {
		return cluster(t, 1, small)[0].engine
	})
}

func TestConformanceCluster(t *testing.T) {
	storagetest.Run(t, func(t testing.TB) fs.Storage {
		return cluster(t, 3, small)[0].engine
	})
}
