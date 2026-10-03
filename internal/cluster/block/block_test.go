package block

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/layout"
	"github.com/go-faster/fs/internal/cluster/peer"
)

func TestStore(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)

	data := []byte("hello block")
	h := Sum(data)

	_, err = s.Get(h)
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, s.Put(h, data))
	assert.True(t, s.Has(h))

	got, err := s.Get(h)
	require.NoError(t, err)
	assert.Equal(t, data, got)

	require.NoError(t, s.Delete(h))
	require.NoError(t, s.Delete(h), "deleting an absent block is not an error")
	assert.False(t, s.Has(h))
}

func TestStoreConcurrentSameBlock(t *testing.T) {
	// Identical content written at once: dedup by name means every writer
	// targets one file, and all of them must succeed.
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)

	data := bytes.Repeat([]byte("same"), 1<<16)
	h := Sum(data)

	var wg sync.WaitGroup

	errs := make(chan error, 16)

	for range 16 {
		wg.Go(func() { errs <- s.Put(h, data) })
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	got, err := s.Get(h)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestStoreFormat(t *testing.T) {
	dir := t.TempDir()

	s, err := NewStore(dir)
	require.NoError(t, err)

	data := []byte("stamped")
	require.NoError(t, s.Put(Sum(data), data))

	_, err = NewStore(dir)
	require.NoError(t, err, "a store reopens over its own format")

	// Blocks without the stamp are a format this binary does not know.
	unstamped := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(unstamped, "ab"), 0o750))

	_, err = NewStore(unstamped)
	require.ErrorContains(t, err, "unknown format")

	require.NoError(t, os.WriteFile(filepath.Join(dir, formatFile), []byte("future\n"), 0o600))

	_, err = NewStore(dir)
	require.ErrorContains(t, err, "has format")
}

func TestPeerPutMismatchRefused(t *testing.T) {
	nodes := cluster(t, 3)
	h := Sum([]byte("claimed"))

	status, _, err := nodes[0].member.Raw(context.Background(), nodes[1].member.ID(), "PUT", "/v1/block/"+h.String(), []byte("actual"))
	require.NoError(t, err)
	assert.Equal(t, 400, status, "a peer stores nothing under a hash its bytes do not have")
	assert.False(t, nodes[1].store.Has(h))
}

func TestStoreCorrupt(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)

	data := []byte("will rot")
	h := Sum(data)
	require.NoError(t, s.Put(h, data))
	require.NoError(t, os.WriteFile(s.path(h), []byte("rotted!!"), 0o600))

	_, err = s.Get(h)
	require.ErrorIs(t, err, ErrCorrupt)
	assert.False(t, s.Has(h), "a corrupt copy is dropped so it reads as missing")
}

func TestStorePutTouches(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)

	data := []byte("dedup")
	h := Sum(data)
	require.NoError(t, s.Put(h, data))

	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(s.path(h), old, old))

	require.NoError(t, s.Put(h, data))

	info, err := os.Stat(s.path(h))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), info.ModTime(), time.Minute,
		"re-putting a block must protect it from GC like a fresh one")
}

func TestStoreWalk(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	data := []byte("walk me")
	require.NoError(t, s.Put(Sum(data), data))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "leftover.123"+tmpSuffix), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README"), nil, 0o600))

	var blocks, tmps int

	require.NoError(t, s.Walk(func(h Hash, tmp bool, _ time.Time, _ string) error {
		if tmp {
			tmps++
		} else {
			blocks++

			assert.Equal(t, Sum(data), h)
		}

		return nil
	}))

	assert.Equal(t, 1, blocks)
	assert.Equal(t, 1, tmps)
}

func TestParseHash(t *testing.T) {
	h := Sum([]byte("x"))

	got, err := ParseHash(h.String())
	require.NoError(t, err)
	assert.Equal(t, h, got)

	_, err = ParseHash("abc")
	require.Error(t, err)
}

type node struct {
	member *peer.Member
	srv    *httptest.Server
	store  *Store
	blocks *Manager
}

// cluster starts n nodes sharing a layout with three slots per partition.
func cluster(t *testing.T, n int) []*node {
	t.Helper()

	return clusterOf(t, n, n)
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
// initial layout, spread for widths when given.
func clusterOf(t *testing.T, n, members int, widths ...int) []*node {
	t.Helper()

	roles := roles(n)

	l, err := layout.Compute(nil, roles[:members], layout.Options{Partitions: 8, Widths: widths})
	require.NoError(t, err)

	nodes := make([]*node, n)
	for i := range nodes {
		srv := httptest.NewUnstartedServer(nil)

		cfg := peer.Config{
			ID:     roles[i].ID,
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

		store, err := NewStore(t.TempDir())
		require.NoError(t, err)

		blocks := NewManager(store, m)

		srv.Config.Handler = m.Handler()
		srv.Start()
		t.Cleanup(srv.Close)

		nodes[i] = &node{member: m, srv: srv, store: store, blocks: blocks}
	}

	// Gossip until every node knows every peer by ID, which takes a round
	// or two more than learning addresses.
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

func byID(nodes []*node, id layout.NodeID) *node {
	for _, n := range nodes {
		if n.member.ID() == id {
			return n
		}
	}

	return nil
}

func TestPutGet(t *testing.T) {
	nodes := cluster(t, 3)
	ctx := context.Background()

	data := []byte("replicated block")
	h, err := nodes[0].blocks.Put(ctx, data)
	require.NoError(t, err)

	// Every replica gets it; the last may land just after Put returns.
	require.Eventually(t, func() bool {
		for _, n := range nodes {
			if !n.store.Has(h) {
				return false
			}
		}

		return true
	}, 5*time.Second, 10*time.Millisecond)

	for _, n := range nodes {
		got, err := n.blocks.Get(ctx, h)
		require.NoError(t, err)
		assert.Equal(t, data, got)
	}

	_, err = nodes[0].blocks.Get(ctx, Sum([]byte("never stored")))
	require.ErrorIs(t, err, ErrNotFound)
}

func TestNonReplicaCoordinator(t *testing.T) {
	// With four nodes and three slots, each block skips one node, which
	// must still be able to store and read it.
	nodes := cluster(t, 4)
	ctx := context.Background()

	for i := range 32 {
		data := fmt.Appendf(nil, "block %d", i)

		for _, n := range nodes {
			h, err := n.blocks.Put(ctx, data)
			require.NoError(t, err)

			got, err := n.blocks.Get(ctx, h)
			require.NoError(t, err)
			require.Equal(t, data, got)
		}
	}
}

func TestOneNodeDown(t *testing.T) {
	nodes := cluster(t, 3)
	ctx := context.Background()

	nodes[2].srv.Close()

	h, err := nodes[0].blocks.Put(ctx, []byte("written while n2 is down"))
	require.NoError(t, err, "two of three replicas are a quorum")

	got, err := nodes[1].blocks.Get(ctx, h)
	require.NoError(t, err)
	assert.Equal(t, "written while n2 is down", string(got))

	require.Eventually(t, func() bool {
		return nodes[0].blocks.Stats().ResyncPending == 1
	}, 5*time.Second, 10*time.Millisecond, "the missed replica is queued for resync")
}

func TestTwoNodesDown(t *testing.T) {
	nodes := cluster(t, 3)

	nodes[1].srv.Close()
	nodes[2].srv.Close()

	_, err := nodes[0].blocks.Put(context.Background(), []byte("no quorum"))
	require.ErrorIs(t, err, ErrQuorum)
}

func TestResyncRepairsMissingAndCorrupt(t *testing.T) {
	nodes := cluster(t, 3)
	ctx := context.Background()

	data := []byte("repair me")
	h, err := nodes[0].blocks.Put(ctx, data)
	require.NoError(t, err)

	// Put returns at quorum; let every replica land, the coordinator's own
	// included, before breaking two of them.
	require.Eventually(t, func() bool {
		for _, n := range nodes {
			if !n.store.Has(h) {
				return false
			}
		}

		return true
	}, 5*time.Second, 10*time.Millisecond)

	// n1 lost its copy; n2's rotted.
	require.NoError(t, nodes[1].store.Delete(h))
	require.NoError(t, os.WriteFile(nodes[2].store.path(h), []byte("rot"), 0o600))

	for _, n := range nodes[1:] {
		got, err := n.blocks.Get(ctx, h)
		require.NoError(t, err, "a read finds a good copy elsewhere")
		require.Equal(t, data, got)
	}

	for _, n := range nodes {
		n.blocks.Resync(ctx)
	}

	for _, n := range nodes {
		got, err := n.store.Get(h)
		require.NoError(t, err, "%s holds a good copy again", n.member.ID())
		assert.Equal(t, data, got)
	}

	assert.Equal(t, int64(1), nodes[2].blocks.Stats().Corrupt)
}

func TestGC(t *testing.T) {
	nodes := cluster(t, 3)
	ctx := context.Background()
	n := nodes[0]

	put := func(s string) Hash {
		h := Sum([]byte(s))
		require.NoError(t, n.store.Put(h, []byte(s)))

		return h
	}

	referenced, orphan, fresh, unknown := put("referenced"), put("orphan"), put("fresh"), put("unknown")

	old := time.Now().Add(-time.Hour)
	for _, h := range []Hash{referenced, orphan, unknown} {
		require.NoError(t, os.Chtimes(n.store.path(h), old, old))
	}

	tmp := filepath.Join(filepath.Dir(n.store.path(orphan)), "x"+tmpSuffix)
	require.NoError(t, os.WriteFile(tmp, nil, 0o600))
	require.NoError(t, os.Chtimes(tmp, old, old))

	removed, err := n.blocks.GC(ctx, DefaultGrace, func(_ context.Context, h Hash) (bool, error) {
		switch h {
		case referenced:
			return true, nil
		case unknown:
			return false, errors.New("refs unreachable")
		default:
			return false, nil
		}
	})
	require.NoError(t, err)
	assert.Equal(t, 1, removed)

	assert.True(t, n.store.Has(referenced))
	assert.False(t, n.store.Has(orphan))
	assert.True(t, n.store.Has(fresh), "a block within grace is kept: its references may not be written yet")
	assert.True(t, n.store.Has(unknown), "a block whose references cannot be checked is kept")
	assert.NoFileExists(t, tmp)
	assert.Equal(t, int64(1), n.blocks.Stats().Collected)
}

func TestMaxSize(t *testing.T) {
	nodes := cluster(t, 3)

	_, err := nodes[0].blocks.Put(context.Background(), bytes.Repeat([]byte{1}, MaxSize+1))
	require.Error(t, err)
}

// TestGCSparesBlockTouchedDuringCheck: a writer that re-references an old
// block touches it after GC read its age but before GC deletes it. GC must
// see the touch and keep the block, or the new reference points at nothing.
func TestGCSparesBlockTouchedDuringCheck(t *testing.T) {
	ctx := context.Background()
	n := clusterOf(t, 1, 1, 1)[0]
	data := []byte("re-referenced while being collected")

	h, err := n.blocks.Put(ctx, data)
	require.NoError(t, err)

	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(n.store.path(h), old, old))

	// The check runs, a writer stores the same block meanwhile, and the
	// check's answer — taken before that writer's reference — is "dead".
	live := func(context.Context, Hash) (bool, error) {
		require.NoError(t, n.store.Put(h, data))

		return false, nil
	}

	removed, err := n.blocks.GC(ctx, time.Minute, live)
	require.NoError(t, err)
	assert.Zero(t, removed)
	assert.True(t, n.store.Has(h), "a block touched during the check is kept")
}
