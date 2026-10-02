package integration

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/server"
)

// TestIntegrity_CorruptBlockNeverServed corrupts an object's block on disk and
// checks the server never serves the corrupt bytes as content, while a healthy
// object reads fine. Every block is verified against its hash on read.
func TestIntegrity_CorruptBlockNeverServed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()

	store, err := engine.Open(root, engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	srv := httptest.NewServer(server.NewHandler(store))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	client, err := minio.New(u.Host, &minio.Options{Secure: false})
	require.NoError(t, err)

	const bucket = "bucket-a"
	require.NoError(t, client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))

	healthy := []byte("this content is intact")
	_, err = client.PutObject(ctx, bucket, "ok.txt", bytes.NewReader(healthy), int64(len(healthy)), minio.PutObjectOptions{})
	require.NoError(t, err)

	// Large enough to be stored as a block rather than inline: the only
	// block in the store.
	rotten := bytes.Repeat([]byte("this content will rot on disk;"), 300)
	_, err = client.PutObject(ctx, bucket, "bad.txt", bytes.NewReader(rotten), int64(len(rotten)), minio.PutObjectOptions{})
	require.NoError(t, err)

	var blocks []string

	require.NoError(t, filepath.WalkDir(filepath.Join(root, "blocks"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blocks = append(blocks, p)
		}

		return err
	}))
	require.Len(t, blocks, 1)

	data, err := os.ReadFile(blocks[0]) //nolint:gosec // test path.
	require.NoError(t, err)

	data[0] ^= 0xFF
	require.NoError(t, os.WriteFile(blocks[0], data, 0o600))

	t.Run("HealthyReadsFine", func(t *testing.T) {
		obj, err := client.GetObject(ctx, bucket, "ok.txt", minio.GetObjectOptions{})
		require.NoError(t, err)

		defer func() { _ = obj.Close() }()

		got, err := io.ReadAll(obj)
		require.NoError(t, err)
		require.Equal(t, healthy, got)
	})

	t.Run("CorruptNeverServed", func(t *testing.T) {
		obj, err := client.GetObject(ctx, bucket, "bad.txt", minio.GetObjectOptions{})
		require.NoError(t, err)

		defer func() { _ = obj.Close() }()

		// The response may already be under way when the block is read, so
		// the refusal can arrive as an error status or as a body cut short.
		// Either way, no corrupt byte reaches the client as content.
		got, err := io.ReadAll(obj)
		require.Error(t, err)
		require.NotEqual(t, rotten, got)
		require.NotContains(t, string(got), string(data[:16]))
	})
}
