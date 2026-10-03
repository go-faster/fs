package integration

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // S3 names an SSE-C key by its MD5.
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/encrypt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/server"
)

// tlsServer is an engine behind a TLS listener: SSE-C keys are refused on
// plain HTTP.
func tlsServer(t *testing.T) *httptest.Server {
	t.Helper()

	storage, err := engine.Open(t.TempDir(), engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = storage.Close() })

	srv := httptest.NewTLSServer(server.NewHandler(storage))
	t.Cleanup(srv.Close)

	return srv
}

func awsClientFor(srv *httptest.Server) *s3.Client {
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		HTTPClient:   srv.Client(),
	})
}

// sseC is an SSE-C key as the SDK takes it: the key and its MD5, base64.
type sseC struct{ key, md5 string }

func newSSEC(b byte) sseC {
	k := bytes.Repeat([]byte{b}, 32)
	sum := md5.Sum(k) //nolint:gosec // S3's key checksum.

	return sseC{base64.StdEncoding.EncodeToString(k), base64.StdEncoding.EncodeToString(sum[:])}
}

func TestAWS_SSEC(t *testing.T) {
	ctx := context.Background()
	srv := tlsServer(t)
	client := awsClientFor(srv)
	key, other := newSSEC(1), newSSEC(2)
	body := bytes.Repeat([]byte("customer encrypted "), 4000)

	require.NoError(t, createBucket(ctx, client, "ssec"))

	put, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("ssec"), Key: aws.String("obj"), Body: bytes.NewReader(body),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(key.key), SSECustomerKeyMD5: aws.String(key.md5),
	})
	require.NoError(t, err)
	assert.Equal(t, "AES256", aws.ToString(put.SSECustomerAlgorithm))
	assert.Equal(t, key.md5, aws.ToString(put.SSECustomerKeyMD5))
	assert.Empty(t, put.ServerSideEncryption)

	get := func(k *sseC, rng string) (*s3.GetObjectOutput, error) {
		in := &s3.GetObjectInput{Bucket: aws.String("ssec"), Key: aws.String("obj")}
		if rng != "" {
			in.Range = aws.String(rng)
		}

		if k != nil {
			in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5 = aws.String("AES256"), aws.String(k.key), aws.String(k.md5)
		}

		return client.GetObject(ctx, in)
	}

	out, err := get(&key, "")
	require.NoError(t, err)

	got, err := io.ReadAll(out.Body)
	require.NoError(t, err)
	assert.Equal(t, body, got)
	assert.Equal(t, key.md5, aws.ToString(out.SSECustomerKeyMD5))

	out, err = get(&key, "bytes=100-199")
	require.NoError(t, err)

	got, err = io.ReadAll(out.Body)
	require.NoError(t, err)
	assert.Equal(t, body[100:200], got, "ranges decrypt")

	_, err = get(nil, "")
	assert.Equal(t, http.StatusBadRequest, httpStatus(t, err), "no key")

	_, err = get(&other, "")
	assert.Equal(t, http.StatusForbidden, httpStatus(t, err), "the wrong key")

	_, err = client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("ssec"), Key: aws.String("obj")})
	assert.Equal(t, http.StatusBadRequest, httpStatus(t, err), "HEAD without the key")

	// Copy: decrypt the source with its key, encrypt the copy with another.
	_, err = client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("ssec"), Key: aws.String("copy"), CopySource: aws.String("ssec/obj"),
		CopySourceSSECustomerAlgorithm: aws.String("AES256"), CopySourceSSECustomerKey: aws.String(key.key), CopySourceSSECustomerKeyMD5: aws.String(key.md5),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(other.key), SSECustomerKeyMD5: aws.String(other.md5),
	})
	require.NoError(t, err)

	cp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("ssec"), Key: aws.String("copy"),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(other.key), SSECustomerKeyMD5: aws.String(other.md5),
	})
	require.NoError(t, err)

	got, err = io.ReadAll(cp.Body)
	require.NoError(t, err)
	assert.Equal(t, body, got)

	// A key whose MD5 does not match it.
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("ssec"), Key: aws.String("bad"), Body: bytes.NewReader(body),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(key.key), SSECustomerKeyMD5: aws.String(other.md5),
	})
	assert.Equal(t, "InvalidArgument", s3ErrorCode(t, err))
}

func TestAWS_SSECMultipart(t *testing.T) {
	ctx := context.Background()
	client := awsClientFor(tlsServer(t))
	key := newSSEC(3)
	part := bytes.Repeat([]byte("p"), 5<<20)

	require.NoError(t, createBucket(ctx, client, "ssec"))

	up, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("ssec"), Key: aws.String("mp"),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(key.key), SSECustomerKeyMD5: aws.String(key.md5),
	})
	require.NoError(t, err)

	var parts []types.CompletedPart

	for i, data := range [][]byte{part, []byte("tail")} {
		p, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("ssec"), Key: aws.String("mp"), UploadId: up.UploadId, PartNumber: aws.Int32(int32(i + 1)),
			Body:                 bytes.NewReader(data),
			SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(key.key), SSECustomerKeyMD5: aws.String(key.md5),
		})
		require.NoError(t, err)

		parts = append(parts, types.CompletedPart{ETag: p.ETag, PartNumber: aws.Int32(int32(i + 1))})
	}

	_, err = client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String("ssec"), Key: aws.String("mp"), UploadId: up.UploadId, PartNumber: aws.Int32(3),
		Body: bytes.NewReader([]byte("no key")),
	})
	assert.Equal(t, http.StatusBadRequest, httpStatus(t, err), "every part carries the upload's key")

	_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("ssec"), Key: aws.String("mp"), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	require.NoError(t, err)

	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("ssec"), Key: aws.String("mp"), Range: aws.String("bytes=5242878-5242883"),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(key.key), SSECustomerKeyMD5: aws.String(key.md5),
	})
	require.NoError(t, err)

	got, err := io.ReadAll(out.Body)
	require.NoError(t, err)
	assert.Equal(t, "pptail", string(got), "a range across the part boundary")
}

func TestAWS_SSECRefusedOverHTTP(t *testing.T) {
	ctx := context.Background()
	client := newAWSClient(t)
	key := newSSEC(1)

	require.NoError(t, createBucket(ctx, client, "ssec"))

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("ssec"), Key: aws.String("obj"), Body: bytes.NewReader([]byte("x")),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(key.key), SSECustomerKeyMD5: aws.String(key.md5),
	})
	assert.Equal(t, "InvalidRequest", s3ErrorCode(t, err))
}

func TestMinio_SSEC(t *testing.T) {
	ctx := context.Background()
	srv := tlsServer(t)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	client, err := minio.New(u.Host, &minio.Options{Secure: true, Transport: srv.Client().Transport})
	require.NoError(t, err)

	key, err := encrypt.NewSSEC(bytes.Repeat([]byte{5}, 32))
	require.NoError(t, err)

	other, err := encrypt.NewSSEC(bytes.Repeat([]byte{6}, 32))
	require.NoError(t, err)

	require.NoError(t, client.MakeBucket(ctx, "ssec", minio.MakeBucketOptions{}))

	body := bytes.Repeat([]byte("minio customer key "), 3000)

	_, err = client.PutObject(ctx, "ssec", "obj", bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ServerSideEncryption: key})
	require.NoError(t, err)

	read := func(sse encrypt.ServerSide) ([]byte, error) {
		obj, err := client.GetObject(ctx, "ssec", "obj", minio.GetObjectOptions{ServerSideEncryption: sse})
		if err != nil {
			return nil, err
		}

		defer func() { _ = obj.Close() }()

		return io.ReadAll(obj)
	}

	got, err := read(key)
	require.NoError(t, err)
	assert.Equal(t, body, got)

	_, err = read(other)
	assert.Equal(t, http.StatusForbidden, minio.ToErrorResponse(err).StatusCode, "the wrong key")

	_, err = read(nil)
	assert.Equal(t, http.StatusBadRequest, minio.ToErrorResponse(err).StatusCode, "no key")

	_, err = client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: "ssec", Object: "copy", Encryption: other},
		minio.CopySrcOptions{Bucket: "ssec", Object: "obj", Encryption: key})
	require.NoError(t, err)

	obj, err := client.GetObject(ctx, "ssec", "copy", minio.GetObjectOptions{ServerSideEncryption: other})
	require.NoError(t, err)

	got, err = io.ReadAll(obj)
	require.NoError(t, err)
	assert.Equal(t, body, got)
}
