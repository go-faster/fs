package block

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/layout"
)

const testPartitions = 8

// walked recomputes a store's digests from its files.
func walked(t *testing.T, s *Store, n int) (blocks map[Slot][32]byte, shards map[ShardSlot][32]byte) {
	t.Helper()

	blocks, shards = map[Slot][32]byte{}, map[ShardSlot][32]byte{}

	require.NoError(t, s.Walk(func(h Hash, tmp bool, _ time.Time, _ string) error {
		if !tmp {
			xorInto(blocks, Slot{P: layout.PartitionOf(h[:], n), S: h[len(h)-1]}, h)
		}

		return nil
	}))
	require.NoError(t, s.WalkShards(func(sh Shard, _ time.Time) error {
		slot := Slot{P: layout.PartitionOf(sh.Hash[:], n), S: sh.Hash[len(sh.Hash)-1]}
		xorInto(shards, ShardSlot{Slot: slot, K: sh.K, M: sh.M, I: sh.I}, sh.Hash)

		return nil
	}))

	return blocks, shards
}

func requireDigestsKept(t *testing.T, s *Store, n int) {
	t.Helper()

	wantBlocks, wantShards := walked(t, s, n)

	blocks, err := s.BlockDigests(n, nil)
	require.NoError(t, err)
	assert.Equal(t, wantBlocks, blocks, "block digests")

	shards, err := s.ShardDigests(n, nil)
	require.NoError(t, err)
	assert.Equal(t, wantShards, shards, "shard digests")

	parts := map[int]bool{}
	for slot := range wantBlocks {
		parts[slot.P] = true
	}

	got, err := s.BlockPartitions(n)
	require.NoError(t, err)
	assert.Len(t, got, len(parts), "partitions holding blocks")
}

func TestDigestsKept(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)

	s.NoSync = true

	var hashes []Hash

	put := func(i int) {
		data := fmt.Appendf(nil, "block %d", i)
		h := Sum(data)
		require.NoError(t, s.Put(h, data))
		require.NoError(t, s.PutShard(Shard{Hash: h, K: 2, M: 1, I: i % 3}, data))

		hashes = append(hashes, h)
	}

	for i := range 50 {
		put(i)
	}

	requireDigestsKept(t, s, testPartitions) // Built by a walk.

	for i := 50; i < 100; i++ {
		put(i)
	}

	// Writing a block again only touches it.
	require.NoError(t, s.Put(hashes[0], []byte("block 0")))
	requireDigestsKept(t, s, testPartitions)

	for i, h := range hashes[:10] {
		require.NoError(t, s.Delete(h))
		require.NoError(t, s.DeleteShard(Shard{Hash: h, K: 2, M: 1, I: i % 3}))
	}

	require.NoError(t, s.Delete(hashes[0]), "deleting an absent block")
	requireDigestsKept(t, s, testPartitions)

	// A corrupt copy is dropped on read, by either read path.
	require.NoError(t, os.WriteFile(s.path(hashes[10]), []byte("rotten bits"), 0o600))
	_, err = s.Get(hashes[10])
	require.ErrorIs(t, err, ErrCorrupt)

	require.NoError(t, os.WriteFile(s.path(hashes[11]), []byte("rotten bits"), 0o600))
	_, err = s.GetInto(hashes[11], nil)
	require.ErrorIs(t, err, ErrCorrupt)
	requireDigestsKept(t, s, testPartitions)

	// GC's removal.
	deleted, err := s.deleteIfOlder(hashes[12].String(), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, deleted)
	requireDigestsKept(t, s, testPartitions)

	// Another partition count is rebuilt, not read as this one.
	requireDigestsKept(t, s, 3)
}

// TestDigestsBuiltDuringWrites checks that a file placed or removed while the
// first build walks is counted exactly once.
func TestDigestsBuiltDuringWrites(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)

	s.NoSync = true

	for i := range 500 {
		data := fmt.Appendf(nil, "old %d", i)
		require.NoError(t, s.Put(Sum(data), data))
	}

	var wg sync.WaitGroup

	wg.Go(func() {
		for i := range 500 {
			data := fmt.Appendf(nil, "new %d", i)
			assert.NoError(t, s.Put(Sum(data), data))

			old := fmt.Appendf(nil, "old %d", i)
			assert.NoError(t, s.Delete(Sum(old)))
		}
	})

	_, err = s.BlockDigests(testPartitions, nil)
	require.NoError(t, err)

	wg.Wait()
	requireDigestsKept(t, s, testPartitions)
}

// TestRepairMixedSchemes checks that digests compare per scheme: beside a
// wide code, a narrow one's missing shard is found, though the slots beyond
// its width hold nothing of it.
func TestRepairMixedSchemes(t *testing.T) {
	ctx := context.Background()
	nodes := clusterOf(t, 6, 6, 3, 6)
	narrow, wide := Scheme{K: 2, M: 1}, Scheme{K: 4, M: 2}

	h, slots := putEverywhere(t, nodes, bytes.Repeat([]byte("narrow "), 50000), narrow)
	putEverywhere(t, nodes, bytes.Repeat([]byte("wide "), 50000), wide)

	lost := byID(nodes, slots[1])
	require.NoError(t, lost.store.DeleteShard(Shard{Hash: h, K: 2, M: 1, I: 1}))

	first := byID(nodes, slots[0])
	require.NoError(t, first.blocks.RepairShards(ctx, alive))
	assert.Equal(t, 1, first.blocks.ShardStats().Critical, "two of three is K")

	require.NoError(t, lost.blocks.RepairShards(ctx, alive))
	assert.Equal(t, int64(1), lost.blocks.ShardStats().Rebuilt)

	require.NoError(t, first.blocks.RepairShards(ctx, alive))
	assert.Zero(t, first.blocks.ShardStats().Critical)
	assert.Zero(t, first.blocks.ShardStats().Degraded)
}
