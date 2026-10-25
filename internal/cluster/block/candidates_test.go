package block

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dead(context.Context, Hash) (bool, error) { return false, nil }

// age makes every copy of the named files on nodes older than any grace.
func age(t *testing.T, nodes []*node, names ...string) {
	t.Helper()

	old := time.Now().Add(-time.Hour)

	for _, n := range nodes {
		for _, name := range names {
			if n.store.exists(name) {
				require.NoError(t, os.Chtimes(n.store.pathOf(name), old, old))
			}
		}
	}
}

func candidateCount(t *testing.T, n *node) int {
	t.Helper()

	cs, err := n.blocks.candidates(nil)
	require.NoError(t, err)

	return len(cs)
}

// TestGCCollectsCandidates checks that collection removes the blocks named
// as candidates on every replica, and only those.
func TestGCCollectsCandidates(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t, 3)

	released, kept := []byte("released"), []byte("still referenced")

	var hashes []Hash

	for _, data := range [][]byte{released, kept} {
		h, err := nodes[0].blocks.Put(ctx, data)
		require.NoError(t, err)

		hashes = append(hashes, h)
	}

	require.Eventually(t, func() bool {
		for _, n := range nodes {
			if !n.store.Has(hashes[0]) || !n.store.Has(hashes[1]) {
				return false
			}
		}

		return true
	}, 5*time.Second, time.Millisecond)

	age(t, nodes, hashes[0].String(), hashes[1].String())

	nodes[0].blocks.Unreferenced(ctx, []Candidate{{Hash: hashes[0]}})

	require.Eventually(t, func() bool {
		for _, n := range nodes {
			if candidateCount(t, n) != 1 {
				return false
			}
		}

		return true
	}, 5*time.Second, time.Millisecond, "every replica is told")

	for _, n := range nodes {
		removed, err := n.blocks.GC(ctx, time.Minute, dead)
		require.NoError(t, err)
		assert.Equal(t, 1, removed)

		assert.False(t, n.store.Has(hashes[0]))
		assert.True(t, n.store.Has(hashes[1]), "not a candidate: not looked at")
		assert.Zero(t, candidateCount(t, n), "checked, and dropped")
	}
}

// TestGCKeepsUncertainCandidates checks that a candidate is checked again
// when its block is too fresh or its references cannot be read, and dropped
// when the block is referenced again.
func TestGCKeepsUncertainCandidates(t *testing.T) {
	ctx := context.Background()
	n := clusterOf(t, 1, 1, 1)[0]

	h, err := n.blocks.Put(ctx, []byte("uncertain"))
	require.NoError(t, err)

	n.blocks.Unreferenced(ctx, []Candidate{{Hash: h}})

	removed, err := n.blocks.GC(ctx, time.Minute, dead)
	require.NoError(t, err)
	assert.Zero(t, removed)
	assert.True(t, n.store.Has(h), "too fresh")
	assert.Equal(t, 1, candidateCount(t, n))
	assert.Equal(t, int64(1), n.blocks.Stats().Candidates)

	age(t, []*node{n}, h.String())

	unreadable := func(context.Context, Hash) (bool, error) { return false, errors.New("refs unreachable") }
	removed, err = n.blocks.GC(ctx, time.Minute, unreadable)
	require.NoError(t, err)
	assert.Zero(t, removed)
	assert.Equal(t, 1, candidateCount(t, n), "references unreadable")

	referenced := func(context.Context, Hash) (bool, error) { return true, nil }
	removed, err = n.blocks.GC(ctx, time.Minute, referenced)
	require.NoError(t, err)
	assert.Zero(t, removed)
	assert.True(t, n.store.Has(h))
	assert.Zero(t, candidateCount(t, n), "referenced again: a later release names it again")
	assert.Zero(t, n.blocks.Stats().Candidates)
}

// TestGCKeepsCandidateNotedDuringCheck checks that a candidate named again
// while a pass checks it survives the pass.
func TestGCKeepsCandidateNotedDuringCheck(t *testing.T) {
	ctx := context.Background()
	n := clusterOf(t, 1, 1, 1)[0]

	h, err := n.blocks.Put(ctx, []byte("released twice"))
	require.NoError(t, err)

	age(t, []*node{n}, h.String())
	n.blocks.Unreferenced(ctx, []Candidate{{Hash: h}})

	// Referenced again and released again while the check runs.
	live := func(context.Context, Hash) (bool, error) {
		time.Sleep(time.Millisecond) // A later stamp.
		n.blocks.Unreferenced(ctx, []Candidate{{Hash: h}})

		return true, nil
	}

	_, err = n.blocks.GC(ctx, time.Minute, live)
	require.NoError(t, err)
	assert.Equal(t, 1, candidateCount(t, n))
}

// TestGCCollectsCodedCandidates checks that every shard holder is told,
// beyond the replica slots, and removes its shard.
func TestGCCollectsCodedCandidates(t *testing.T) {
	ctx := context.Background()
	nodes := clusterOf(t, 6, 6, 3, 6)
	s := Scheme{K: 4, M: 2}

	h, slots := putEverywhere(t, nodes, []byte("coded and released"), s)

	var names []string
	for i := range slots {
		names = append(names, Shard{Hash: h, K: s.K, M: s.M, I: i}.String())
	}

	age(t, nodes, names...)

	nodes[0].blocks.Unreferenced(ctx, []Candidate{{Hash: h, Scheme: s}})

	for i, id := range slots {
		n := byID(nodes, id)

		require.Eventually(t, func() bool { return candidateCount(t, n) == 1 }, 5*time.Second, time.Millisecond)

		removed, err := n.blocks.GC(ctx, time.Minute, dead)
		require.NoError(t, err)
		assert.Equal(t, 1, removed, "slot %d", i)
		assert.False(t, n.store.HasShard(Shard{Hash: h, K: s.K, M: s.M, I: i}))
	}
}
