package integration

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pathLikeKeys are keys S3 stores as they are, which a server treating keys
// as paths would refuse or rewrite: a redirect to the cleaned path, or a 400.
var pathLikeKeys = []string{"a//b", "x/../y", "../up", "./here", "a/./b", `back\slash`, `C:\drive`, "trailing/"}

// TestAWS_PathLikeKeys round-trips each with signed aws-sdk-go-v2 requests,
// which sign the path as sent: the server must neither clean it nor refuse it.
func TestAWS_PathLikeKeys(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := awsClient(t, newAuthServer(t, adminConfig()))

	_, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String("keys")})
	require.NoError(t, err)

	for _, key := range pathLikeKeys {
		_, err := client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("keys"), Key: aws.String(key), Body: bytes.NewReader([]byte(key))})
		require.NoError(t, err, "put %q", key)

		out, err := client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("keys"), Key: aws.String(key)})
		require.NoError(t, err, "get %q", key)

		got, err := io.ReadAll(out.Body)
		_ = out.Body.Close()

		require.NoError(t, err)
		assert.Equal(t, key, string(got), "key %q holds its own content", key)
	}

	list, err := client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String("keys")})
	require.NoError(t, err)

	var listed []string
	for _, o := range list.Contents {
		listed = append(listed, aws.ToString(o.Key))
	}

	assert.ElementsMatch(t, pathLikeKeys, listed, "listed exactly as written")
}

// TestMinio_PathLikeKeys is the same through minio-go's streaming signatures.
func TestMinio_PathLikeKeys(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := minioClient(t, newAuthServer(t, adminConfig()), authAccessKey, authSecretKey)

	require.NoError(t, client.MakeBucket(ctx, "keys", minio.MakeBucketOptions{}))

	for _, key := range pathLikeKeys {
		_, err := client.PutObject(ctx, "keys", key, bytes.NewReader([]byte(key)), int64(len(key)), minio.PutObjectOptions{})
		require.NoError(t, err, "put %q", key)

		obj, err := client.GetObject(ctx, "keys", key, minio.GetObjectOptions{})
		require.NoError(t, err)

		got, err := io.ReadAll(obj)
		_ = obj.Close()

		require.NoError(t, err, "get %q", key)
		assert.Equal(t, key, string(got), "key %q holds its own content", key)
	}
}
