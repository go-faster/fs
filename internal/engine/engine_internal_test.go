package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/block"
)

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}

	return b
}

func put(t *testing.T, e *Engine, key string, data []byte) {
	t.Helper()

	_, err := e.PutObject(context.Background(), &fs.PutObjectRequest{
		Bucket: "b", Key: key, Reader: bytes.NewReader(data), Size: int64(len(data)),
	})
	require.NoError(t, err)
}

// blocksOf returns the blocks the current version of key references.
func blocksOf(t *testing.T, e *Engine, key string) []block.Hash {
	t.Helper()

	_, p, err := e.current(context.Background(), "b", key)
	require.NoError(t, err)

	out := make([]block.Hash, len(p.Blocks))
	for i, b := range p.Blocks {
		out[i] = b.Hash
	}

	return out
}

func live(t *testing.T, e *Engine, hashes []block.Hash) []bool {
	t.Helper()

	out := make([]bool, len(hashes))
	for i, h := range hashes {
		ok, err := e.BlockLive(context.Background(), h)
		require.NoError(t, err)

		out[i] = ok
	}

	return out
}

func allTrue(n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = true
	}

	return out
}

func newEngine(t *testing.T, n int) *Engine {
	t.Helper()

	e := cluster(t, n, small)[0].engine
	require.NoError(t, e.CreateBucket(context.Background(), "b"))

	return e
}

func TestInlineAndBlocks(t *testing.T) {
	e := newEngine(t, 1)

	put(t, e, "tiny", []byte("hi"))
	put(t, e, "big", pattern(1000))

	_, tiny, err := e.current(context.Background(), "b", "tiny")
	require.NoError(t, err)
	assert.Equal(t, []byte("hi"), tiny.Inline)
	assert.Empty(t, tiny.Blocks)

	_, big, err := e.current(context.Background(), "b", "big")
	require.NoError(t, err)
	assert.Nil(t, big.Inline)
	assert.Len(t, big.Blocks, 16, "1000 bytes in 64-byte blocks")
}

func TestRangeRead(t *testing.T) {
	e := newEngine(t, 3)
	data := pattern(1000)
	put(t, e, "k", data)

	resp, err := e.GetObject(context.Background(), "b", "k")
	require.NoError(t, err)

	defer func() { _ = resp.Reader.Close() }()

	rs, ok := resp.Reader.(io.ReadSeeker)
	require.True(t, ok, "ranges are served by seeking")

	for _, c := range []struct{ off, n int }{{0, 10}, {60, 10}, {130, 300}, {990, 10}, {500, 1}} {
		_, err := rs.Seek(int64(c.off), io.SeekStart)
		require.NoError(t, err)

		got := make([]byte, c.n)
		_, err = io.ReadFull(rs, got)
		require.NoError(t, err)
		assert.Equal(t, data[c.off:c.off+c.n], got, "offset %d", c.off)
	}

	_, err = rs.Seek(0, io.SeekEnd)
	require.NoError(t, err)

	n, err := rs.Read(make([]byte, 1))
	assert.Zero(t, n)
	assert.ErrorIs(t, err, io.EOF)
}

func TestBlockReferences(t *testing.T) {
	e := newEngine(t, 1)
	ctx := context.Background()

	put(t, e, "k", pattern(300))
	first := blocksOf(t, e, "k")
	require.Equal(t, allTrue(len(first)), live(t, e, first))

	// Overwriting releases what only the old version used.
	put(t, e, "k", bytes.Repeat([]byte{1}, 300))
	assert.NotContains(t, live(t, e, first), true, "the replaced version's blocks are collectable")

	second := blocksOf(t, e, "k")
	assert.Equal(t, allTrue(len(second)), live(t, e, second))

	// Deleting releases the rest.
	require.NoError(t, e.DeleteObject(ctx, "b", "k"))
	assert.NotContains(t, live(t, e, second), true)
}

func TestMultipartReferences(t *testing.T) {
	e := newEngine(t, 1)
	ctx := context.Background()

	upload, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "mp"})
	require.NoError(t, err)

	partData := func(seed byte) []byte { return bytes.Repeat([]byte{seed}, 200) }

	uploadPart := func(n int, data []byte) *fs.Part {
		p, err := e.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket: "b", Key: "mp", UploadID: upload.UploadID, PartNumber: n,
			Reader: bytes.NewReader(data), Size: int64(len(data)),
		})
		require.NoError(t, err)

		return p
	}

	// Part 1 is uploaded twice; the first attempt's blocks are released.
	uploadPart(1, partData(9))
	replaced := block.Sum(partData(9)[:64])

	p1 := uploadPart(1, partData(1))
	p2 := uploadPart(2, partData(2))

	assert.Equal(t, []bool{false}, live(t, e, []block.Hash{replaced}))

	_, err = e.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "b", Key: "mp", UploadID: upload.UploadID,
		Parts: []fs.CompletedPart{{PartNumber: 1, ETag: p1.ETag}, {PartNumber: 2, ETag: p2.ETag}},
	})
	require.NoError(t, err)

	got := blocksOf(t, e, "mp")
	assert.Equal(t, allTrue(len(got)), live(t, e, got), "the object holds its blocks after the parts are released")

	resp, err := e.GetObject(ctx, "b", "mp")
	require.NoError(t, err)

	body, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)
	assert.Equal(t, append(partData(1), partData(2)...), body)

	// An aborted upload releases its parts.
	aborted, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "ab"})
	require.NoError(t, err)

	p, err := e.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket: "b", Key: "ab", UploadID: aborted.UploadID, PartNumber: 1,
		Reader: bytes.NewReader(partData(7)), Size: 200,
	})
	require.NoError(t, err)
	require.NotEmpty(t, p.ETag)

	require.NoError(t, e.AbortMultipartUpload(ctx, "b", "ab", aborted.UploadID))
	assert.Equal(t, []bool{false}, live(t, e, []block.Hash{block.Sum(partData(7)[:64])}))
}

func TestUploadSurvivesPut(t *testing.T) {
	// S3 keeps an upload valid however many PUTs complete meanwhile.
	e := newEngine(t, 1)
	ctx := context.Background()

	upload, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "k"})
	require.NoError(t, err)

	put(t, e, "k", []byte("a put in between"))

	_, err = e.ListParts(ctx, "b", "k", upload.UploadID)
	require.NoError(t, err, "the upload is still there")
}

func TestOneNodeDown(t *testing.T) {
	nodes := cluster(t, 3, small)
	ctx := context.Background()
	e := nodes[0].engine

	require.NoError(t, e.CreateBucket(ctx, "b"))

	nodes[2].srv.Close()

	data := pattern(700)
	put(t, e, "k", data)

	resp, err := e.GetObject(ctx, "b", "k")
	require.NoError(t, err)

	body, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)
	assert.Equal(t, data, body)

	list, err := e.ListObjects(ctx, &fs.ListObjectsRequest{Bucket: "b"})
	require.NoError(t, err)
	require.Len(t, list.Objects, 1)

	require.NoError(t, e.DeleteObject(ctx, "b", "k"))
}

func TestListObjectsAcrossPages(t *testing.T) {
	// More keys than one table page, folded and paged, must match FoldPage.
	e := newEngine(t, 1)
	ctx := context.Background()

	var all []fs.Object

	for i := range 1200 {
		key := fmt.Sprintf("%s%c/%04d", []string{"a/", "b/", "c"}[i%3], 'a'+i%26, i)
		put(t, e, key, []byte("x"))
		all = append(all, fs.Object{Key: key})
	}

	for _, req := range []fs.ListObjectsRequest{
		{Bucket: "b", Limit: 700},
		{Bucket: "b", Delimiter: "/", Limit: 2},
		{Bucket: "b", Prefix: "a/", Delimiter: "/", Limit: 5},
		{Bucket: "b", Prefix: "c", Limit: 100},
	} {
		var got, want []string

		ref := req
		ref.StartAfter = ""

		for {
			page, err := e.ListObjects(ctx, &req)
			require.NoError(t, err)

			wantPage := ref.FoldPage(filter(all, ref.Prefix))

			got = append(got, keys(page)...)
			want = append(want, keys(wantPage)...)

			assert.Equal(t, wantPage.IsTruncated, page.IsTruncated, "%+v", req)
			assert.Equal(t, wantPage.NextStartAfter, page.NextStartAfter, "%+v", req)

			if !page.IsTruncated {
				break
			}

			req.StartAfter, ref.StartAfter = page.NextStartAfter, wantPage.NextStartAfter
		}

		assert.Equal(t, want, got, "%+v", req)
	}
}

func filter(objects []fs.Object, prefix string) []fs.Object {
	var out []fs.Object

	for _, o := range objects {
		if len(o.Key) >= len(prefix) && o.Key[:len(prefix)] == prefix {
			out = append(out, o)
		}
	}

	return out
}

func keys(r *fs.ListObjectsResponse) []string {
	var out []string
	for _, o := range r.Objects {
		out = append(out, o.Key)
	}

	return append(out, r.CommonPrefixes...)
}

func TestVersionedBlockReferences(t *testing.T) {
	e := newEngine(t, 1)
	ctx := context.Background()

	require.NoError(t, e.SetBucketVersioning(ctx, "b", fs.VersioningEnabled))

	put(t, e, "k", pattern(300))
	first := blocksOf(t, e, "k")

	versions, err := e.ListObjectVersions(ctx, &fs.ListObjectVersionsRequest{Bucket: "b"})
	require.NoError(t, err)
	require.Len(t, versions.Versions, 1)

	firstID := versions.Versions[0].VersionID

	// An overwrite keeps the old version, and so its blocks.
	put(t, e, "k", bytes.Repeat([]byte{3}, 300))
	assert.Equal(t, allTrue(len(first)), live(t, e, first))

	// So does a delete marker: nothing is removed.
	_, err = e.DeleteObjectVersion(ctx, "b", "k", "")
	require.NoError(t, err)
	assert.Equal(t, allTrue(len(first)), live(t, e, first))

	// Only deleting the version itself releases them.
	_, err = e.DeleteObjectVersion(ctx, "b", "k", firstID)
	require.NoError(t, err)
	assert.NotContains(t, live(t, e, first), true)
}

func TestVersionedMultipart(t *testing.T) {
	e := newEngine(t, 1)
	ctx := context.Background()

	require.NoError(t, e.SetBucketVersioning(ctx, "b", fs.VersioningEnabled))
	put(t, e, "mp", []byte("before"))

	upload, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "mp"})
	require.NoError(t, err)

	p, err := e.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket: "b", Key: "mp", UploadID: upload.UploadID, PartNumber: 1,
		Reader: bytes.NewReader([]byte("assembled")), Size: 9,
	})
	require.NoError(t, err)

	_, err = e.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "b", Key: "mp", UploadID: upload.UploadID,
		Parts: []fs.CompletedPart{{PartNumber: 1, ETag: p.ETag}},
	})
	require.NoError(t, err)

	versions, err := e.ListObjectVersions(ctx, &fs.ListObjectVersionsRequest{Bucket: "b"})
	require.NoError(t, err)
	require.Len(t, versions.Versions, 2, "the completed upload is a version; the PUT before it is kept")
	assert.Equal(t, upload.UploadID, versions.Versions[0].VersionID)
	assert.True(t, versions.Versions[0].IsLatest)
}
