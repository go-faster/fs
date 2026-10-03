package engine

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// collect runs tombstone collection with no delay.
func collect(t *testing.T, e *Engine) {
	t.Helper()

	ctx := context.Background()

	require.NoError(t, e.objects.Collect(ctx, 0))
	require.NoError(t, e.refs.Collect(ctx, 0))
	require.NoError(t, e.parts.Collect(ctx, 0))
}

func TestTombstonesCollected(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, 3)

	_, inc, err := e.bucket(ctx, "b")
	require.NoError(t, err)

	put(t, e, "deleted", pattern(1000))
	hashes := blocksOf(t, e, "deleted")

	require.NoError(t, e.DeleteObject(ctx, "b", "deleted"))

	// A finished upload's parts, and an aborted one's. Content apart from
	// the deleted object's, which would otherwise share its blocks.
	mpData := bytes.Repeat([]byte("m"), 100)

	var uploads []string

	for _, abort := range []bool{false, true} {
		up, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "mp"})
		require.NoError(t, err)

		p, err := e.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket: "b", Key: "mp", UploadID: up.UploadID, PartNumber: 1,
			Reader: bytes.NewReader(mpData), Size: int64(len(mpData)),
		})
		require.NoError(t, err)

		if abort {
			require.NoError(t, e.AbortMultipartUpload(ctx, "b", "mp", up.UploadID))
		} else {
			_, err = e.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
				Bucket: "b", Key: "mp", UploadID: up.UploadID,
				Parts: []fs.CompletedPart{{PartNumber: 1, ETag: p.ETag}},
			})
			require.NoError(t, err)
		}

		uploads = append(uploads, up.UploadID)
	}

	collect(t, e)

	_, found, err := e.objects.Get(ctx, inc.ID, "deleted")
	require.NoError(t, err)
	assert.False(t, found, "a deleted object's row goes")

	for _, h := range hashes {
		refs, err := e.refs.Range(ctx, h.String(), "", 10)
		require.NoError(t, err)
		assert.Empty(t, refs, "and so do its block references")
	}

	assert.Equal(t, []bool{false, false, false, false, false, false, false, false, false, false, false, false, false, false, false, false},
		live(t, e, hashes), "leaving its blocks to block collection")

	for _, id := range uploads {
		parts, err := e.parts.Range(ctx, id, "", 10)
		require.NoError(t, err)
		assert.Empty(t, parts, "a finished upload's parts go")
	}

	o, found, err := e.objects.Get(ctx, inc.ID, "mp")
	require.NoError(t, err)
	require.True(t, found)
	assert.Len(t, o.Versions, 1, "the aborted upload's version goes, the completed one stays")

	resp, err := e.GetObject(ctx, "b", "mp")
	require.NoError(t, err)

	got, err := io.ReadAll(resp.Reader)
	_ = resp.Reader.Close()

	require.NoError(t, err)
	assert.Equal(t, mpData, got)
}

func TestCollectKeepsVersions(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, 1)

	require.NoError(t, e.SetBucketVersioning(ctx, "b", fs.VersioningEnabled))

	put(t, e, "k", []byte("one"))
	put(t, e, "k", []byte("two"))

	list, err := e.ListObjectVersions(ctx, &fs.ListObjectVersionsRequest{Bucket: "b"})
	require.NoError(t, err)
	require.Len(t, list.Versions, 2)

	kept := list.Versions[1].VersionID

	_, err = e.DeleteObjectVersion(ctx, "b", "k", list.Versions[0].VersionID)
	require.NoError(t, err)

	// A delete marker is a version, not a tombstone.
	require.NoError(t, e.DeleteObject(ctx, "b", "k"))

	collect(t, e)

	after, err := e.ListObjectVersions(ctx, &fs.ListObjectVersionsRequest{Bucket: "b"})
	require.NoError(t, err)
	require.Len(t, after.Versions, 2)
	assert.True(t, after.Versions[0].DeleteMarker)
	assert.Equal(t, kept, after.Versions[1].VersionID)
}

func TestUploadPartAfterComplete(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, 1)

	up, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "k"})
	require.NoError(t, err)

	p, err := e.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket: "b", Key: "k", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader([]byte("x")), Size: 1,
	})
	require.NoError(t, err)

	_, err = e.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "b", Key: "k", UploadID: up.UploadID, Parts: []fs.CompletedPart{{PartNumber: 1, ETag: p.ETag}},
	})
	require.NoError(t, err)

	_, err = e.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket: "b", Key: "k", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader([]byte("y")), Size: 1,
	})
	require.ErrorIs(t, err, fs.ErrUploadNotFound)
}
