package engine

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAccessKeysAreClusterWide: a key stored through one node is listed by
// every other, a delete through another removes it everywhere, and the same ID
// can be stored again after it was deleted — an imported credential keeps its
// ID across a delete and a re-create.
func TestAccessKeysAreClusterWide(t *testing.T) {
	nodes := cluster(t, 3, Config{})
	ctx := t.Context()

	record := json.RawMessage(`{"secret_key":"s1"}`)
	require.NoError(t, nodes[0].engine.PutAccessKey(ctx, "AKIAONE", record))

	for i, n := range nodes {
		keys, err := n.engine.AccessKeys(ctx)
		require.NoError(t, err)
		assert.JSONEq(t, string(record), string(keys["AKIAONE"]), "node %d", i)
	}

	require.NoError(t, nodes[1].engine.DeleteAccessKey(ctx, "AKIAONE"))

	for i, n := range nodes {
		keys, err := n.engine.AccessKeys(ctx)
		require.NoError(t, err)
		assert.NotContains(t, keys, "AKIAONE", "node %d still lists a deleted key", i)
	}

	again := json.RawMessage(`{"secret_key":"s2"}`)
	require.NoError(t, nodes[2].engine.PutAccessKey(ctx, "AKIAONE", again))

	keys, err := nodes[0].engine.AccessKeys(ctx)
	require.NoError(t, err)
	assert.JSONEq(t, string(again), string(keys["AKIAONE"]), "a deleted ID could not be stored again")
}

// TestAccessKeysOnASingleNode: a single server keeps them too, in its own
// one-node layout — there is no separate file for it.
func TestAccessKeysOnASingleNode(t *testing.T) {
	n := cluster(t, 1, Config{})[0]

	require.NoError(t, n.engine.PutAccessKey(t.Context(), "AKIASOLO", json.RawMessage(`{"secret_key":"s"}`)))

	keys, err := n.engine.AccessKeys(t.Context())
	require.NoError(t, err)
	assert.Contains(t, keys, "AKIASOLO")
}
