package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/storagefs"
)

// newAWSClient returns an aws-sdk-go-v2 S3 client wired to an in-process server
// backed by storagefs. The SDK is configured for a bare, path-style,
// anonymous endpoint: the server ignores request signatures, so the static
// credentials are arbitrary placeholders.
func newAWSClient(t testing.TB) *s3.Client {
	t.Helper()

	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	srv := newTestServer(t, storage)

	return s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
	})
}

// s3ErrorCode extracts the S3 API error code (e.g. NoSuchBucket) from an SDK
// error, so tests assert on the wire contract rather than message text.
func s3ErrorCode(t testing.TB, err error) string {
	t.Helper()
	require.Error(t, err)

	var apiErr smithy.APIError
	require.ErrorAs(t, err, &apiErr)

	return apiErr.ErrorCode()
}

// httpStatus extracts the HTTP status code from an SDK error.
func httpStatus(t testing.TB, err error) int {
	t.Helper()
	require.Error(t, err)

	var respErr *smithyhttp.ResponseError
	require.ErrorAs(t, err, &respErr)

	return respErr.HTTPStatusCode()
}

func TestAWS_BucketLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newAWSClient(t)

	const bucket = "aws-bucket"

	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)

	_, err = client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)

	list, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	require.NoError(t, err)
	require.Len(t, list.Buckets, 1)
	require.Equal(t, bucket, aws.ToString(list.Buckets[0].Name))

	_, err = client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)

	_, err = client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	require.Equal(t, 404, httpStatus(t, err))
}

func TestAWS_ObjectRoundTripWithMetadata(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newAWSClient(t)

	const (
		bucket = "aws-bucket"
		key    = "docs/report.txt"
	)

	require.NoError(t, createBucket(ctx, client, bucket))

	content := []byte("hello from aws-sdk-go-v2")

	put, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:             aws.String(bucket),
		Key:                aws.String(key),
		Body:               bytes.NewReader(content),
		ContentType:        aws.String("text/plain"),
		CacheControl:       aws.String("max-age=42"),
		ContentDisposition: aws.String(`attachment; filename="r.txt"`),
		Metadata:           map[string]string{"color": "blue", "owner": "aws"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, aws.ToString(put.ETag))

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(t, err)
	require.Equal(t, int64(len(content)), aws.ToInt64(head.ContentLength))
	require.Equal(t, "text/plain", aws.ToString(head.ContentType))
	require.Equal(t, "max-age=42", aws.ToString(head.CacheControl))
	require.Equal(t, `attachment; filename="r.txt"`, aws.ToString(head.ContentDisposition))
	require.Equal(t, "blue", head.Metadata["color"])
	require.Equal(t, "aws", head.Metadata["owner"])

	get, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(t, err)

	defer func() { _ = get.Body.Close() }()

	data, err := io.ReadAll(get.Body)
	require.NoError(t, err)
	require.Equal(t, content, data)
	require.Equal(t, "text/plain", aws.ToString(get.ContentType))
	require.Equal(t, aws.ToString(put.ETag), aws.ToString(get.ETag))
}

func TestAWS_GetRange(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newAWSClient(t)

	const bucket = "aws-bucket"

	require.NoError(t, createBucket(ctx, client, bucket))
	require.NoError(t, putObject(ctx, client, bucket, "data.bin", []byte("0123456789")))

	get, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("data.bin"),
		Range:  aws.String("bytes=2-5"),
	})
	require.NoError(t, err)

	defer func() { _ = get.Body.Close() }()

	data, err := io.ReadAll(get.Body)
	require.NoError(t, err)
	require.Equal(t, "2345", string(data))
	require.Equal(t, int64(4), aws.ToInt64(get.ContentLength))
}

func TestAWS_ListObjectsV2(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newAWSClient(t)

	const bucket = "aws-bucket"

	require.NoError(t, createBucket(ctx, client, bucket))

	for _, key := range []string{"a.txt", "b.txt", "docs/x.txt", "docs/y.txt"} {
		require.NoError(t, putObject(ctx, client, bucket, key, []byte("x")))
	}

	t.Run("Delimiter", func(t *testing.T) {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:    aws.String(bucket),
			Delimiter: aws.String("/"),
		})
		require.NoError(t, err)
		require.Equal(t, int32(3), aws.ToInt32(out.KeyCount)) // a.txt, b.txt, docs/
		require.Len(t, out.Contents, 2)
		require.Len(t, out.CommonPrefixes, 1)
		require.Equal(t, "docs/", aws.ToString(out.CommonPrefixes[0].Prefix))
	})

	t.Run("Pagination", func(t *testing.T) {
		page1, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:  aws.String(bucket),
			MaxKeys: aws.Int32(2),
		})
		require.NoError(t, err)
		require.True(t, aws.ToBool(page1.IsTruncated))
		require.Len(t, page1.Contents, 2)
		require.NotEmpty(t, aws.ToString(page1.NextContinuationToken))

		page2, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			ContinuationToken: page1.NextContinuationToken,
		})
		require.NoError(t, err)
		require.False(t, aws.ToBool(page2.IsTruncated))
		require.Len(t, page2.Contents, 2)
	})

	t.Run("Paginator", func(t *testing.T) {
		var keys []string

		p := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
			Bucket:  aws.String(bucket),
			MaxKeys: aws.Int32(1),
		})
		for p.HasMorePages() {
			out, err := p.NextPage(ctx)
			require.NoError(t, err)

			for _, o := range out.Contents {
				keys = append(keys, aws.ToString(o.Key))
			}
		}

		require.Equal(t, []string{"a.txt", "b.txt", "docs/x.txt", "docs/y.txt"}, keys)
	})
}

func TestAWS_CopyObject(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newAWSClient(t)

	const bucket = "aws-bucket"

	require.NoError(t, createBucket(ctx, client, bucket))
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String("src.txt"),
		Body:        strings.NewReader("payload"),
		ContentType: aws.String("text/plain"),
		Metadata:    map[string]string{"k": "v"},
	})
	require.NoError(t, err)

	t.Run("CopyPreservesMetadata", func(t *testing.T) {
		_, err := client.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket:     aws.String(bucket),
			Key:        aws.String("dst.txt"),
			CopySource: aws.String(bucket + "/src.txt"),
		})
		require.NoError(t, err)

		head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("dst.txt")})
		require.NoError(t, err)
		require.Equal(t, "text/plain", aws.ToString(head.ContentType))
		require.Equal(t, "v", head.Metadata["k"])
	})

	t.Run("ReplaceMetadata", func(t *testing.T) {
		_, err := client.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket:            aws.String(bucket),
			Key:               aws.String("dst2.txt"),
			CopySource:        aws.String(bucket + "/src.txt"),
			MetadataDirective: types.MetadataDirectiveReplace,
			ContentType:       aws.String("application/json"),
			Metadata:          map[string]string{"shape": "round"},
		})
		require.NoError(t, err)

		head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("dst2.txt")})
		require.NoError(t, err)
		require.Equal(t, "application/json", aws.ToString(head.ContentType))
		require.Equal(t, "round", head.Metadata["shape"])
		require.NotContains(t, head.Metadata, "k")
	})
}

func TestAWS_ObjectTagging(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newAWSClient(t)

	const bucket = "aws-bucket"

	require.NoError(t, createBucket(ctx, client, bucket))
	require.NoError(t, putObject(ctx, client, bucket, "obj.txt", []byte("x")))

	_, err := client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("obj.txt"),
		Tagging: &types.Tagging{TagSet: []types.Tag{
			{Key: aws.String("env"), Value: aws.String("prod")},
			{Key: aws.String("team"), Value: aws.String("storage")},
		}},
	})
	require.NoError(t, err)

	got, err := client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("obj.txt")})
	require.NoError(t, err)
	require.Len(t, got.TagSet, 2)
	require.Equal(t, "env", aws.ToString(got.TagSet[0].Key))
	require.Equal(t, "prod", aws.ToString(got.TagSet[0].Value))

	_, err = client.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("obj.txt")})
	require.NoError(t, err)

	got, err = client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("obj.txt")})
	require.NoError(t, err)
	require.Empty(t, got.TagSet)
}

func TestAWS_MultipartManual(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newAWSClient(t)

	const (
		bucket = "aws-bucket"
		key    = "big.bin"
	)

	require.NoError(t, createBucket(ctx, client, bucket))

	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		ContentType: aws.String("application/octet-stream"),
		Metadata:    map[string]string{"kind": "assembled"},
	})
	require.NoError(t, err)

	uploadID := aws.ToString(create.UploadId)
	require.NotEmpty(t, uploadID)

	// Two 5 MiB parts plus a small tail.
	part1 := bytes.Repeat([]byte("a"), 5*1024*1024)
	part2 := bytes.Repeat([]byte("b"), 5*1024*1024)
	part3 := []byte("tail")

	var completed []types.CompletedPart

	for i, body := range [][]byte{part1, part2, part3} {
		up, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:     aws.String(bucket),
			Key:        aws.String(key),
			UploadId:   aws.String(uploadID),
			PartNumber: aws.Int32(int32(i + 1)),
			Body:       bytes.NewReader(body),
		})
		require.NoError(t, err)

		completed = append(completed, types.CompletedPart{
			PartNumber: aws.Int32(int32(i + 1)),
			ETag:       up.ETag,
		})
	}

	// ListParts reflects the uploaded parts.
	parts, err := client.ListParts(ctx, &s3.ListPartsInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	require.NoError(t, err)
	require.Len(t, parts.Parts, 3)

	// ListMultipartUploads shows the in-progress upload.
	ups, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, ups.Uploads, 1)
	require.Equal(t, key, aws.ToString(ups.Uploads[0].Key))

	complete, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(aws.ToString(complete.ETag), `-3"`))

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(t, err)
	require.Equal(t, int64(len(part1)+len(part2)+len(part3)), aws.ToInt64(head.ContentLength))
	require.Equal(t, "assembled", head.Metadata["kind"])
}

func TestAWS_MultipartAbort(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newAWSClient(t)

	const bucket = "aws-bucket"

	require.NoError(t, createBucket(ctx, client, bucket))

	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("gone.bin"),
	})
	require.NoError(t, err)

	_, err = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String("gone.bin"),
		UploadId: create.UploadId,
	})
	require.NoError(t, err)

	ups, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Empty(t, ups.Uploads)
}

func TestAWS_DeleteObjects(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newAWSClient(t)

	const bucket = "aws-bucket"

	require.NoError(t, createBucket(ctx, client, bucket))

	for _, key := range []string{"x1", "x2", "x3"} {
		require.NoError(t, putObject(ctx, client, bucket, key, []byte("x")))
	}

	out, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(bucket),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("x1")},
			{Key: aws.String("x2")},
			{Key: aws.String("missing")}, // idempotent: deleting a missing key succeeds
		}},
	})
	require.NoError(t, err)
	require.Len(t, out.Deleted, 3)
	require.Empty(t, out.Errors)

	list, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, list.Contents, 1)
	require.Equal(t, "x3", aws.ToString(list.Contents[0].Key))
}

func TestAWS_ConditionalPut(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newAWSClient(t)

	const bucket = "aws-bucket"

	require.NoError(t, createBucket(ctx, client, bucket))
	require.NoError(t, putObject(ctx, client, bucket, "obj.txt", []byte("first")))

	// If-None-Match: * must fail once the object exists.
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String("obj.txt"),
		Body:        strings.NewReader("second"),
		IfNoneMatch: aws.String("*"),
	})
	require.Equal(t, 412, httpStatus(t, err))
}

func TestAWS_Errors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newAWSClient(t)

	t.Run("NoSuchBucket", func(t *testing.T) {
		_, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("missing-bucket"), Key: aws.String("k")})
		require.Equal(t, 404, httpStatus(t, err))
	})

	t.Run("NoSuchKey", func(t *testing.T) {
		require.NoError(t, createBucket(ctx, client, "aws-bucket"))

		_, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("aws-bucket"), Key: aws.String("missing")})
		require.Equal(t, "NoSuchKey", s3ErrorCode(t, err))
	})

	t.Run("NoSuchUpload", func(t *testing.T) {
		require.NoError(t, createBucket(ctx, client, "upload-bucket"))

		_, err := client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:   aws.String("upload-bucket"),
			Key:      aws.String("k"),
			UploadId: aws.String("nonexistent"),
		})
		require.Equal(t, "NoSuchUpload", s3ErrorCode(t, err))
	})
}

// createBucket is a test helper that ignores an existing bucket.
func createBucket(ctx context.Context, client *s3.Client, bucket string) error {
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})

	var already *types.BucketAlreadyOwnedByYou
	if errors.As(err, &already) {
		return nil
	}

	return err
}

// putObject is a test helper that writes an object with default options.
//
//nolint:unparam // bucket kept explicit for call-site readability.
func putObject(ctx context.Context, client *s3.Client, bucket, key string, content []byte) error {
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(content),
	})

	return err
}
