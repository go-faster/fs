package integration

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"
)

func TestIntegration_RangeGet(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const bucket = "range-bucket"
	require.NoError(t, client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))

	content := []byte("0123456789")
	_, err := client.PutObject(ctx, bucket, "obj", bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{})
	require.NoError(t, err)

	opts := minio.GetObjectOptions{}
	require.NoError(t, opts.SetRange(2, 5))

	obj, err := client.GetObject(ctx, bucket, "obj", opts)
	require.NoError(t, err)

	t.Cleanup(func() { _ = obj.Close() })

	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.Equal(t, "2345", string(got))
}
