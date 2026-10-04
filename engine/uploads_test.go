package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// TestUploadIndex: the listing of uploads in flight comes from the index,
// follows uploads through completion and abort, and handles a key with NUL.
func TestUploadIndex(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, 3)

	start := func(key string) string {
		up, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: key})
		require.NoError(t, err)

		return up.UploadID
	}

	listed := func() []string {
		ups, err := e.ListMultipartUploads(ctx, "b")
		require.NoError(t, err)

		var out []string
		for _, u := range ups {
			out = append(out, u.Key+"|"+u.UploadID)
		}

		return out
	}

	nul := "a\x00b"
	a, b, c := start("k"), start(nul), start("z")

	assert.ElementsMatch(t, []string{"k|" + a, nul + "|" + b, "z|" + c}, listed(), "a key with NUL splits at the last one")

	p, err := e.UploadPart(ctx, &fs.UploadPartRequest{Bucket: "b", Key: "k", UploadID: a, PartNumber: 1, Reader: bytes.NewReader([]byte("x")), Size: 1})
	require.NoError(t, err)

	_, err = e.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "b", Key: "k", UploadID: a, Parts: []fs.CompletedPart{{PartNumber: 1, ETag: p.ETag}},
	})
	require.NoError(t, err)
	require.NoError(t, e.AbortMultipartUpload(ctx, "b", "z", c))

	assert.Equal(t, []string{nul + "|" + b}, listed(), "completed and aborted uploads leave the listing")

	// Their index rows are tombstones, collected like any other.
	require.NoError(t, e.uploads.Collect(ctx, 0))

	_, inc, err := e.bucket(ctx, "b")
	require.NoError(t, err)

	rows, err := e.uploads.Range(ctx, inc.ID, "", 10)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "only the upload in flight is left in the index")
}
