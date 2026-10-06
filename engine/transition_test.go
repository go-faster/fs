package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/layout"
)

// TestLayoutTransition: a node leaves the layout; the old version stays
// retained until every node has swept the new one, then retires, the node
// that left is emptied, and every object still reads.
func TestLayoutTransition(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t, 4, small)
	e := nodes[0].engine

	require.NoError(t, e.CreateBucket(ctx, "b"))

	objects := map[string][]byte{}

	for i := range 20 {
		k := fmt.Sprint("k", i)
		objects[k] = bytes.Repeat([]byte{byte(i)}, 300)

		_, err := e.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: k, Reader: bytes.NewReader(objects[k]), Size: 300})
		require.NoError(t, err)
	}

	// n3 leaves.
	var roles []layout.Node
	for _, n := range nodes[:3] {
		roles = append(roles, layout.Node{ID: n.member.ID(), Zone: "z-" + string(n.member.ID()), Capacity: 1})
	}

	next, err := layout.Compute(nodes[0].member.Layout(), roles, layout.Options{})
	require.NoError(t, err)

	for _, n := range nodes {
		_, err := n.member.Adopt(next)
		require.NoError(t, err)
	}

	require.Len(t, nodes[0].member.Layouts(), 2, "the old version is retained until synced")

	// Each node sweeps; once all have synced the new version, gossip retires
	// the old one everywhere.
	require.Eventually(t, func() bool {
		for _, n := range nodes {
			_ = n.engine.Sweep(ctx)
		}

		for _, n := range nodes {
			n.member.Round(ctx)
		}

		for _, n := range nodes {
			if len(n.member.Layouts()) != 1 {
				return false
			}
		}

		return true
	}, 10*time.Second, 10*time.Millisecond)

	// The next sweep empties the node that left.
	_ = nodes[3].engine.Sweep(ctx)

	left := nodes[3].engine.Stats()
	// Block references spread over every partition by hash, so the node
	// that left held some; one bucket's objects are one partition, which it
	// may not have held.
	assert.Positive(t, left.TableSync["block_refs"].HandedOver, "the node that left handed its rows over")
	assert.Positive(t, left.BlockSync.HandedOver, "and its blocks")

	for k, want := range objects {
		resp, err := nodes[1].engine.GetObject(ctx, "b", k)
		require.NoError(t, err, k)

		got, err := io.ReadAll(resp.Reader)
		_ = resp.Reader.Close()

		require.NoError(t, err)
		assert.Equal(t, want, got, k)
	}
}
