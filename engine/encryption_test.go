package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/sse"
	"github.com/go-faster/fs/storagetest"
)

func masterKey(t testing.TB) sse.MasterKey {
	t.Helper()

	k := make([]byte, sse.KeySize)
	_, err := rand.Read(k)
	require.NoError(t, err)

	mk, err := sse.NewMasterKey(k)
	require.NoError(t, err)

	return mk
}

func keyring(t testing.TB, current sse.MasterKey, previous ...sse.MasterKey) *sse.Keyring {
	t.Helper()

	kr, err := sse.NewKeyring(current, previous...)
	require.NoError(t, err)

	return kr
}

func TestConformanceEncrypted(t *testing.T) {
	for _, n := range []int{1, 3} {
		storagetest.Run(t, func(t testing.TB) fs.Storage {
			cfg := small
			cfg.Keyring = keyring(t, masterKey(t))

			return cluster(t, n, cfg)[0].engine
		})
	}
}

// sealedEngine has a keyring and blocks big enough that a test of tens of
// kilobytes stays quick.
func sealedEngine(t *testing.T, kr *sse.Keyring) *Engine {
	t.Helper()

	e := cluster(t, 1, Config{BlockSize: 4096, InlineLimit: 256, Keyring: kr})[0].engine
	require.NoError(t, e.CreateBucket(context.Background(), "b"))

	return e
}

func putSealed(t *testing.T, e *Engine, key string, data []byte) {
	t.Helper()

	resp, err := e.PutObject(context.Background(), &fs.PutObjectRequest{
		Bucket: "b", Key: key, Reader: bytes.NewReader(data), Size: int64(len(data)),
		ServerSideEncryption: sse.Algorithm,
	})
	require.NoError(t, err)
	require.Equal(t, sse.Algorithm, resp.ServerSideEncryption)
}

// stored returns every byte the engine stored for key: inline content and
// block content alike.
func stored(t *testing.T, e *Engine, key string) []byte {
	t.Helper()

	_, p, err := e.current(context.Background(), "b", key)
	require.NoError(t, err)

	out := bytes.Clone(p.Inline)

	for _, b := range p.Blocks {
		data, err := e.blocks.Get(context.Background(), b.Hash)
		require.NoError(t, err)

		out = append(out, data...)
	}

	return out
}

func TestNothingPlainAtRest(t *testing.T) {
	e := sealedEngine(t, keyring(t, masterKey(t)))

	small, big := []byte("a secret, inline"), bytes.Repeat([]byte("a secret in blocks;"), 2000)
	putSealed(t, e, "small", small)
	putSealed(t, e, "big", big)

	assert.NotContains(t, string(stored(t, e, "small")), "secret")
	assert.NotContains(t, string(stored(t, e, "big")), "secret")

	for key, want := range map[string][]byte{"small": small, "big": big} {
		resp, err := e.GetObject(context.Background(), "b", key)
		require.NoError(t, err)
		assert.Equal(t, sse.Algorithm, resp.ServerSideEncryption)

		got, err := io.ReadAll(resp.Reader)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
}

func TestEncryptedMultipartRanges(t *testing.T) {
	e := sealedEngine(t, keyring(t, masterKey(t)))
	ctx := context.Background()

	upload, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: "b", Key: "mp", ServerSideEncryption: sse.Algorithm,
	})
	require.NoError(t, err)

	// Parts larger than one cipher chunk, of sizes that align with nothing.
	sizes := []int{70001, 131072 + 5, 999}

	var (
		whole     []byte
		completed []fs.CompletedPart
	)

	for i, n := range sizes {
		data := make([]byte, n)
		_, _ = rand.Read(data)
		whole = append(whole, data...)

		p, err := e.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket: "b", Key: "mp", UploadID: upload.UploadID, PartNumber: i + 1,
			Reader: bytes.NewReader(data), Size: int64(n),
		})
		require.NoError(t, err)

		completed = append(completed, fs.CompletedPart{PartNumber: i + 1, ETag: p.ETag})
	}

	done, err := e.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "b", Key: "mp", UploadID: upload.UploadID, Parts: completed,
	})
	require.NoError(t, err)
	assert.Equal(t, sse.Algorithm, done.ServerSideEncryption)

	resp, err := e.GetObject(ctx, "b", "mp")
	require.NoError(t, err)
	require.Equal(t, int64(len(whole)), resp.Size)

	rs := resp.Reader.(io.ReadSeeker)

	all, err := io.ReadAll(rs)
	require.NoError(t, err)
	require.Equal(t, whole, all)

	for _, c := range []struct{ off, n int }{
		{0, 1}, {65535, 2}, {69990, 30}, {70001 + 65536 - 3, 10}, {len(whole) - 1000, 1000}, {123456, 50000},
	} {
		_, err := rs.Seek(int64(c.off), io.SeekStart)
		require.NoError(t, err)

		got := make([]byte, c.n)
		_, err = io.ReadFull(rs, got)
		require.NoError(t, err)
		require.Equal(t, whole[c.off:c.off+c.n], got, "offset %d", c.off)
	}
}

func TestKeyRotation(t *testing.T) {
	old, current := masterKey(t), masterKey(t)
	nodes := cluster(t, 1, Config{BlockSize: 4096, Keyring: keyring(t, old)})
	e := nodes[0].engine

	require.NoError(t, e.CreateBucket(context.Background(), "b"))
	putSealed(t, e, "k", []byte("written under the old key"))

	// The key is rotated: new writes use the current one, and what the old
	// one sealed still reads while the ring keeps it.
	e.keyring = keyring(t, current, old)

	resp, err := e.GetObject(context.Background(), "b", "k")
	require.NoError(t, err)

	got, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)
	assert.Equal(t, "written under the old key", string(got))

	// Without any key, the object is unreadable — and says so rather than
	// serving ciphertext.
	e.keyring = nil

	_, err = e.GetObject(context.Background(), "b", "k")
	require.ErrorIs(t, err, fs.ErrUnsupportedOperation)
}

func readAll(t *testing.T, e *Engine, key string) string {
	t.Helper()

	resp, err := e.GetObject(context.Background(), "b", key)
	require.NoError(t, err)

	defer func() { _ = resp.Reader.Close() }()

	got, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)

	return string(got)
}

// TestRotateThenRetire is the point of rotation: once it reports nothing
// remaining, the old master key can go and everything still reads — objects,
// and an upload in flight that completes after the rotation.
func TestRotateThenRetire(t *testing.T) {
	ctx := context.Background()
	old, current := masterKey(t), masterKey(t)

	e := sealedEngine(t, keyring(t, old))

	small, large := "inline under the old key", string(bytes.Repeat([]byte("blocks "), 3000))
	putSealed(t, e, "small", []byte(small))
	putSealed(t, e, "large", []byte(large))

	up, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "mp", ServerSideEncryption: sse.Algorithm})
	require.NoError(t, err)

	part, err := e.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket: "b", Key: "mp", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader([]byte(large)), Size: int64(len(large)),
	})
	require.NoError(t, err)

	// Without the old key in the ring, nothing can move.
	e.keyring = keyring(t, current)

	res, err := e.RotateKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Remaining)
	assert.Len(t, res.Failed, 3)
	assert.Contains(t, res.Failed[0], "no master key")

	e.keyring = keyring(t, current, old)

	res, err = e.RotateKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, RotateResult{Rewrapped: 3}, res)

	res, err = e.RotateKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, RotateResult{Current: 3}, res, "a second run has nothing to do")

	// Retire the old key.
	e.keyring = keyring(t, current)

	assert.Equal(t, small, readAll(t, e, "small"))
	assert.Equal(t, large, readAll(t, e, "large"))

	_, err = e.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "b", Key: "mp", UploadID: up.UploadID, Parts: []fs.CompletedPart{{PartNumber: 1, ETag: part.ETag}},
	})
	require.NoError(t, err)
	assert.Equal(t, large, readAll(t, e, "mp"))
}

// TestCustomerKeyAtRest: an SSE-C object's stored bytes are ciphertext, and
// nothing stored holds the customer key.
func TestCustomerKeyAtRest(t *testing.T) {
	e := sealedEngine(t, nil)
	ck := fs.CustomerKey(bytes.Repeat([]byte{7}, 32))
	body := bytes.Repeat([]byte("customer plaintext "), 1000)

	_, err := e.PutObject(fs.WithCustomerKey(context.Background(), ck), &fs.PutObjectRequest{
		Bucket: "b", Key: "k", Reader: bytes.NewReader(body), Size: int64(len(body)),
	})
	require.NoError(t, err)

	at := stored(t, e, "k")
	assert.NotContains(t, string(at), "customer plaintext")
	assert.NotContains(t, string(at), string(ck))

	_, inc, err := e.bucket(context.Background(), "b")
	require.NoError(t, err)

	o, _, err := e.objects.Get(context.Background(), inc.ID, "k")
	require.NoError(t, err)
	assert.NotContains(t, string(mustJSON(o)), string(ck), "the key is never in the metadata")
}

func TestRotateSkipsCustomerKeys(t *testing.T) {
	ctx := context.Background()
	e := sealedEngine(t, keyring(t, masterKey(t)))

	_, err := e.PutObject(fs.WithCustomerKey(ctx, bytes.Repeat([]byte{7}, 32)), &fs.PutObjectRequest{
		Bucket: "b", Key: "k", Reader: bytes.NewReader([]byte("x")), Size: 1,
	})
	require.NoError(t, err)

	e.keyring = keyring(t, masterKey(t))

	res, err := e.RotateKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, RotateResult{}, res, "an SSE-C key is the client's, not the server's to rewrap")
}
