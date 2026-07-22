package scheme_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/scheme"
)

func TestCodecEncodeShape(t *testing.T) {
	c, err := scheme.NewCodec(4, 2)
	require.NoError(t, err)
	assert.Equal(t, 4, c.DataShards())
	assert.Equal(t, 2, c.ParityShards())
	assert.Equal(t, 6, c.TotalShards())

	shards, err := c.Encode(randBytes(1000))
	require.NoError(t, err)
	require.Len(t, shards, 6)

	// All shards are equal length (ceil(1000/4) = 250).
	for _, s := range shards {
		assert.Len(t, s, 250)
	}

	ok, err := c.Verify(shards)
	require.NoError(t, err)
	assert.True(t, ok, "freshly-encoded parity must verify")
}

func TestCodecInvalid(t *testing.T) {
	_, err := scheme.NewCodec(0, 2)
	assert.Error(t, err)

	_, err = scheme.NewCodec(4, 0)
	assert.Error(t, err)

	c, err := scheme.NewCodec(4, 2)
	require.NoError(t, err)

	_, err = c.Encode(nil)
	assert.Error(t, err, "empty object cannot be erasure-coded")
}

// TestCodecReconstructAnyMLoss is the durability guarantee: an RS(k,m) object
// survives the loss of any m of its k+m shards.
func TestCodecReconstructAnyMLoss(t *testing.T) {
	for _, p := range []struct{ k, m int }{{4, 2}, {2, 1}, {6, 3}, {10, 4}} {
		c, err := scheme.NewCodec(p.k, p.m)
		require.NoError(t, err)

		total := p.k + p.m

		for _, n := range []int{1, 2, 17, 1000, 65537} {
			data := randBytes(n)

			shards, err := c.Encode(data)
			require.NoError(t, err)

			// Drop every distinct combination of exactly m shards and rebuild.
			for _, lost := range combinations(total, p.m) {
				damaged := cloneShards(shards)
				for _, idx := range lost {
					damaged[idx] = nil
				}

				got, err := c.Reconstruct(damaged, n)
				require.NoError(t, err, "k=%d m=%d n=%d lost=%v", p.k, p.m, n, lost)
				assert.True(t, bytes.Equal(data, got),
					"reconstruct must be exact: k=%d m=%d n=%d lost=%v", p.k, p.m, n, lost)
			}
		}
	}
}

// TestCodecReconstructTooManyLost: losing more than m shards is unrecoverable.
func TestCodecReconstructTooManyLost(t *testing.T) {
	c, err := scheme.NewCodec(4, 2)
	require.NoError(t, err)

	data := randBytes(1000)
	shards, err := c.Encode(data)
	require.NoError(t, err)

	// Lose 3 (> m=2) shards.
	shards[0], shards[1], shards[2] = nil, nil, nil

	_, err = c.Reconstruct(shards, len(data))
	assert.Error(t, err, "losing more than m shards must fail")
}

func TestCodecReconstructWrongShardCount(t *testing.T) {
	c, err := scheme.NewCodec(4, 2)
	require.NoError(t, err)

	_, err = c.Reconstruct(make([][]byte, 5), 100)
	assert.Error(t, err)
}

func TestSchemeCodec(t *testing.T) {
	rf25, err := (scheme.Scheme{Kind: scheme.RF25}).Codec()
	require.NoError(t, err)
	assert.Equal(t, 2, rf25.DataShards())
	assert.Equal(t, 1, rf25.ParityShards())

	ec, err := (scheme.Scheme{Kind: scheme.EC, K: 8, M: 3}).Codec()
	require.NoError(t, err)
	assert.Equal(t, 11, ec.TotalShards())

	_, err = (scheme.Scheme{Kind: scheme.RF3}).Codec()
	assert.Error(t, err, "RF=3 has no erasure codec")
}

// TestRF25Parity checks the RF=2.5 half-parity: it is half the object size and
// repairs a damaged replica half from the surviving half + parity.
func TestRF25Parity(t *testing.T) {
	for _, n := range []int{1, 2, 5, 8, 9, 100, 4097} {
		data := randBytes(n)

		parity, err := scheme.HalfParity(data)
		require.NoError(t, err)
		require.Len(t, parity, (n+1)/2, "parity is half length, n=%d", n)

		// A replica lost its first half (index 0); the second half (index 1)
		// survived. Rebuild the whole object from the surviving half + parity.
		c, err := scheme.NewCodec(2, 1)
		require.NoError(t, err)

		shards, err := c.Encode(data)
		require.NoError(t, err)

		survivingSecond := shards[1]

		obj, err := scheme.RepairHalf(survivingSecond, 1, parity, n)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(data, obj), "repair from 2nd half + parity, n=%d", n)

		// Symmetric: lose the second half, survive the first.
		survivingFirst := shards[0]

		obj2, err := scheme.RepairHalf(survivingFirst, 0, parity, n)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(data, obj2), "repair from 1st half + parity, n=%d", n)
	}
}

func TestHalfParityEmpty(t *testing.T) {
	p, err := scheme.HalfParity(nil)
	require.NoError(t, err)
	assert.Nil(t, p)
}

func TestRepairHalfErrors(t *testing.T) {
	_, err := scheme.RepairHalf([]byte{1, 2}, 2, []byte{1, 2}, 4)
	assert.Error(t, err, "index must be 0 or 1")

	_, err = scheme.RepairHalf([]byte{1, 2, 3}, 0, []byte{1, 2}, 4)
	assert.Error(t, err, "half/parity length mismatch")
}

// cloneShards deep-copies a shard set so a reconstruction can't mutate the
// pristine original.
func cloneShards(in [][]byte) [][]byte {
	out := make([][]byte, len(in))
	for i, s := range in {
		if s == nil {
			continue
		}

		out[i] = append([]byte(nil), s...)
	}

	return out
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
