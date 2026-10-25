package integration

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/engine"
)

// newQuotaServer serves an engine whose bucket "quota-bucket" may hold one object.
func newQuotaServer(t *testing.T) *TestServer {
	t.Helper()

	ctx := context.Background()

	storage, err := engine.Open(t.TempDir(), engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = storage.Close() })

	require.NoError(t, storage.CreateBucket(ctx, "quota-bucket"))
	require.NoError(t, storage.SetBucketQuota(ctx, "quota-bucket", engine.Quota{Enabled: true, MaxSize: 64 << 10, MaxObjects: 1}))

	return newTestServer(t, storage)
}

// TestQuotaExceeded: a write past a quota is refused the way Ceph RGW
// refuses it, 403 QuotaExceeded, and a delete makes room again.
func TestQuotaExceeded(t *testing.T) {
	ctx := context.Background()

	t.Run("minio-go", func(t *testing.T) {
		client := NewClient(t, newQuotaServer(t))

		_, err := client.PutObject(ctx, "quota-bucket", "one", bytes.NewReader([]byte("data")), 4, minio.PutObjectOptions{})
		require.NoError(t, err)

		_, err = client.PutObject(ctx, "quota-bucket", "two", bytes.NewReader([]byte("data")), 4, minio.PutObjectOptions{})
		resp := minio.ToErrorResponse(err)
		assert.Equal(t, "QuotaExceeded", resp.Code)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)

		// Server-side copies count too.
		_, err = client.CopyObject(ctx,
			minio.CopyDestOptions{Bucket: "quota-bucket", Object: "copy"}, minio.CopySrcOptions{Bucket: "quota-bucket", Object: "one"})
		assert.Equal(t, "QuotaExceeded", minio.ToErrorResponse(err).Code)

		require.NoError(t, client.RemoveObject(ctx, "quota-bucket", "one", minio.RemoveObjectOptions{}))

		_, err = client.PutObject(ctx, "quota-bucket", "two", bytes.NewReader([]byte("data")), 4, minio.PutObjectOptions{})
		require.NoError(t, err)
	})

	t.Run("aws-sdk-go-v2", func(t *testing.T) {
		srv := newQuotaServer(t)
		client := s3.New(s3.Options{
			BaseEndpoint: aws.String(srv.URL),
			Region:       "us-east-1",
			UsePathStyle: true,
			Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		})

		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("quota-bucket"), Key: aws.String("one"), Body: bytes.NewReader([]byte("data")),
		})
		require.NoError(t, err)

		_, err = client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("quota-bucket"), Key: aws.String("two"), Body: bytes.NewReader([]byte("data")),
		})
		assert.Equal(t, "QuotaExceeded", s3ErrorCode(t, err))
		assert.Equal(t, http.StatusForbidden, httpStatus(t, err))

		// Overwriting the one object stored does not grow the count.
		_, err = client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("quota-bucket"), Key: aws.String("one"), Body: bytes.NewReader([]byte("new data")),
		})
		require.NoError(t, err)

		// A multipart upload is refused at the part that cannot fit. The part
		// is over the SDK's 2 MiB Expect: 100-continue threshold: the SDK
		// sends that header but never waits for the 100 (smithy-go leaves the
		// request's Proto unset), so the body arrives though the server
		// refused it before reading; the error still reaches the client.
		up, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("quota-bucket"), Key: aws.String("big")})
		require.NoError(t, err)

		_, err = client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("quota-bucket"), Key: aws.String("big"), UploadId: up.UploadId, PartNumber: aws.Int32(1),
			Body: bytes.NewReader(make([]byte, 8<<20)),
		})
		assert.Equal(t, "QuotaExceeded", s3ErrorCode(t, err))
	})
}
