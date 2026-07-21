package integration

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	aws "github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagefs"
)

const (
	authAccessKey = "AKIAIOSFODNN7EXAMPLE"
	authSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

// newAuthServer starts an in-process auth-enabled server and returns its
// endpoint host and the backing auth store.
func newAuthServer(t testing.TB, cfg auth.Config) string {
	t.Helper()

	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	store, err := auth.NewStore(cfg)
	require.NoError(t, err)

	srv := httptest.NewServer(server.NewHandler(storage, server.WithAuth(store)))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	return u.Host
}

// adminConfig grants one key admin over all buckets.
func adminConfig() auth.Config {
	return auth.Config{
		Keys: []auth.Key{{
			AccessKey: authAccessKey,
			SecretKey: authSecretKey,
			Grants:    []auth.Grant{{Pattern: "*", Permission: auth.Admin}},
		}},
	}
}

func minioClient(t testing.TB, endpoint, access, secret string) *minio.Client {
	t.Helper()

	c, err := minio.New(endpoint, &minio.Options{
		Creds:  miniocreds.NewStaticV4(access, secret, ""),
		Secure: false,
	})
	require.NoError(t, err)

	return c
}

func awsClient(t testing.TB, endpoint, access, secret string) *awss3.Client {
	t.Helper()

	return awss3.New(awss3.Options{
		BaseEndpoint: aws.String("http://" + endpoint),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  awscreds.NewStaticCredentialsProvider(access, secret, ""),
	})
}

// TestAuth_MinioRoundTrip exercises minio-go against an auth-enabled server. Its
// PutObject uses STREAMING-AWS4-HMAC-SHA256-PAYLOAD, so this validates per-chunk
// signature verification end-to-end.
func TestAuth_MinioRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	endpoint := newAuthServer(t, adminConfig())
	client := minioClient(t, endpoint, authAccessKey, authSecretKey)

	const bucket = "auth-bucket"

	require.NoError(t, client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))

	// A multi-KiB body forces minio-go's streaming signed chunks.
	content := bytes.Repeat([]byte("go-faster/fs signed streaming payload. "), 2000)
	_, err := client.PutObject(ctx, bucket, "obj.bin", bytes.NewReader(content), int64(len(content)),
		minio.PutObjectOptions{})
	require.NoError(t, err)

	obj, err := client.GetObject(ctx, bucket, "obj.bin", minio.GetObjectOptions{})
	require.NoError(t, err)

	defer func() { _ = obj.Close() }()

	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.Equal(t, content, got)

	var listed []string

	for o := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		require.NoError(t, o.Err)
		listed = append(listed, o.Key)
	}

	require.Equal(t, []string{"obj.bin"}, listed)

	require.NoError(t, client.RemoveObject(ctx, bucket, "obj.bin", minio.RemoveObjectOptions{}))
}

// TestAuth_AWSRoundTrip exercises aws-sdk-go-v2 (which uses
// STREAMING-UNSIGNED-PAYLOAD-TRAILER for uploads) against auth.
func TestAuth_AWSRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	endpoint := newAuthServer(t, adminConfig())
	client := awsClient(t, endpoint, authAccessKey, authSecretKey)

	const bucket = "auth-bucket"

	_, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)

	content := []byte("aws sdk authenticated upload")
	_, err = client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("obj.txt"),
		Body:   bytes.NewReader(content),
	})
	require.NoError(t, err)

	get, err := client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("obj.txt")})
	require.NoError(t, err)

	defer func() { _ = get.Body.Close() }()

	got, err := io.ReadAll(get.Body)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestAuth_WrongSecret(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	endpoint := newAuthServer(t, adminConfig())
	client := minioClient(t, endpoint, authAccessKey, "the-wrong-secret")

	err := client.MakeBucket(ctx, "any-bucket", minio.MakeBucketOptions{})
	require.Error(t, err)

	resp := minio.ToErrorResponse(err)
	require.Equal(t, "SignatureDoesNotMatch", resp.Code)
}

func TestAuth_UnknownKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	endpoint := newAuthServer(t, adminConfig())
	client := minioClient(t, endpoint, "AKIAUNKNOWN", "whatever-secret")

	err := client.MakeBucket(ctx, "any-bucket", minio.MakeBucketOptions{})
	require.Error(t, err)
	require.Equal(t, "InvalidAccessKeyId", minio.ToErrorResponse(err).Code)
}

func TestAuth_Anonymous(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	cfg := adminConfig()
	cfg.PublicReadBuckets = []string{"public"}
	endpoint := newAuthServer(t, cfg)

	admin := minioClient(t, endpoint, authAccessKey, authSecretKey)
	require.NoError(t, admin.MakeBucket(ctx, "public", minio.MakeBucketOptions{}))
	require.NoError(t, admin.MakeBucket(ctx, "private", minio.MakeBucketOptions{}))

	content := []byte("readable by anyone")
	_, err := admin.PutObject(ctx, "public", "open.txt", bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{})
	require.NoError(t, err)
	_, err = admin.PutObject(ctx, "private", "secret.txt", bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{})
	require.NoError(t, err)

	anon, err := minio.New(endpoint, &minio.Options{Secure: false})
	require.NoError(t, err)

	t.Run("PublicReadAllowed", func(t *testing.T) {
		obj, err := anon.GetObject(ctx, "public", "open.txt", minio.GetObjectOptions{})
		require.NoError(t, err)

		defer func() { _ = obj.Close() }()

		got, err := io.ReadAll(obj)
		require.NoError(t, err)
		require.Equal(t, content, got)
	})

	t.Run("PrivateReadDenied", func(t *testing.T) {
		obj, err := anon.GetObject(ctx, "private", "secret.txt", minio.GetObjectOptions{})
		require.NoError(t, err)

		defer func() { _ = obj.Close() }()

		_, err = io.ReadAll(obj)
		require.Equal(t, "AccessDenied", minio.ToErrorResponse(err).Code)
	})

	t.Run("AnonymousWriteDenied", func(t *testing.T) {
		_, err := anon.PutObject(ctx, "public", "hack.txt", bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{})
		require.Error(t, err)

		// minio-go's anonymous PutObject still attaches an (empty-credential)
		// streaming signature, so the denial surfaces as InvalidAccessKeyId;
		// a truly unsigned write is AccessDenied. Both are 403 denials.
		resp := minio.ToErrorResponse(err)
		require.Equal(t, 403, resp.StatusCode)
		require.Contains(t, []string{"AccessDenied", "InvalidAccessKeyId"}, resp.Code)
	})
}

func TestAuth_GrantEnforcement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	const roKey, roSecret = "AKIAREADONLY00000000", "read-only-secret-value-000000000000000000"

	cfg := auth.Config{
		Keys: []auth.Key{
			{AccessKey: authAccessKey, SecretKey: authSecretKey, Grants: []auth.Grant{{Pattern: "*", Permission: auth.Admin}}},
			{AccessKey: roKey, SecretKey: roSecret, Grants: []auth.Grant{{Pattern: "shared", Permission: auth.Read}}},
		},
	}
	endpoint := newAuthServer(t, cfg)

	admin := minioClient(t, endpoint, authAccessKey, authSecretKey)
	require.NoError(t, admin.MakeBucket(ctx, "shared", minio.MakeBucketOptions{}))

	content := []byte("shared content")
	_, err := admin.PutObject(ctx, "shared", "file.txt", bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{})
	require.NoError(t, err)

	ro := minioClient(t, endpoint, roKey, roSecret)

	t.Run("ReadAllowed", func(t *testing.T) {
		obj, err := ro.GetObject(ctx, "shared", "file.txt", minio.GetObjectOptions{})
		require.NoError(t, err)

		defer func() { _ = obj.Close() }()

		got, err := io.ReadAll(obj)
		require.NoError(t, err)
		require.Equal(t, content, got)
	})

	t.Run("WriteDenied", func(t *testing.T) {
		_, err := ro.PutObject(ctx, "shared", "nope.txt", bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{})
		require.Equal(t, "AccessDenied", minio.ToErrorResponse(err).Code)
	})

	t.Run("OtherBucketDenied", func(t *testing.T) {
		_, err := ro.GetObject(ctx, "shared", "file.txt", minio.GetObjectOptions{})
		require.NoError(t, err) // sanity: same bucket read is fine
		require.NoError(t, admin.MakeBucket(ctx, "other", minio.MakeBucketOptions{}))

		obj, err := ro.GetObject(ctx, "other", "file.txt", minio.GetObjectOptions{})
		require.NoError(t, err)

		defer func() { _ = obj.Close() }()

		_, err = io.ReadAll(obj)
		require.Equal(t, "AccessDenied", minio.ToErrorResponse(err).Code)
	})
}

func TestAuth_PresignedURL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	endpoint := newAuthServer(t, adminConfig())
	client := minioClient(t, endpoint, authAccessKey, authSecretKey)

	const bucket = "auth-bucket"

	require.NoError(t, client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))

	content := []byte("presigned content")
	_, err := client.PutObject(ctx, bucket, "obj.txt", bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{})
	require.NoError(t, err)

	presigned, err := client.PresignedGetObject(ctx, bucket, "obj.txt", 15*time.Minute, url.Values{})
	require.NoError(t, err)

	resp, err := http.Get(presigned.String()) //nolint:noctx // test fetch of an in-process URL.
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, content, got)
}
