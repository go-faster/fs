package peer

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// node is a Member serving on a test listener.
type node struct {
	*Member
	srv *httptest.Server
}

// cluster starts n members. Every one but the first knows only the first, as
// a node joining with a single bootstrap address does.
func cluster(t *testing.T, n int) []node {
	t.Helper()

	nodes := make([]node, n)
	for i := range nodes {
		srv := httptest.NewUnstartedServer(nil)

		cfg := Config{
			ID:     layout.NodeID(fmt.Sprintf("n%d", i)),
			Addr:   srv.Listener.Addr().String(),
			Secret: testSecret,
			Dir:    t.TempDir(),
		}
		if i > 0 {
			cfg.Peers = []string{nodes[0].srv.Listener.Addr().String()}
		}

		m, err := New(cfg)
		require.NoError(t, err)

		srv.Config.Handler = m.Handler()
		srv.Start()
		t.Cleanup(srv.Close)

		nodes[i] = node{Member: m, srv: srv}
	}

	return nodes
}

func testLayout(t *testing.T, prev *layout.Layout, ids ...string) *layout.Layout {
	t.Helper()

	var members []layout.Node
	for i, id := range ids {
		members = append(members, layout.Node{ID: layout.NodeID(id), Zone: fmt.Sprint("z", i%3), Capacity: 1 << 40})
	}

	l, err := layout.Compute(prev, members, layout.Options{Partitions: 16})
	require.NoError(t, err)

	return l
}

// gossip runs rounds on every node until cond holds.
func gossip(t *testing.T, nodes []node, cond func() bool) {
	t.Helper()

	for range 10 {
		if cond() {
			return
		}

		for _, n := range nodes {
			n.Round(context.Background())
		}
	}

	require.True(t, cond(), "did not converge in 10 rounds")
}

func TestClusterConverges(t *testing.T) {
	nodes := cluster(t, 3)

	// Apply on a node nobody was told about: it must still reach everyone.
	l := testLayout(t, nil, "n0", "n1", "n2")
	adopted, err := nodes[2].Adopt(l)
	require.NoError(t, err)
	require.True(t, adopted)

	gossip(t, nodes, func() bool {
		for _, n := range nodes {
			if got := n.Layout(); got == nil || got.Version != l.Version {
				return false
			}
		}

		return true
	})

	for _, n := range nodes {
		assert.Equal(t, l.Slots, n.Layout().Slots)
	}

	// Each node learned the others, including the two that started knowing
	// only n0; one more round exchanges with the peers learned last.
	for _, n := range nodes {
		n.Round(context.Background())
	}

	for i, n := range nodes {
		peers := n.Peers()
		require.Len(t, peers, 2, "node %d", i)

		for _, p := range peers {
			assert.NoError(t, p.Err)
			assert.False(t, p.Seen.IsZero())
			assert.Equal(t, l.Version, p.Version)
		}
	}

	// A newer layout applied anywhere replaces it everywhere.
	next := testLayout(t, l, "n0", "n1", "n2", "n3")
	_, err = nodes[0].Adopt(next)
	require.NoError(t, err)

	gossip(t, nodes, func() bool {
		for _, n := range nodes {
			if n.Layout().Version != next.Version {
				return false
			}
		}

		return true
	})
}

func TestPeerDown(t *testing.T) {
	nodes := cluster(t, 2)
	nodes[1].Round(context.Background())

	nodes[0].srv.Close()
	nodes[1].Round(context.Background())

	p := nodes[1].Peers()[0]
	assert.Error(t, p.Err)
	assert.False(t, p.Seen.IsZero(), "the last successful exchange is kept")
}

func TestAdopt(t *testing.T) {
	m := cluster(t, 1)[0]

	l := testLayout(t, nil, "a", "b", "c")
	ok, err := m.Adopt(l)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = m.Adopt(l)
	require.NoError(t, err)
	assert.False(t, ok, "the same layout again is not newer")

	older := *l
	older.Version = 0
	ok, err = m.Adopt(&older)
	require.NoError(t, err)
	assert.False(t, ok)

	bad := testLayout(t, l, "a", "b", "c")
	bad.Slots[0][0] = "ghost"
	_, err = m.Adopt(bad)
	require.Error(t, err)

	other, err := layout.Compute(nil, l.Nodes, layout.Options{Partitions: 32})
	require.NoError(t, err)

	other.Version = 99
	_, err = m.Adopt(other)
	require.Error(t, err, "a layout of another partition count is another cluster")
}

func TestAdoptTieBreak(t *testing.T) {
	// Two layouts of one version, applied concurrently on two nodes: both
	// nodes must keep the same one, whichever arrives first.
	base := testLayout(t, nil, "a", "b", "c")
	x := testLayout(t, base, "a", "b", "c", "d")
	y := testLayout(t, base, "a", "b", "c", "e")
	require.Equal(t, x.Version, y.Version)

	m1, m2 := cluster(t, 1)[0], cluster(t, 1)[0]

	for _, l := range []*layout.Layout{x, y} {
		_, err := m1.Adopt(l)
		require.NoError(t, err)
	}

	for _, l := range []*layout.Layout{y, x} {
		_, err := m2.Adopt(l)
		require.NoError(t, err)
	}

	assert.Equal(t, m1.Layout(), m2.Layout())
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ID: "n0", Addr: "127.0.0.1:1", Secret: testSecret, Dir: dir}

	m, err := New(cfg)
	require.NoError(t, err)
	assert.Nil(t, m.Layout())

	l := testLayout(t, nil, "a", "b", "c")
	_, err = m.Adopt(l)
	require.NoError(t, err)

	restarted, err := New(cfg)
	require.NoError(t, err)
	assert.Equal(t, l, restarted.Layout())

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp*"))
	require.NoError(t, err)
	assert.Empty(t, matches, "no temporary file is left behind")
}

func TestPersistenceRefusesUnknown(t *testing.T) {
	for name, content := range map[string]string{
		"other format": `{"format": 2, "layout": {}}`,
		"not json":     `{`,
		"no layout":    `{"format": 1}`,
		"invalid":      `{"format": 1, "layout": {"version": 1, "widths": [3], "slots": [["x","y","z"]]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, layoutFile), []byte(content), 0o600))

			_, err := New(Config{ID: "n0", Addr: "127.0.0.1:1", Secret: testSecret, Dir: dir})
			require.Error(t, err)
		})
	}
}

func TestNewValidates(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no id":        {Addr: "a:1", Secret: testSecret},
		"no address":   {ID: "n0", Secret: testSecret},
		"short secret": {ID: "n0", Addr: "a:1", Secret: Secret("short")},
	} {
		t.Run(name, func(t *testing.T) {
			cfg.Dir = t.TempDir()
			_, err := New(cfg)
			require.Error(t, err)
		})
	}
}

func TestRunStops(t *testing.T) {
	m := cluster(t, 1)[0]

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		m.cfg.Interval = time.Millisecond
		m.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestApply(t *testing.T) {
	m := cluster(t, 1)[0]
	roles := testLayout(t, nil, "a", "b", "c").Nodes

	first, moved, err := m.Apply(roles, layout.Options{Partitions: 16}, true)
	require.NoError(t, err)
	assert.Zero(t, moved)
	assert.Nil(t, m.Layout(), "a dry run adopts nothing")

	_, _, err = m.Apply(roles, layout.Options{Partitions: 16}, false)
	require.NoError(t, err)
	assert.Equal(t, first.Slots, m.Layout().Slots)

	grown := append(slices.Clone(roles), layout.Node{ID: "d", Zone: "z0", Capacity: 1 << 40})
	next, moved, err := m.Apply(grown, layout.Options{}, false)
	require.NoError(t, err)
	assert.Equal(t, first.Version+1, next.Version)
	assert.Equal(t, layout.Moved(first, next), moved)
	assert.Positive(t, moved)
	assert.Len(t, next.Slots, 16, "partitions are kept")

	_, _, err = m.Apply(roles[:2], layout.Options{}, false)
	require.Error(t, err, "two members cannot hold three slots")
}
