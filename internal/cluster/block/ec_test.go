package block

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/layout"
)

func TestParseScheme(t *testing.T) {
	for in, want := range map[string]Scheme{"rf3": {}, "": {}, "ec:4,2": {4, 2}, "ec:2,1": {2, 1}} {
		got, err := ParseScheme(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got)
		assert.Equal(t, cmpString(in), got.String())
	}

	for _, in := range []string{"ec:1,1", "ec:4,0", "ec:12,5", "ec:4", "rs:4,2", "ec:a,b"} {
		_, err := ParseScheme(in)
		assert.Error(t, err, in)
	}
}

func cmpString(in string) string {
	if in == "" {
		return "rf3"
	}

	return in
}

func TestParseShard(t *testing.T) {
	sh := Shard{Hash: Sum([]byte("x")), K: 4, M: 2, I: 5}

	got, err := ParseShard(sh.String())
	require.NoError(t, err)
	assert.Equal(t, sh, got)

	for _, bad := range []string{sh.Hash.String(), sh.Hash.String() + ".4-2-6", sh.Hash.String() + ".4-2-05", "zz.4-2-1"} {
		_, err := ParseShard(bad)
		assert.Error(t, err, bad)
	}
}

// TestCodecAnyK is the property erasure coding rests on: any K of the K+M
// shards rebuild the block.
func TestCodecAnyK(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))

	for _, s := range []Scheme{{2, 1}, {4, 2}, {6, 3}} {
		enc, err := codec(s)
		require.NoError(t, err)

		for range 50 {
			data := make([]byte, 1+r.IntN(5000))
			for i := range data {
				data[i] = byte(r.IntN(256))
			}

			shards, err := enc.Split(bytes.Clone(data))
			require.NoError(t, err)
			require.NoError(t, enc.Encode(shards))

			// Drop M shards chosen at random.
			lost := make([][]byte, len(shards))
			copy(lost, shards)

			for _, i := range r.Perm(len(lost))[:s.M] {
				lost[i] = nil
			}

			require.NoError(t, enc.ReconstructData(lost))

			var got []byte
			for _, sh := range lost[:s.K] {
				got = append(got, sh...)
			}

			require.Equal(t, data, got[:len(data)], "%s", s)
		}
	}
}

func TestCodedPutGet(t *testing.T) {
	ctx := context.Background()
	nodes := clusterOf(t, 6, 6, 3, 6)
	s := Scheme{K: 4, M: 2}
	data := bytes.Repeat([]byte("erasure coded block "), 30000)
	h := Sum(data)

	require.NoError(t, nodes[0].blocks.PutCoded(ctx, h, data, s, nil))

	slots, err := nodes[0].blocks.slotsFor(h, s)
	require.NoError(t, err)

	// One shard per node, shard i on slot i, each about a quarter of the block.
	require.Eventually(t, func() bool {
		for i, id := range slots {
			if !byID(nodes, id).store.HasShard(Shard{Hash: h, K: 4, M: 2, I: i}) {
				return false
			}
		}

		return true
	}, 5*time.Second, time.Millisecond)

	got, err := nodes[3].blocks.GetCoded(ctx, h, len(data), s)
	require.NoError(t, err)
	assert.Equal(t, data, got)
	assert.Zero(t, nodes[3].blocks.Stats().Degraded, "a healthy read decodes nothing")

	for _, n := range nodes {
		assert.False(t, n.store.Has(h), "no whole copy anywhere")
	}
}

// TestCodedSurvivesM: with M nodes down, reads still rebuild the block, and
// with one down, writes still reach K+1.
func TestCodedSurvivesM(t *testing.T) {
	ctx := context.Background()
	nodes := clusterOf(t, 6, 6, 3, 6)
	s := Scheme{K: 4, M: 2}
	data := bytes.Repeat([]byte("survive "), 100000)
	h := Sum(data)

	require.NoError(t, nodes[0].blocks.PutCoded(ctx, h, data, s, nil))

	slots, err := nodes[0].blocks.slotsFor(h, s)
	require.NoError(t, err)

	// Wait for every shard, then take down two data shards' nodes.
	require.Eventually(t, func() bool {
		for i, id := range slots {
			if !byID(nodes, id).store.HasShard(Shard{Hash: h, K: 4, M: 2, I: i}) {
				return false
			}
		}

		return true
	}, 5*time.Second, time.Millisecond)

	down := []layout.NodeID{slots[0], slots[2]}
	for _, id := range down {
		byID(nodes, id).srv.Close()
	}

	reader := byID(nodes, slots[5])

	got, err := reader.blocks.GetCoded(ctx, h, len(data), s)
	require.NoError(t, err)
	assert.Equal(t, data, got)
	assert.Equal(t, int64(1), reader.blocks.Stats().Degraded)

	// A third node down leaves three of six: unreadable, and said so.
	byID(nodes, slots[1]).srv.Close()

	_, err = reader.blocks.GetCoded(ctx, h, len(data), s)
	require.ErrorIs(t, err, ErrNotFound)

	// Writes: one node down of six still reaches K+1 = 5; two down do not.
	fresh := clusterOf(t, 6, 6, 3, 6)
	other := []byte("another block, written with a node down")
	oh := Sum(other)

	oslots, err := fresh[0].blocks.slotsFor(oh, s)
	require.NoError(t, err)

	coord := byID(fresh, oslots[0])
	byID(fresh, oslots[5]).srv.Close()
	require.NoError(t, coord.blocks.PutCoded(ctx, oh, other, s, nil))

	byID(fresh, oslots[4]).srv.Close()
	require.ErrorIs(t, coord.blocks.PutCoded(ctx, Sum([]byte("x")), []byte("x"), s, nil), ErrQuorum)
}

func TestCodedNeedsWideLayout(t *testing.T) {
	nodes := cluster(t, 3)

	err := nodes[0].blocks.PutCoded(context.Background(), Sum([]byte("x")), []byte("x"), Scheme{K: 4, M: 2}, nil)
	require.ErrorContains(t, err, "needs 6 nodes per partition")
}

func TestCodedGC(t *testing.T) {
	ctx := context.Background()
	nodes := clusterOf(t, 6, 6, 3, 6)
	s := Scheme{K: 2, M: 1}
	data := []byte("collect my shards")
	h := Sum(data)

	require.NoError(t, nodes[0].blocks.PutCoded(ctx, h, data, s, nil))

	slots, err := nodes[0].blocks.slotsFor(h, s)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return byID(nodes, slots[2]).store.HasShard(Shard{Hash: h, K: 2, M: 1, I: 2})
	}, 5*time.Second, time.Millisecond)

	dead := func(context.Context, Hash) (bool, error) { return false, nil }

	for i, id := range slots {
		n, err := byID(nodes, id).blocks.GC(ctx, 0, dead)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.False(t, byID(nodes, id).store.HasShard(Shard{Hash: h, K: 2, M: 1, I: i}))
	}
}

// TestCodedPlacement: the slots a coded block uses are distinct nodes, and a
// layout spread for K+M puts at most M of them in one zone, so losing a zone
// leaves K shards.
func TestCodedPlacement(t *testing.T) {
	var nodes []layout.Node
	for i := range 6 {
		nodes = append(nodes, layout.Node{ID: layout.NodeID(fmt.Sprint("n", i)), Zone: fmt.Sprint("z", i%3), Capacity: 1})
	}

	l, err := layout.Compute(nil, nodes, layout.Options{Partitions: 64, Widths: []int{3, 6}})
	require.NoError(t, err)

	zone := map[layout.NodeID]string{}
	for _, n := range nodes {
		zone[n.ID] = n.Zone
	}

	for p, slots := range l.Slots {
		perZone := map[string]int{}
		seen := map[layout.NodeID]bool{}

		for _, id := range slots[:6] {
			require.False(t, seen[id], "partition %d: node %s holds two shards", p, id)
			seen[id] = true
			perZone[zone[id]]++
		}

		for z, n := range perZone {
			assert.LessOrEqual(t, n, 2, "partition %d: zone %s holds %d shards of ec:4,2", p, z, n)
		}
	}
}

// TestCodedReadFallsBackToMoreParity: one data shard is missing, so a read
// fetches one parity shard; that one is missing too, so it fetches the next.
func TestCodedReadFallsBackToMoreParity(t *testing.T) {
	ctx := context.Background()
	nodes := clusterOf(t, 6, 6, 3, 6)
	s := Scheme{K: 4, M: 2}
	data := bytes.Repeat([]byte("fallback "), 30000)
	h, slots := putEverywhere(t, nodes, data, s)

	for _, i := range []int{1, 4} {
		require.NoError(t, byID(nodes, slots[i]).store.DeleteShard(Shard{Hash: h, K: 4, M: 2, I: i}))
	}

	got, err := nodes[0].blocks.GetCoded(ctx, h, len(data), s)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}
