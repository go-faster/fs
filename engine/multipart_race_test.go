package engine

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/meta"
)

// TestCompletionOnSomeReplicas is a completion that lands on one replica of
// three and fails: the key must still read as one of its two versions.
func TestCompletionOnSomeReplicas(t *testing.T) {
	nodes := cluster(t, 3, small)
	ctx := context.Background()
	e := nodes[0].engine

	require.NoError(t, e.CreateBucket(ctx, "b"))

	v1 := pattern(300)
	put(t, e, "k", v1)

	up, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "k"})
	require.NoError(t, err)

	v2 := bytes.Repeat([]byte("y"), 300)
	p, err := e.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket: "b", Key: "k", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader(v2), Size: int64(len(v2)),
	})
	require.NoError(t, err)

	nodes[1].noWrites.Store(true)
	nodes[2].noWrites.Store(true)

	_, err = e.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "b", Key: "k", UploadID: up.UploadID, Parts: []fs.CompletedPart{{PartNumber: 1, ETag: p.ETag}},
	})
	t.Logf("completion: %v", err)

	nodes[1].noWrites.Store(false)
	nodes[2].noWrites.Store(false)

	for i, n := range nodes {
		resp, err := n.engine.GetObject(ctx, "b", "k")
		if err != nil {
			t.Errorf("node %d: %v", i, err)

			continue
		}

		got, err := io.ReadAll(resp.Reader)
		require.NoError(t, err)
		require.NoError(t, resp.Reader.Close())

		if !bytes.Equal(got, v1) && !bytes.Equal(got, v2) {
			t.Errorf("node %d: read neither version (%d bytes)", i, len(got))
		}
	}
}

// TestCanceledCompletion cuts completions off at random points: the key must
// always read as one of its two versions.
func TestCanceledCompletion(t *testing.T) {
	nodes := cluster(t, 3, small)
	ctx := context.Background()
	e := nodes[0].engine

	require.NoError(t, e.CreateBucket(ctx, "b"))

	for i := range 100 {
		v1 := bytes.Repeat([]byte{byte(i)}, 300)
		put(t, e, "k", v1)

		up, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "k"})
		require.NoError(t, err)

		v2 := bytes.Repeat([]byte{byte(i), 1}, 200)
		p, err := e.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket: "b", Key: "k", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader(v2), Size: int64(len(v2)),
		})
		require.NoError(t, err)

		cctx, cancel := context.WithTimeout(ctx, time.Duration(i%40)*100*time.Microsecond)
		_, cerr := e.CompleteMultipartUpload(cctx, &fs.CompleteMultipartUploadRequest{
			Bucket: "b", Key: "k", UploadID: up.UploadID, Parts: []fs.CompletedPart{{PartNumber: 1, ETag: p.ETag}},
		})

		cancel()

		for j, n := range nodes {
			resp, err := n.engine.GetObject(ctx, "b", "k")
			if err != nil {
				t.Fatalf("iteration %d (completion: %v): node %d: %v", i, cerr, j, err)
			}

			got, err := io.ReadAll(resp.Reader)
			require.NoError(t, err)
			require.NoError(t, resp.Reader.Close())

			if !bytes.Equal(got, v1) && !bytes.Equal(got, v2) {
				t.Fatalf("iteration %d (completion: %v): node %d read neither version", i, cerr, j)
			}
		}
	}
}

// TestAbortRacingCompletion is the loss the chaos soak found: a client gave
// up on a completion that went on to succeed, and aborted the upload. The
// abort must not delete the object the completion made, nor the version it
// replaced.
func TestAbortRacingCompletion(t *testing.T) {
	nodes := cluster(t, 3, small)
	ctx := context.Background()
	e0, e1 := nodes[0].engine, nodes[1].engine

	require.NoError(t, e0.CreateBucket(ctx, "b"))
	put(t, e0, "k", pattern(300))

	up, err := e0.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "k"})
	require.NoError(t, err)

	v2 := bytes.Repeat([]byte("y"), 300)
	p, err := e0.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket: "b", Key: "k", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader(v2), Size: int64(len(v2)),
	})
	require.NoError(t, err)

	// An abort through another node reads the upload still in flight...
	bucketID, stale, err := e1.upload(ctx, "b", "k", up.UploadID)
	require.NoError(t, err)

	_, err = e0.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "b", Key: "k", UploadID: up.UploadID, Parts: []fs.CompletedPart{{PartNumber: 1, ETag: p.ETag}},
	})
	require.NoError(t, err)

	// ...and writes its tombstone after the completion landed.
	stale.State = meta.Gone
	require.NoError(t, e1.objects.Insert(ctx, bucketID, "k", meta.Object{Versions: []meta.Version{stale}}))

	for i, n := range nodes {
		resp, err := n.engine.GetObject(ctx, "b", "k")
		require.NoError(t, err, "node %d", i)

		got, err := io.ReadAll(resp.Reader)
		require.NoError(t, err)
		require.NoError(t, resp.Reader.Close())
		require.Equal(t, v2, got, "node %d: the completed object stands", i)
	}

	// Through any node, an abort after the completion is refused.
	for _, n := range nodes {
		require.ErrorIs(t, n.engine.AbortMultipartUpload(ctx, "b", "k", up.UploadID), fs.ErrUploadNotFound)
	}
}
