package integration

import (
	"bytes"
	"context"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"
)

func TestIntegration_DelimitedList(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const bucket = "list-bucket"
	require.NoError(t, client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))

	for _, key := range []string{"a/1", "a/2", "b/1", "top"} {
		_, err := client.PutObject(ctx, bucket, key, bytes.NewReader([]byte("x")), 1, minio.PutObjectOptions{})
		require.NoError(t, err)
	}

	var (
		keys     []string
		prefixes []string
	)

	for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: false}) {
		require.NoError(t, obj.Err)

		if obj.Size == 0 && obj.Key[len(obj.Key)-1] == '/' {
			prefixes = append(prefixes, obj.Key)
			continue
		}

		keys = append(keys, obj.Key)
	}

	require.ElementsMatch(t, []string{"top"}, keys)
	require.ElementsMatch(t, []string{"a/", "b/"}, prefixes)
}
