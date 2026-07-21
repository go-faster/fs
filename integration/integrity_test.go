package integration

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagefs"
)

// TestIntegrity_VerifyOnReadServes500 corrupts an object on disk and checks that
// a verify-on-read server refuses to serve it (500) instead of returning bad
// bytes, while a healthy object reads fine.
func TestIntegrity_VerifyOnReadServes500(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	root := t.TempDir()
	store, err := storagefs.New(root, storagefs.WithVerifyReads(true))
	require.NoError(t, err)

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

	rotten := []byte("this content will rot on disk")
	_, err = client.PutObject(ctx, bucket, "bad.txt", bytes.NewReader(rotten), int64(len(rotten)), minio.PutObjectOptions{})
	require.NoError(t, err)

	// Flip a byte directly in the object file to simulate bit-rot.
	path := filepath.Join(root, bucket, "bad.txt")

	data, err := os.ReadFile(path) //nolint:gosec // test path.
	require.NoError(t, err)

	data[0] ^= 0xFF
	require.NoError(t, os.WriteFile(path, data, 0o600))

	t.Run("HealthyReadsFine", func(t *testing.T) {
		obj, err := client.GetObject(ctx, bucket, "ok.txt", minio.GetObjectOptions{})
		require.NoError(t, err)

		defer func() { _ = obj.Close() }()

		got, err := io.ReadAll(obj)
		require.NoError(t, err)
		require.Equal(t, healthy, got)
	})

	t.Run("CorruptRefused", func(t *testing.T) {
		obj, err := client.GetObject(ctx, bucket, "bad.txt", minio.GetObjectOptions{})
		require.NoError(t, err)

		defer func() { _ = obj.Close() }()

		_, err = io.ReadAll(obj)
		require.Error(t, err)
		require.Equal(t, "InternalError", minio.ToErrorResponse(err).Code)
	})
}
