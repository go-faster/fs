// Package storagetest provides a conformance test suite for fs.Storage
// implementations.
//
// Backend packages (and third-party implementations) call Run from a regular
// test to verify they satisfy the behavioral contract expected by the S3
// handler:
//
//	func TestStorage(t *testing.T) {
//		storagetest.Run(t, func(t testing.TB) fs.Storage {
//			return storagemem.New()
//		})
//	}
package storagetest

import (
	"bytes"
	"crypto/md5" //nolint:gosec // MD5 is required for S3 ETag compatibility.
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// Names used by every conformance subtest.
const (
	testBucket = "bucket"
	testKey    = "big.bin"
)

// Factory returns a fresh, empty fs.Storage for a single (sub)test. Cleanup
// should be registered on t (e.g. via t.TempDir or t.Cleanup).
type Factory func(t testing.TB) fs.Storage

// Run executes the fs.Storage conformance suite against implementations
// produced by factory. Every subtest receives its own storage instance.
func Run(t *testing.T, factory Factory) {
	t.Helper()

	for name, test := range suite {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test(t, factory(t))
		})
	}
}

var suite = map[string]func(t *testing.T, storage fs.Storage){
	"CreateBucket":                    testCreateBucket,
	"CreateBucket/AlreadyExists":      testCreateBucketAlreadyExists,
	"ListBuckets":                     testListBuckets,
	"BucketExists":                    testBucketExists,
	"DeleteBucket":                    testDeleteBucket,
	"DeleteBucket/NotFound":           testDeleteBucketNotFound,
	"DeleteBucket/NotEmpty":           testDeleteBucketNotEmpty,
	"DeleteBucket/EmptyAfterNested":   testDeleteBucketEmptyAfterNestedDelete,
	"PutObject":                       testPutObject,
	"PutObject/NestedKey":             testPutObjectNestedKey,
	"PutObject/Overwrite":             testPutObjectOverwrite,
	"PutObject/BucketNotFound":        testPutObjectBucketNotFound,
	"GetObject":                       testGetObject,
	"GetObject/BucketNotFound":        testGetObjectBucketNotFound,
	"GetObject/ObjectNotFound":        testGetObjectObjectNotFound,
	"DeleteObject":                    testDeleteObject,
	"DeleteObject/BucketNotFound":     testDeleteObjectBucketNotFound,
	"DeleteObject/ObjectNotFound":     testDeleteObjectObjectNotFound,
	"ListObjects":                     testListObjects,
	"ListObjects/WithPrefix":          testListObjectsWithPrefix,
	"ListObjects/BucketNotFound":      testListObjectsBucketNotFound,
	"Multipart/Create":                testMultipartCreate,
	"Multipart/Create/BucketNotFound": testMultipartCreateBucketNotFound,
	"Multipart/UploadPart":            testMultipartUploadPart,
	"Multipart/UploadPart/NotFound":   testMultipartUploadPartNotFound,
	"Multipart/Complete":              testMultipartComplete,
	"Multipart/Complete/ETag":         testMultipartCompleteETag,
	"Multipart/Complete/OutOfOrder":   testMultipartCompleteOutOfOrder,
	"Multipart/Complete/NotFound":     testMultipartCompleteNotFound,
	"Multipart/Abort":                 testMultipartAbort,
	"Multipart/Abort/NotFound":        testMultipartAbortNotFound,
}

func putObject(t *testing.T, storage fs.Storage, key string, content []byte) {
	t.Helper()

	err := storage.PutObject(t.Context(), &fs.PutObjectRequest{
		Bucket: testBucket,
		Key:    key,
		Reader: bytes.NewReader(content),
		Size:   int64(len(content)),
	})
	require.NoError(t, err)
}

func readObject(t *testing.T, storage fs.Storage, key string) []byte {
	t.Helper()

	resp, err := storage.GetObject(t.Context(), testBucket, key)
	require.NoError(t, err)

	defer func() { _ = resp.Reader.Close() }()

	data, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)

	return data
}

func testCreateBucket(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	buckets, err := storage.ListBuckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	require.Equal(t, testBucket, buckets[0].Name)
}

func testCreateBucketAlreadyExists(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	require.ErrorIs(t, storage.CreateBucket(ctx, testBucket), fs.ErrBucketAlreadyExists)
}

func testListBuckets(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	buckets, err := storage.ListBuckets(ctx)
	require.NoError(t, err)
	require.Empty(t, buckets)

	for _, name := range []string{"bucket-a", "bucket-b"} {
		require.NoError(t, storage.CreateBucket(ctx, name))
	}

	buckets, err = storage.ListBuckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 2)

	names := []string{buckets[0].Name, buckets[1].Name}
	require.ElementsMatch(t, []string{"bucket-a", "bucket-b"}, names)
}

func testBucketExists(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	exists, err := storage.BucketExists(ctx, testBucket)
	require.NoError(t, err)
	require.False(t, exists)

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	exists, err = storage.BucketExists(ctx, testBucket)
	require.NoError(t, err)
	require.True(t, exists)
}

func testDeleteBucket(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	require.NoError(t, storage.DeleteBucket(ctx, testBucket))

	buckets, err := storage.ListBuckets(ctx)
	require.NoError(t, err)
	require.Empty(t, buckets)
}

func testDeleteBucketNotFound(t *testing.T, storage fs.Storage) {
	err := storage.DeleteBucket(t.Context(), "nonexistent")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

func testDeleteBucketNotEmpty(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	putObject(t, storage, "test.txt", []byte("content"))

	require.ErrorIs(t, storage.DeleteBucket(ctx, testBucket), fs.ErrBucketNotEmpty)

	// Bucket and object must survive the failed delete.
	data := readObject(t, storage, "test.txt")
	require.Equal(t, []byte("content"), data)
}

// testDeleteBucketEmptyAfterNestedDelete guards the contract that deleting the
// last object under a "directory" prefix leaves the bucket genuinely empty, so
// it can then be removed. Backends that materialize nested keys as directories
// must not leave empty parents behind.
func testDeleteBucketEmptyAfterNestedDelete(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	putObject(t, storage, "a/b/c/deep.txt", []byte("content"))
	require.NoError(t, storage.DeleteObject(ctx, testBucket, "a/b/c/deep.txt"))

	objects, err := storage.ListObjects(ctx, testBucket, "")
	require.NoError(t, err)
	require.Empty(t, objects)

	// The bucket has no remaining objects, so it must delete cleanly.
	require.NoError(t, storage.DeleteBucket(ctx, testBucket))
}

func testPutObject(t *testing.T, storage fs.Storage) {
	ctx := t.Context()
	content := []byte("hello, world!")

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	putObject(t, storage, "test.txt", content)

	objects, err := storage.ListObjects(ctx, testBucket, "")
	require.NoError(t, err)
	require.Len(t, objects, 1)
	require.Equal(t, "test.txt", objects[0].Key)
	require.Equal(t, int64(len(content)), objects[0].Size)
}

func testPutObjectNestedKey(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	const key = "path/to/nested/object.txt"

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	putObject(t, storage, key, []byte("nested"))

	data := readObject(t, storage, key)
	require.Equal(t, []byte("nested"), data)

	objects, err := storage.ListObjects(ctx, testBucket, "")
	require.NoError(t, err)
	require.Len(t, objects, 1)
	require.Equal(t, key, objects[0].Key)
}

func testPutObjectOverwrite(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	putObject(t, storage, "test.txt", []byte("first"))
	putObject(t, storage, "test.txt", []byte("second version"))

	data := readObject(t, storage, "test.txt")
	require.Equal(t, []byte("second version"), data)

	objects, err := storage.ListObjects(ctx, testBucket, "")
	require.NoError(t, err)
	require.Len(t, objects, 1)
	require.Equal(t, int64(len("second version")), objects[0].Size)
}

func testPutObjectBucketNotFound(t *testing.T, storage fs.Storage) {
	err := storage.PutObject(t.Context(), &fs.PutObjectRequest{
		Bucket: "nonexistent",
		Key:    "test.txt",
		Reader: strings.NewReader("content"),
		Size:   7,
	})
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

func testGetObject(t *testing.T, storage fs.Storage) {
	ctx := t.Context()
	content := []byte("hello, world!")

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	putObject(t, storage, "test.txt", content)

	resp, err := storage.GetObject(ctx, testBucket, "test.txt")
	require.NoError(t, err)

	defer func() { _ = resp.Reader.Close() }()

	data, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)
	require.Equal(t, content, data)
	require.Equal(t, int64(len(content)), resp.Size)
	require.NotEmpty(t, resp.ETag)
	require.False(t, resp.LastModified.IsZero())
}

func testGetObjectBucketNotFound(t *testing.T, storage fs.Storage) {
	_, err := storage.GetObject(t.Context(), "nonexistent", "test.txt")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

func testGetObjectObjectNotFound(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	_, err := storage.GetObject(ctx, testBucket, "nonexistent.txt")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
}

func testDeleteObject(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	putObject(t, storage, "test.txt", []byte("content"))

	require.NoError(t, storage.DeleteObject(ctx, testBucket, "test.txt"))

	objects, err := storage.ListObjects(ctx, testBucket, "")
	require.NoError(t, err)
	require.Empty(t, objects)

	_, err = storage.GetObject(ctx, testBucket, "test.txt")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
}

func testDeleteObjectBucketNotFound(t *testing.T, storage fs.Storage) {
	err := storage.DeleteObject(t.Context(), "nonexistent", "test.txt")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

func testDeleteObjectObjectNotFound(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	err := storage.DeleteObject(ctx, testBucket, "nonexistent.txt")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
}

func testListObjects(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	objects, err := storage.ListObjects(ctx, testBucket, "")
	require.NoError(t, err)
	require.Empty(t, objects)

	keys := []string{"file1.txt", "file2.txt", "dir/file3.txt"}
	for _, key := range keys {
		putObject(t, storage, key, []byte("content"))
	}

	objects, err = storage.ListObjects(ctx, testBucket, "")
	require.NoError(t, err)
	require.Len(t, objects, len(keys))

	var listed []string
	for _, obj := range objects {
		listed = append(listed, obj.Key)
	}

	require.ElementsMatch(t, keys, listed)
}

func testListObjectsWithPrefix(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	keys := []string{
		"docs/readme.txt",
		"docs/guide.txt",
		"images/logo.png",
		"images/banner.jpg",
		"index.html",
	}
	for _, key := range keys {
		putObject(t, storage, key, []byte("content"))
	}

	result, err := storage.ListObjects(ctx, testBucket, "docs/")
	require.NoError(t, err)
	require.Len(t, result, 2)

	result, err = storage.ListObjects(ctx, testBucket, "images/")
	require.NoError(t, err)
	require.Len(t, result, 2)

	result, err = storage.ListObjects(ctx, testBucket, "videos/")
	require.NoError(t, err)
	require.Empty(t, result)
}

func testListObjectsBucketNotFound(t *testing.T, storage fs.Storage) {
	_, err := storage.ListObjects(t.Context(), "nonexistent", "")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

func testMultipartCreate(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	upload, err := storage.CreateMultipartUpload(ctx, testBucket, testKey)
	require.NoError(t, err)
	require.NotEmpty(t, upload.UploadID)
	require.Equal(t, testBucket, upload.Bucket)
	require.Equal(t, testKey, upload.Key)
}

func testMultipartCreateBucketNotFound(t *testing.T, storage fs.Storage) {
	_, err := storage.CreateMultipartUpload(t.Context(), "nonexistent", testKey)
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

func uploadPart(t *testing.T, storage fs.Storage, uploadID string, partNumber int, content []byte) *fs.Part {
	t.Helper()

	part, err := storage.UploadPart(t.Context(), &fs.UploadPartRequest{
		Bucket:     testBucket,
		Key:        testKey,
		UploadID:   uploadID,
		PartNumber: partNumber,
		Reader:     bytes.NewReader(content),
		Size:       int64(len(content)),
	})
	require.NoError(t, err)
	require.Equal(t, partNumber, part.PartNumber)
	require.NotEmpty(t, part.ETag)

	return part
}

func testMultipartUploadPart(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	upload, err := storage.CreateMultipartUpload(ctx, testBucket, testKey)
	require.NoError(t, err)

	part := uploadPart(t, storage, upload.UploadID, 1, []byte("part data"))
	require.Equal(t, int64(len("part data")), part.Size)
}

func testMultipartUploadPartNotFound(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	_, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     testBucket,
		Key:        testKey,
		UploadID:   "nonexistent-upload",
		PartNumber: 1,
		Reader:     strings.NewReader("data"),
		Size:       4,
	})
	require.ErrorIs(t, err, fs.ErrUploadNotFound)
}

func testMultipartComplete(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	upload, err := storage.CreateMultipartUpload(ctx, testBucket, testKey)
	require.NoError(t, err)

	part1 := uploadPart(t, storage, upload.UploadID, 1, []byte("hello, "))
	part2 := uploadPart(t, storage, upload.UploadID, 2, []byte("world!"))

	resp, err := storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   testBucket,
		Key:      testKey,
		UploadID: upload.UploadID,
		Parts: []fs.CompletedPart{
			{PartNumber: 1, ETag: part1.ETag},
			{PartNumber: 2, ETag: part2.ETag},
		},
	})
	require.NoError(t, err)
	require.Equal(t, testBucket, resp.Bucket)
	require.Equal(t, testKey, resp.Key)
	require.NotEmpty(t, resp.ETag)

	data := readObject(t, storage, testKey)
	require.Equal(t, []byte("hello, world!"), data)
}

func testMultipartCompleteETag(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	upload, err := storage.CreateMultipartUpload(ctx, testBucket, testKey)
	require.NoError(t, err)

	part1Data := []byte("hello, ")
	part2Data := []byte("world!")
	part1 := uploadPart(t, storage, upload.UploadID, 1, part1Data)
	part2 := uploadPart(t, storage, upload.UploadID, 2, part2Data)

	resp, err := storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   testBucket,
		Key:      testKey,
		UploadID: upload.UploadID,
		Parts: []fs.CompletedPart{
			{PartNumber: 1, ETag: part1.ETag},
			{PartNumber: 2, ETag: part2.ETag},
		},
	})
	require.NoError(t, err)

	expected := expectedMultipartETag(part1Data, part2Data)
	require.Equal(t, expected, resp.ETag)

	object, err := storage.GetObject(ctx, testBucket, testKey)
	require.NoError(t, err)

	defer func() { _ = object.Reader.Close() }()

	require.Equal(t, expected, object.ETag)
}

func expectedMultipartETag(parts ...[]byte) string {
	hash := md5.New() //nolint:gosec // MD5 is required for S3 ETag compatibility.

	for _, part := range parts {
		partHash := md5.Sum(part) //nolint:gosec // MD5 is required for S3 ETag compatibility.
		_, _ = hash.Write(partHash[:])
	}

	return fmt.Sprintf("%x-%d", hash.Sum(nil), len(parts))
}

func testMultipartCompleteOutOfOrder(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	upload, err := storage.CreateMultipartUpload(ctx, testBucket, testKey)
	require.NoError(t, err)

	// Upload parts in reverse order; completion must assemble by part number.
	part2 := uploadPart(t, storage, upload.UploadID, 2, []byte("world!"))
	part1 := uploadPart(t, storage, upload.UploadID, 1, []byte("hello, "))

	_, err = storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   testBucket,
		Key:      testKey,
		UploadID: upload.UploadID,
		Parts: []fs.CompletedPart{
			{PartNumber: 1, ETag: part1.ETag},
			{PartNumber: 2, ETag: part2.ETag},
		},
	})
	require.NoError(t, err)

	data := readObject(t, storage, testKey)
	require.Equal(t, []byte("hello, world!"), data)
}

func testMultipartCompleteNotFound(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	_, err := storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   testBucket,
		Key:      testKey,
		UploadID: "nonexistent-upload",
		Parts:    []fs.CompletedPart{{PartNumber: 1, ETag: "etag"}},
	})
	require.ErrorIs(t, err, fs.ErrUploadNotFound)
}

func testMultipartAbort(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	upload, err := storage.CreateMultipartUpload(ctx, testBucket, testKey)
	require.NoError(t, err)

	uploadPart(t, storage, upload.UploadID, 1, []byte("data"))

	require.NoError(t, storage.AbortMultipartUpload(ctx, testBucket, testKey, upload.UploadID))

	// The upload is gone and no object was created.
	_, err = storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     testBucket,
		Key:        testKey,
		UploadID:   upload.UploadID,
		PartNumber: 2,
		Reader:     strings.NewReader("data"),
		Size:       4,
	})
	require.ErrorIs(t, err, fs.ErrUploadNotFound)

	_, err = storage.GetObject(ctx, testBucket, testKey)
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
}

func testMultipartAbortNotFound(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	err := storage.AbortMultipartUpload(ctx, testBucket, testKey, "nonexistent-upload")
	require.ErrorIs(t, err, fs.ErrUploadNotFound)
}
