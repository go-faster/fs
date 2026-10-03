package engine

import (
	"bytes"
	"context"
	"io"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func TestBucketScheme(t *testing.T) {
	ctx := context.Background()

	// A layout three wide cannot hold ec:4,2 on distinct nodes.
	narrow := newEngine(t, 3)
	require.ErrorIs(t, narrow.SetBucketScheme(ctx, "b", "ec:4,2"), fs.ErrUnsupportedOperation)
	require.ErrorIs(t, narrow.SetBucketScheme(ctx, "b", "ec:9"), fs.ErrUnsupportedOperation)
	require.NoError(t, narrow.SetBucketScheme(ctx, "b", "ec:2,1"))

	got, err := narrow.BucketScheme(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, "ec:2,1", got)
}

// TestCodedBucket is erasure coding end to end: a bucket set to ec:4,2
// stores its large blocks as shards, small ones and inline objects as before,
// and reads them back with shard holders down.
func TestCodedBucket(t *testing.T) {
	ctx := context.Background()
	cfg := Config{BlockSize: 4096, InlineLimit: 64, CodedMinSize: 1024}
	nodes := clusterWide(t, 7, cfg, 3, 6)
	e := nodes[0].engine

	require.NoError(t, e.CreateBucket(ctx, "b"))
	require.NoError(t, e.SetBucketScheme(ctx, "b", "ec:4,2"))

	objects := map[string][]byte{
		"big":    bytes.Repeat([]byte("coded block data "), 1500), // Six blocks, the last short.
		"short":  bytes.Repeat([]byte("s"), 500),                  // One block below the coded minimum.
		"inline": []byte("tiny"),
	}

	for k, v := range objects {
		_, err := e.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: k, Reader: bytes.NewReader(v), Size: int64(len(v))})
		require.NoError(t, err)
	}

	_, p, err := e.current(ctx, "b", "big")
	require.NoError(t, err)

	coded := 0

	for _, b := range p.Blocks {
		if b.Scheme == "ec:4,2" {
			coded++
		}
	}

	assert.Equal(t, len(p.Blocks)-1, coded, "every full block coded, the short tail replicated")

	_, sp, err := e.current(ctx, "b", "short")
	require.NoError(t, err)
	assert.Empty(t, sp.Blocks[0].Scheme)

	// Take down up to two nodes that hold shards but none of the metadata —
	// the bucket row and the object rows, each on three nodes — so a read
	// can only fail on the blocks. Seven nodes leave at least one.
	_, inc, err := e.bucket(ctx, "b")
	require.NoError(t, err)

	l := nodes[0].member.Layout()
	metadata := append(slices.Clone(l.Slots[l.Partition([]byte(inc.ID))][:3]), l.Slots[l.Partition([]byte(bucketsPK))][:3]...)

	var reader *Engine

	down := 0

	for _, n := range nodes {
		switch {
		case slices.Contains(metadata, n.member.ID()):
			reader = n.engine
		case down < 2:
			n.srv.Close()

			down++
		}
	}

	require.Positive(t, down)

	resp, err := reader.GetObject(ctx, "b", "big")
	require.NoError(t, err)

	got, err := io.ReadAll(resp.Reader)
	_ = resp.Reader.Close()

	require.NoError(t, err)
	assert.Equal(t, objects["big"], got)
	assert.Positive(t, reader.blocks.Stats().Degraded, "some block lost a shard holder and was rebuilt from parity")
}
