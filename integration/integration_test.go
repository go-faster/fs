package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/core/service"
	"github.com/go-faster/fs/internal/core/storage/storagefs"
)

func newTestClient(t testing.TB) *minio.Client {
	t.Helper()

	// Create storage with temp directory (real filesystem)
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Create service
	svc := service.New(storage)

	// Create handler and test server (real HTTP server)
	srv := newTestServer(t, svc)

	// Create real minio client
	return NewClient(t, srv)
}

func TestIntegration_CreateBucket(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const bucketName = "test-bucket"

	// Create bucket
	err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)

	// Verify bucket exists
	exists, err := client.BucketExists(ctx, bucketName)
	require.NoError(t, err)
	require.True(t, exists)
}

func TestIntegration_ListBuckets(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	// Create multiple buckets
	bucketNames := []string{"bucket-one", "bucket-two", "bucket-three"}
	for _, name := range bucketNames {
		err := client.MakeBucket(ctx, name, minio.MakeBucketOptions{})
		require.NoError(t, err)
	}

	// List buckets
	buckets, err := client.ListBuckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 3)

	// Verify all buckets are present
	bucketMap := make(map[string]bool)
	for _, b := range buckets {
		bucketMap[b.Name] = true
	}

	for _, name := range bucketNames {
		require.True(t, bucketMap[name], "bucket %s not found", name)
	}
}

func TestIntegration_PutObject(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const (
		bucketName = "test-bucket"
		objectName = "test-object.txt"
	)

	content := []byte("Hello, World!")

	// Create bucket
	err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)

	// Put object (use explicit size)
	_, err = client.PutObject(ctx, bucketName, objectName,
		bytes.NewReader(content), int64(len(content)),
		minio.PutObjectOptions{ContentType: "text/plain"})
	require.NoError(t, err)

	// Verify object exists
	stat, err := client.StatObject(ctx, bucketName, objectName, minio.StatObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, objectName, stat.Key)
	require.Equal(t, int64(len(content)), stat.Size)
}

func TestIntegration_GetObject(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const (
		bucketName = "test-bucket"
		objectName = "test-object.txt"
	)

	content := []byte("Hello, World!")

	// Create bucket
	err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)

	// Put object
	_, err = client.PutObject(ctx, bucketName, objectName,
		bytes.NewReader(content), int64(len(content)),
		minio.PutObjectOptions{ContentType: "text/plain"})
	require.NoError(t, err)

	// Get object
	obj, err := client.GetObject(ctx, bucketName, objectName, minio.GetObjectOptions{})
	require.NoError(t, err)

	defer obj.Close()

	// Read and verify content
	data, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.Equal(t, content, data)
}

func TestIntegration_ListObjects(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const bucketName = "test-bucket"

	// Create bucket
	err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)

	// Put multiple objects
	objectNames := []string{
		"file1.txt",
		"file2.txt",
		"folder/file3.txt",
		"folder/subfolder/file4.txt",
	}

	for _, name := range objectNames {
		_, err = client.PutObject(ctx, bucketName, name,
			bytes.NewReader([]byte("content")), 7,
			minio.PutObjectOptions{})
		require.NoError(t, err)
	}

	// List all objects
	var foundObjects []string

	for obj := range client.ListObjects(ctx, bucketName, minio.ListObjectsOptions{}) {
		require.NoError(t, obj.Err)
		foundObjects = append(foundObjects, obj.Key)
	}

	require.Len(t, foundObjects, len(objectNames))
}

func TestIntegration_ListObjectsWithPrefix(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const bucketName = "test-bucket"

	// Create bucket
	err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)

	// Put objects with different prefixes
	objects := map[string]string{
		"docs/readme.txt":   "readme content",
		"docs/guide.txt":    "guide content",
		"images/logo.png":   "logo data",
		"images/banner.jpg": "banner data",
		"index.html":        "index content",
	}

	for name, content := range objects {
		_, err = client.PutObject(ctx, bucketName, name,
			bytes.NewReader([]byte(content)), int64(len(content)),
			minio.PutObjectOptions{})
		require.NoError(t, err)
	}

	// List objects with "docs/" prefix
	var docsObjects []string

	for obj := range client.ListObjects(ctx, bucketName, minio.ListObjectsOptions{
		Prefix: "docs/",
	}) {
		require.NoError(t, obj.Err)
		docsObjects = append(docsObjects, obj.Key)
	}

	require.Len(t, docsObjects, 2)

	// List objects with "images/" prefix
	var imageObjects []string

	for obj := range client.ListObjects(ctx, bucketName, minio.ListObjectsOptions{
		Prefix: "images/",
	}) {
		require.NoError(t, obj.Err)
		imageObjects = append(imageObjects, obj.Key)
	}

	require.Len(t, imageObjects, 2)
}

func TestIntegration_PutObjectLarge(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const (
		bucketName = "test-bucket"
		objectName = "large-file.bin"
	)

	// Create 1MB content
	contentSize := 1024 * 1024

	content := make([]byte, contentSize)
	for i := range content {
		content[i] = byte(i % 256)
	}

	// Create bucket
	err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)

	// Put large object
	_, err = client.PutObject(ctx, bucketName, objectName,
		bytes.NewReader(content), int64(len(content)),
		minio.PutObjectOptions{})
	require.NoError(t, err)

	// Verify object size
	stat, err := client.StatObject(ctx, bucketName, objectName, minio.StatObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(contentSize), stat.Size)

	// Get and verify content
	obj, err := client.GetObject(ctx, bucketName, objectName, minio.GetObjectOptions{})
	require.NoError(t, err)

	defer obj.Close()

	data, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.Equal(t, content, data)
}

func TestIntegration_OverwriteObject(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const (
		bucketName = "test-bucket"
		objectName = "overwrite-test.txt"
	)

	// Create bucket
	err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)

	// Put object first time
	content1 := []byte("original content")
	_, err = client.PutObject(ctx, bucketName, objectName,
		bytes.NewReader(content1), int64(len(content1)),
		minio.PutObjectOptions{})
	require.NoError(t, err)

	// Put object second time (overwrite)
	content2 := []byte("updated content with more data")
	_, err = client.PutObject(ctx, bucketName, objectName,
		bytes.NewReader(content2), int64(len(content2)),
		minio.PutObjectOptions{})
	require.NoError(t, err)

	// Get and verify updated content
	obj, err := client.GetObject(ctx, bucketName, objectName, minio.GetObjectOptions{})
	require.NoError(t, err)

	defer obj.Close()

	data, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.Equal(t, content2, data)
}

func TestIntegration_NestedObjects(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const bucketName = "test-bucket"

	// Create bucket
	err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)

	// Put nested objects
	nestedObjects := []string{
		"a/b/c/file1.txt",
		"a/b/file2.txt",
		"a/file3.txt",
		"file4.txt",
	}

	for _, name := range nestedObjects {
		_, err = client.PutObject(ctx, bucketName, name,
			bytes.NewReader([]byte("content")), 7,
			minio.PutObjectOptions{})
		require.NoError(t, err)
	}

	// List objects with "a/b/" prefix
	var foundObjects []string

	for obj := range client.ListObjects(ctx, bucketName, minio.ListObjectsOptions{
		Prefix: "a/b/",
	}) {
		require.NoError(t, obj.Err)
		foundObjects = append(foundObjects, obj.Key)
	}

	require.Len(t, foundObjects, 2)
}

func TestIntegration_BucketNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const bucketName = "nonexistent-bucket"

	// Try to check if nonexistent bucket exists
	exists, err := client.BucketExists(ctx, bucketName)
	require.NoError(t, err)
	require.False(t, exists)
}

func TestIntegration_ObjectNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const (
		bucketName = "test-bucket"
		objectName = "nonexistent-object.txt"
	)

	// Create bucket
	err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)

	// Try to get nonexistent object
	_, err = client.StatObject(ctx, bucketName, objectName, minio.StatObjectOptions{})
	require.Error(t, err)
}

func TestIntegration_MultipleBucketsAndObjects(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	// Create multiple buckets with objects
	buckets := map[string][]string{
		"bucket-a": {"file1.txt", "file2.txt"},
		"bucket-b": {"file3.txt", "file4.txt", "file5.txt"},
		"bucket-c": {"file6.txt"},
	}

	for bucketName, objectNames := range buckets {
		// Create bucket
		err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
		require.NoError(t, err)

		// Put objects
		for _, objectName := range objectNames {
			_, err = client.PutObject(ctx, bucketName, objectName,
				bytes.NewReader([]byte("content")), 7,
				minio.PutObjectOptions{})
			require.NoError(t, err)
		}
	}

	// Verify all buckets exist
	allBuckets, err := client.ListBuckets(ctx)
	require.NoError(t, err)
	require.Len(t, allBuckets, len(buckets))

	// Verify objects in each bucket
	for bucketName, expectedObjects := range buckets {
		var foundObjects []string

		for obj := range client.ListObjects(ctx, bucketName, minio.ListObjectsOptions{}) {
			require.NoError(t, obj.Err)
			foundObjects = append(foundObjects, obj.Key)
		}

		require.Len(t, foundObjects, len(expectedObjects))
	}
}

func TestIntegration_ConcurrentOperations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const bucketName = "concurrent-test"

	// Create bucket
	err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)

	// Put multiple objects concurrently
	numObjects := 10
	done := make(chan bool, numObjects)

	for i := 0; i < numObjects; i++ {
		go func(idx int) {
			defer func() { done <- true }()

			objectName := fmt.Sprintf("file%d.txt", idx)
			content := []byte(fmt.Sprintf("content%d", idx))
			_, err := client.PutObject(ctx, bucketName, objectName,
				bytes.NewReader(content), int64(len(content)),
				minio.PutObjectOptions{})
			require.NoError(t, err)
		}(i)
	}

	// Wait for all goroutines to complete
	for i := 0; i < numObjects; i++ {
		<-done
	}

	// Verify all objects were created
	var foundObjects []string

	for obj := range client.ListObjects(ctx, bucketName, minio.ListObjectsOptions{}) {
		require.NoError(t, obj.Err)
		foundObjects = append(foundObjects, obj.Key)
	}

	require.Len(t, foundObjects, numObjects)
}

func TestIntegration_MultipartUpload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const (
		bucketName = "multipart-test"
		objectName = "large-multipart.bin"
	)

	// Create 10MB content (forces multipart upload)
	contentSize := 10 * 1024 * 1024

	content := make([]byte, contentSize)
	for i := range content {
		content[i] = byte(i % 256)
	}

	// Create bucket
	err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)

	// Put large object (will use multipart)
	_, err = client.PutObject(ctx, bucketName, objectName,
		bytes.NewReader(content), int64(len(content)),
		minio.PutObjectOptions{})
	require.NoError(t, err)

	// Verify object size
	stat, err := client.StatObject(ctx, bucketName, objectName, minio.StatObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(contentSize), stat.Size)

	// Get and verify content
	obj, err := client.GetObject(ctx, bucketName, objectName, minio.GetObjectOptions{})
	require.NoError(t, err)

	defer obj.Close()

	data, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.Equal(t, content, data)
}

func TestIntegration_MultipartUploadManual(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newTestClient(t)

	const (
		bucketName = "multipart-manual-test"
		objectName = "manual-multipart.bin"
	)

	// Create bucket
	err := client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)

	// Create three parts of 5MB each
	partSize := 5 * 1024 * 1024
	part1 := make([]byte, partSize)
	part2 := make([]byte, partSize)
	part3 := make([]byte, partSize)

	for i := range part1 {
		part1[i] = byte(i % 256)
	}

	for i := range part2 {
		part2[i] = byte((i + 100) % 256)
	}

	for i := range part3 {
		part3[i] = byte((i + 200) % 256)
	}

	// Get Core client for low-level multipart operations
	core := minio.Core{Client: client}

	// Initiate multipart upload
	uploadID, err := core.NewMultipartUpload(ctx, bucketName, objectName, minio.PutObjectOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, uploadID)

	// Upload parts
	uploadedParts := make([]minio.CompletePart, 3)

	// Part 1
	partInfo, err := core.PutObjectPart(ctx, bucketName, objectName, uploadID, 1,
		bytes.NewReader(part1), int64(len(part1)), minio.PutObjectPartOptions{})
	require.NoError(t, err)

	uploadedParts[0] = minio.CompletePart{
		PartNumber: partInfo.PartNumber,
		ETag:       partInfo.ETag,
	}

	// Part 2
	partInfo, err = core.PutObjectPart(ctx, bucketName, objectName, uploadID, 2,
		bytes.NewReader(part2), int64(len(part2)), minio.PutObjectPartOptions{})
	require.NoError(t, err)

	uploadedParts[1] = minio.CompletePart{
		PartNumber: partInfo.PartNumber,
		ETag:       partInfo.ETag,
	}

	// Part 3
	partInfo, err = core.PutObjectPart(ctx, bucketName, objectName, uploadID, 3,
		bytes.NewReader(part3), int64(len(part3)), minio.PutObjectPartOptions{})
	require.NoError(t, err)

	uploadedParts[2] = minio.CompletePart{
		PartNumber: partInfo.PartNumber,
		ETag:       partInfo.ETag,
	}

	// Complete multipart upload
	_, err = core.CompleteMultipartUpload(ctx, bucketName, objectName, uploadID, uploadedParts, minio.PutObjectOptions{})
	require.NoError(t, err)

	// Verify object size
	stat, err := client.StatObject(ctx, bucketName, objectName, minio.StatObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(partSize*3), stat.Size)

	// Get and verify content
	obj, err := client.GetObject(ctx, bucketName, objectName, minio.GetObjectOptions{})
	require.NoError(t, err)

	defer obj.Close()

	data, err := io.ReadAll(obj)
	require.NoError(t, err)

	// Verify content is correct
	expectedData := append(append(part1, part2...), part3...)
	require.Equal(t, expectedData, data)
}
