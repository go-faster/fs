package fragment_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

// topo builds racks×nodesPerRack×disksPerNode with equal weights.
func topo(racks, nodesPerRack, disksPerNode int) *cluster.Topology {
	t := &cluster.Topology{Epoch: 1}

	for r := range racks {
		for n := range nodesPerRack {
			node := cluster.Node{
				ID:   cluster.NodeID(fmt.Sprintf("r%d-n%d", r, n)),
				Rack: fmt.Sprintf("rack%d", r),
			}
			for d := range disksPerNode {
				node.Disks = append(node.Disks, cluster.Disk{ID: cluster.DiskID(fmt.Sprintf("d%d", d)), Weight: 1})
			}

			t.Nodes = append(t.Nodes, node)
		}
	}

	return t
}

func randBytes(n int) []byte {
	r := rand.New(rand.NewSource(int64(n)*2654435761 + 7)) //nolint:gosec // deterministic test data
	b := make([]byte, n)
	_, _ = r.Read(b)

	return b
}

// drop returns the fragments with the given indices removed (simulating loss).
func drop(frags []fragment.Fragment, lost ...int) []fragment.Fragment {
	skip := make(map[int]struct{}, len(lost))
	for _, i := range lost {
		skip[i] = struct{}{}
	}

	var out []fragment.Fragment

	for i, f := range frags {
		if _, ok := skip[i]; ok {
			continue
		}

		out = append(out, f)
	}

	return out
}

func TestEncodeShapeRF3(t *testing.T) {
	top := topo(3, 2, 2)

	frags, err := fragment.Encode(top, scheme.Scheme{Kind: scheme.RF3}, "obj", randBytes(500))
	require.NoError(t, err)
	require.Len(t, frags, 3)

	targets := map[string]struct{}{}

	for _, f := range frags {
		assert.Equal(t, fragment.Replica, f.Kind)
		assert.Len(t, f.Data, 500, "replicas hold the full object")
		targets[string(f.Target.Node)+"/"+string(f.Target.Disk)] = struct{}{}
	}

	assert.Len(t, targets, 3, "distinct targets")
}

func TestEncodeShapeRF25(t *testing.T) {
	top := topo(3, 2, 2)
	data := randBytes(501)

	frags, err := fragment.Encode(top, scheme.Scheme{Kind: scheme.RF25}, "obj", data)
	require.NoError(t, err)
	require.Len(t, frags, 3)

	assert.Equal(t, fragment.Replica, frags[0].Kind)
	assert.Equal(t, fragment.Replica, frags[1].Kind)
	assert.Equal(t, fragment.Parity, frags[2].Kind)
	assert.Len(t, frags[0].Data, 501)
	assert.Len(t, frags[2].Data, (501+1)/2, "parity is half length")
}

func TestEncodeShapeEC(t *testing.T) {
	top := topo(3, 3, 2) // 18 disks, enough for RS(4,2)

	frags, err := fragment.Encode(top, scheme.Scheme{Kind: scheme.EC, K: 4, M: 2}, "obj", randBytes(1000))
	require.NoError(t, err)
	require.Len(t, frags, 6)

	for i, f := range frags {
		assert.Equal(t, fragment.Shard, f.Kind)
		assert.Equal(t, i, f.Index)
		assert.Len(t, f.Data, 250, "shards are ceil(1000/4) each")
	}
}

func TestEncodeInsufficientTargets(t *testing.T) {
	small := topo(1, 1, 4) // 4 disks

	_, err := fragment.Encode(small, scheme.Scheme{Kind: scheme.EC, K: 4, M: 2}, "obj", randBytes(100))
	require.ErrorIs(t, err, fragment.ErrInsufficientTargets)
}

func TestRoundTripAllPresent(t *testing.T) {
	top := topo(3, 3, 2)

	schemes := []scheme.Scheme{
		{Kind: scheme.RF25},
		{Kind: scheme.RF3},
		{Kind: scheme.EC, K: 4, M: 2},
		{Kind: scheme.EC, K: 2, M: 1},
	}

	for _, s := range schemes {
		for _, n := range []int{1, 2, 999, 4096} {
			data := randBytes(n)

			frags, err := fragment.Encode(top, s, "k", data)
			require.NoError(t, err, "%s n=%d", s, n)

			got, err := fragment.Decode(s, n, frags)
			require.NoError(t, err, "%s n=%d", s, n)
			assert.True(t, bytes.Equal(data, got), "%s n=%d round-trip", s, n)
		}
	}
}

func TestDecodeRF3Tolerance(t *testing.T) {
	top := topo(3, 2, 2)
	data := randBytes(400)

	frags, err := fragment.Encode(top, scheme.Scheme{Kind: scheme.RF3}, "k", data)
	require.NoError(t, err)

	// Any two of the three replicas may be lost.
	for _, lost := range [][]int{{0, 1}, {0, 2}, {1, 2}} {
		got, err := fragment.Decode(scheme.Scheme{Kind: scheme.RF3}, len(data), drop(frags, lost...))
		require.NoError(t, err, "lost=%v", lost)
		assert.True(t, bytes.Equal(data, got))
	}

	// All three lost is unrecoverable.
	_, err = fragment.Decode(scheme.Scheme{Kind: scheme.RF3}, len(data), drop(frags, 0, 1, 2))
	require.ErrorIs(t, err, fragment.ErrUnrecoverable)
}

func TestDecodeRF25Tolerance(t *testing.T) {
	s := scheme.Scheme{Kind: scheme.RF25}
	top := topo(3, 2, 2)
	data := randBytes(400)

	frags, err := fragment.Encode(top, s, "k", data)
	require.NoError(t, err)

	// Any single loss survives (a full replica always remains).
	for _, lost := range []int{0, 1, 2} {
		got, err := fragment.Decode(s, len(data), drop(frags, lost))
		require.NoError(t, err, "lost=%d", lost)
		assert.True(t, bytes.Equal(data, got))
	}

	// Losing both full replicas (0 and 1) leaves only parity — unrecoverable on
	// the read path (half-repair is the repair worker's job).
	_, err = fragment.Decode(s, len(data), drop(frags, 0, 1))
	require.ErrorIs(t, err, fragment.ErrUnrecoverable)
}

func TestDecodeECTolerance(t *testing.T) {
	s := scheme.Scheme{Kind: scheme.EC, K: 4, M: 2}
	top := topo(3, 3, 2)
	data := randBytes(5000)

	frags, err := fragment.Encode(top, s, "k", data)
	require.NoError(t, err)

	total := s.K + s.M

	// Any m=2 lost shards reconstruct.
	for _, lost := range combinations(total, s.M) {
		got, err := fragment.Decode(s, len(data), drop(frags, lost...))
		require.NoError(t, err, "lost=%v", lost)
		assert.True(t, bytes.Equal(data, got), "lost=%v", lost)
	}

	// Any m+1=3 lost is unrecoverable.
	for _, lost := range combinations(total, s.M+1) {
		_, err := fragment.Decode(s, len(data), drop(frags, lost...))
		require.ErrorIs(t, err, fragment.ErrUnrecoverable, "lost=%v", lost)
	}
}

func TestRoundTripEmpty(t *testing.T) {
	top := topo(3, 3, 2)

	for _, s := range []scheme.Scheme{{Kind: scheme.RF25}, {Kind: scheme.RF3}, {Kind: scheme.EC, K: 4, M: 2}} {
		frags, err := fragment.Encode(top, s, "empty", nil)
		require.NoError(t, err, "%s", s)

		got, err := fragment.Decode(s, 0, frags)
		require.NoError(t, err, "%s", s)
		assert.Empty(t, got, "%s empty round-trip", s)
	}
}

// combinations returns every set of r distinct indices in [0,n).
func combinations(n, r int) [][]int {
	var out [][]int

	idx := make([]int, r)

	var rec func(start, depth int)

	rec = func(start, depth int) {
		if depth == r {
			out = append(out, append([]int(nil), idx...))
			return
		}

		for i := start; i < n; i++ {
			idx[depth] = i
			rec(i+1, depth+1)
		}
	}

	rec(0, 0)

	return out
}
