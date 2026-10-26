package integration

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/server"
)

// vhostDomain is the service domain the virtual-host tests configure. No DNS
// resolves it: vhostClient dials the test server for every name under it.
const vhostDomain = "s3.test"

// newVhostServer starts an auth-enabled server serving virtual hosts under
// vhostDomain, and returns its port and an HTTP client that reaches it by any
// name.
func newVhostServer(t *testing.T) (port string, client *http.Client) {
	t.Helper()

	storage, err := engine.Open(t.TempDir(), engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = storage.Close() })

	store, err := auth.NewStore(adminConfig())
	require.NoError(t, err)

	srv := httptest.NewServer(server.NewHandler(storage, server.WithAuth(store), server.WithDomains([]string{vhostDomain})))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	addr := u.Host
	dialer := &net.Dialer{Timeout: 5 * time.Second}

	return u.Port(), &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
	}}
}

// TestAWS_VirtualHost drives aws-sdk-go-v2 with path-style off: every bucket
// request goes to <bucket>.s3.test, signed over the path without the bucket.
func TestAWS_VirtualHost(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	port, httpClient := newVhostServer(t)

	client := awss3.New(awss3.Options{
		BaseEndpoint: aws.String("http://" + vhostDomain + ":" + port),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(authAccessKey, authSecretKey, ""),
		HTTPClient:   httpClient,
	})

	const bucket, key = "photos", "2024/a b+c.jpg"

	_, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)

	_, err = client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader([]byte("cat")),
	})
	require.NoError(t, err)

	got, err := client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(t, err)

	body, err := io.ReadAll(got.Body)
	require.NoError(t, err)
	require.Equal(t, "cat", string(body))

	list, err := client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, list.Contents, 1)
	require.Equal(t, key, aws.ToString(list.Contents[0].Key))

	// Multipart: initiation, a part and completion all address the bucket by
	// host.
	up, err := client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("big")})
	require.NoError(t, err)

	part, err := client.UploadPart(ctx, &awss3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String("big"), UploadId: up.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader([]byte("part")),
	})
	require.NoError(t, err)

	_, err = client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String("big"), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: part.ETag, PartNumber: aws.Int32(1)}}},
	})
	require.NoError(t, err)

	// A presigned URL carries the bucket in its host.
	presigned, err := awss3.NewPresignClient(client).PresignGetObject(ctx,
		&awss3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(presigned.URL, "http://"+bucket+"."+vhostDomain+":"), presigned.URL)

	resp, err := httpClient.Get(presigned.URL)
	require.NoError(t, err)

	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Equal(t, "cat", string(body))

	_, err = client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(t, err)

	// Path-style keeps working on the bare domain, against the same data.
	pathStyle := awss3.New(awss3.Options{
		BaseEndpoint: aws.String("http://" + vhostDomain + ":" + port),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(authAccessKey, authSecretKey, ""),
		HTTPClient:   httpClient,
	})

	head, err := pathStyle.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("big")})
	require.NoError(t, err)
	require.Equal(t, int64(4), aws.ToInt64(head.ContentLength))
}

// TestMinio_VirtualHost drives minio-go with DNS bucket lookup, which signs
// its uploads with streaming (aws-chunked) signatures.
func TestMinio_VirtualHost(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	port, httpClient := newVhostServer(t)

	client, err := minio.New(vhostDomain+":"+port, &minio.Options{
		Creds:        miniocreds.NewStaticV4(authAccessKey, authSecretKey, ""),
		BucketLookup: minio.BucketLookupDNS,
		Transport:    httpClient.Transport,
	})
	require.NoError(t, err)

	const bucket = "docs"

	require.NoError(t, client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))

	content := strings.Repeat("x", 100<<10)
	_, err = client.PutObject(ctx, bucket, "dir/file.txt", strings.NewReader(content), int64(len(content)),
		minio.PutObjectOptions{})
	require.NoError(t, err)

	obj, err := client.GetObject(ctx, bucket, "dir/file.txt", minio.GetObjectOptions{})
	require.NoError(t, err)

	data, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.Equal(t, content, string(data))

	var keys []string

	for o := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		require.NoError(t, o.Err)
		keys = append(keys, o.Key)
	}

	require.Equal(t, []string{"dir/file.txt"}, keys)
}
