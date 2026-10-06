package peer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// TestTransition: a newer layout keeps the one it replaces until every node
// holding data in either has synced it — the node that left included — and a
// node that never will is released with Skip. All of it survives a restart.
func TestTransition(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ID: "a", Addr: "127.0.0.1:1", Secret: testSecret, Dir: dir}

	m, err := New(cfg)
	require.NoError(t, err)

	v1 := testLayout(t, nil, "a", "b", "c", "d")
	_, err = m.Adopt(v1)
	require.NoError(t, err)
	assert.Len(t, m.Layouts(), 1, "a first layout has nothing to retain")

	v2 := testLayout(t, v1, "a", "b", "c") // d leaves.
	_, err = m.Adopt(v2)
	require.NoError(t, err)

	versions := func(m *Member) []uint64 {
		var out []uint64
		for _, l := range m.Layouts() {
			out = append(out, l.Version)
		}

		return out
	}

	require.Equal(t, []uint64{v1.Version, v2.Version}, versions(m), "the old version is retained")

	require.NoError(t, m.MarkSynced(v2.Version))

	m.mu.Lock()
	m.synced["b"], m.synced["c"] = v2.Version, v2.Version // As gossip would report, and persist.
	m.prune()
	require.NoError(t, m.persist())
	m.mu.Unlock()

	assert.Len(t, m.Layouts(), 2, "d held data in v1 and has not synced v2: its rows may not be handed over yet")

	// It survives a restart.
	restarted, err := New(cfg)
	require.NoError(t, err)
	require.Equal(t, versions(m), versions(restarted))

	require.NoError(t, restarted.Skip(layout.NodeID("d")))
	assert.Equal(t, []uint64{v2.Version}, versions(restarted), "skipping the node that will not sync retires v1")
}

// TestTransitionGossip: a node that is behind learns the retained versions
// along with the current one, and every node's sync spreads.
func TestTransitionGossip(t *testing.T) {
	nodes := cluster(t, 4)

	v1 := testLayout(t, nil, "n0", "n1", "n2", "n3")
	v2 := testLayout(t, v1, "n0", "n1", "n2") // n3 leaves.

	for _, n := range nodes[:3] {
		_, err := n.Adopt(v1)
		require.NoError(t, err)
	}

	_, err := nodes[0].Adopt(v2)
	require.NoError(t, err)

	// n3 never adopted anything; gossip gives it both versions.
	gossip(t, nodes, func() bool { return len(nodes[3].Layouts()) == 2 })

	for _, n := range nodes {
		require.NoError(t, n.MarkSynced(v2.Version))
	}

	gossip(t, nodes, func() bool {
		for _, n := range nodes {
			if len(n.Layouts()) != 1 {
				return false
			}
		}

		return true
	})
}
