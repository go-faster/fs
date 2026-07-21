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
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// Names used by every conformance subtest.
const (
	testBucket = "bucket"
	testKey    = "big.bin"

	// Names and values reused by the metadata/tagging subtests.
	metaKey = "meta.txt"
	tagEnv  = "env"
	tagProd = "prod"
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
	"CreateBucket":                       testCreateBucket,
	"CreateBucket/AlreadyExists":         testCreateBucketAlreadyExists,
	"ListBuckets":                        testListBuckets,
	"BucketExists":                       testBucketExists,
	"DeleteBucket":                       testDeleteBucket,
	"DeleteBucket/NotFound":              testDeleteBucketNotFound,
	"DeleteBucket/NotEmpty":              testDeleteBucketNotEmpty,
	"DeleteBucket/EmptyAfterNested":      testDeleteBucketEmptyAfterNestedDelete,
	"PutObject":                          testPutObject,
	"PutObject/NestedKey":                testPutObjectNestedKey,
	"PutObject/Overwrite":                testPutObjectOverwrite,
	"PutObject/BucketNotFound":           testPutObjectBucketNotFound,
	"GetObject":                          testGetObject,
	"GetObject/BucketNotFound":           testGetObjectBucketNotFound,
	"GetObject/ObjectNotFound":           testGetObjectObjectNotFound,
	"DeleteObject":                       testDeleteObject,
	"DeleteObject/BucketNotFound":        testDeleteObjectBucketNotFound,
	"DeleteObject/ObjectNotFound":        testDeleteObjectObjectNotFound,
	"ListObjects":                        testListObjects,
	"ListObjects/WithPrefix":             testListObjectsWithPrefix,
	"ListObjects/BucketNotFound":         testListObjectsBucketNotFound,
	"Multipart/Create":                   testMultipartCreate,
	"Multipart/Create/BucketNotFound":    testMultipartCreateBucketNotFound,
	"Multipart/UploadPart":               testMultipartUploadPart,
	"Multipart/UploadPart/NotFound":      testMultipartUploadPartNotFound,
	"Multipart/Complete":                 testMultipartComplete,
	"Multipart/Complete/ETag":            testMultipartCompleteETag,
	"Multipart/Complete/OutOfOrder":      testMultipartCompleteOutOfOrder,
	"Multipart/Complete/NotFound":        testMultipartCompleteNotFound,
	"Multipart/Abort":                    testMultipartAbort,
	"Multipart/Abort/NotFound":           testMultipartAbortNotFound,
	"Multipart/ListParts":                testMultipartListParts,
	"Multipart/ListParts/Overwrite":      testMultipartListPartsOverwrite,
	"Multipart/ListParts/NotFound":       testMultipartListPartsNotFound,
	"Multipart/ListParts/WrongKey":       testMultipartListPartsWrongKey,
	"Multipart/ListUploads":              testMultipartListUploads,
	"Multipart/ListUploads/NotFound":     testMultipartListUploadsBucketNotFound,
	"Multipart/ListUploads/Lifecycle":    testMultipartListUploadsLifecycle,
	"Metadata/PutETag":                   testPutObjectETag,
	"Metadata/RoundTrip":                 testMetadataRoundTrip,
	"Metadata/OverwriteReplaces":         testMetadataOverwriteReplaces,
	"Metadata/Multipart":                 testMetadataMultipart,
	"Tagging/RoundTrip":                  testTaggingRoundTrip,
	"Tagging/PutObjectTags":              testTaggingOnPut,
	"Tagging/NotFound":                   testTaggingNotFound,
	"Conditional/IfNoneMatch":            testConditionalIfNoneMatch,
	"Conditional/IfMatch":                testConditionalIfMatch,
	"Conditional/ConcurrentSingleWinner": testConditionalConcurrentSingleWinner,
	"ACL/BucketRoundTrip":                testACLBucketRoundTrip,
	"ACL/BucketDefaultPrivate":           testACLBucketDefaultPrivate,
	"ACL/BucketNotFound":                 testACLBucketNotFound,
	"ACL/ObjectFromPut":                  testACLObjectFromPut,
	"ACL/ObjectDefaultPrivate":           testACLObjectDefaultPrivate,
}

func putObject(t *testing.T, storage fs.Storage, key string, content []byte) {
	t.Helper()

	_, err := storage.PutObject(t.Context(), &fs.PutObjectRequest{
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
	_, err := storage.PutObject(t.Context(), &fs.PutObjectRequest{
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

	upload, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: testKey})
	require.NoError(t, err)
	require.NotEmpty(t, upload.UploadID)
	require.Equal(t, testBucket, upload.Bucket)
	require.Equal(t, testKey, upload.Key)
}

func testMultipartCreateBucketNotFound(t *testing.T, storage fs.Storage) {
	_, err := storage.CreateMultipartUpload(t.Context(), &fs.CreateMultipartUploadRequest{Bucket: "nonexistent", Key: testKey})
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

	upload, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: testKey})
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

	upload, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: testKey})
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

	upload, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: testKey})
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

	upload, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: testKey})
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

	upload, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: testKey})
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

func testMultipartListParts(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	upload, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: testKey})
	require.NoError(t, err)

	// Upload out of order; the listing must come back sorted by part number.
	part3 := uploadPart(t, storage, upload.UploadID, 3, []byte("ccc"))
	part1 := uploadPart(t, storage, upload.UploadID, 1, []byte("a"))
	part2 := uploadPart(t, storage, upload.UploadID, 2, []byte("bb"))

	parts, err := storage.ListParts(ctx, testBucket, testKey, upload.UploadID)
	require.NoError(t, err)
	require.Len(t, parts, 3)

	for i, expected := range []*fs.Part{part1, part2, part3} {
		require.Equal(t, expected.PartNumber, parts[i].PartNumber)
		require.Equal(t, expected.ETag, parts[i].ETag)
		require.Equal(t, expected.Size, parts[i].Size)
		require.False(t, parts[i].LastModified.IsZero())
	}
}

func testMultipartListPartsOverwrite(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	upload, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: testKey})
	require.NoError(t, err)

	uploadPart(t, storage, upload.UploadID, 1, []byte("first attempt"))
	replaced := uploadPart(t, storage, upload.UploadID, 1, []byte("second"))

	parts, err := storage.ListParts(ctx, testBucket, testKey, upload.UploadID)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, replaced.ETag, parts[0].ETag)
	require.Equal(t, int64(len("second")), parts[0].Size)
}

func testMultipartListPartsNotFound(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	_, err := storage.ListParts(ctx, testBucket, testKey, "nonexistent-upload")
	require.ErrorIs(t, err, fs.ErrUploadNotFound)
}

func testMultipartListPartsWrongKey(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	upload, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: testKey})
	require.NoError(t, err)

	// The upload ID is scoped to (bucket, key): a different key must not see it.
	_, err = storage.ListParts(ctx, testBucket, "other.bin", upload.UploadID)
	require.ErrorIs(t, err, fs.ErrUploadNotFound)
}

func testMultipartListUploads(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	uploads, err := storage.ListMultipartUploads(ctx, testBucket)
	require.NoError(t, err)
	require.Empty(t, uploads)

	// Two uploads on different keys plus a second upload on the same key.
	uploadB, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: "b.bin"})
	require.NoError(t, err)
	uploadA1, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: "a.bin"})
	require.NoError(t, err)
	uploadA2, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: "a.bin"})
	require.NoError(t, err)

	uploads, err = storage.ListMultipartUploads(ctx, testBucket)
	require.NoError(t, err)
	require.Len(t, uploads, 3)

	// Sorted by key, then upload ID for equal keys.
	require.Equal(t, "a.bin", uploads[0].Key)
	require.Equal(t, "a.bin", uploads[1].Key)
	require.Equal(t, "b.bin", uploads[2].Key)
	require.Equal(t, uploadB.UploadID, uploads[2].UploadID)
	require.LessOrEqual(t, uploads[0].UploadID, uploads[1].UploadID)
	require.ElementsMatch(t,
		[]string{uploadA1.UploadID, uploadA2.UploadID},
		[]string{uploads[0].UploadID, uploads[1].UploadID})

	for _, u := range uploads {
		require.Equal(t, testBucket, u.Bucket)
		require.False(t, u.Initiated.IsZero())
	}
}

func testMultipartListUploadsBucketNotFound(t *testing.T, storage fs.Storage) {
	_, err := storage.ListMultipartUploads(t.Context(), "nonexistent")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

// testMultipartListUploadsLifecycle checks that completed and aborted uploads
// disappear from the listing.
func testMultipartListUploadsLifecycle(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	completed, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: "done.bin"})
	require.NoError(t, err)
	aborted, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: testBucket, Key: "gone.bin"})
	require.NoError(t, err)

	part := uploadPart(t, storage, completed.UploadID, 1, []byte("data"))

	_, err = storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   testBucket,
		Key:      "done.bin",
		UploadID: completed.UploadID,
		Parts:    []fs.CompletedPart{{PartNumber: 1, ETag: part.ETag}},
	})
	require.NoError(t, err)

	require.NoError(t, storage.AbortMultipartUpload(ctx, testBucket, "gone.bin", aborted.UploadID))

	uploads, err := storage.ListMultipartUploads(ctx, testBucket)
	require.NoError(t, err)
	require.Empty(t, uploads)

	_, err = storage.ListParts(ctx, testBucket, "done.bin", completed.UploadID)
	require.ErrorIs(t, err, fs.ErrUploadNotFound)
}

// testPutObjectETag guards that PutObject reports the MD5 ETag of the content
// and that reads agree with it.
func testPutObjectETag(t *testing.T, storage fs.Storage) {
	ctx := t.Context()
	content := []byte("hello, world!")

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	resp, err := storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: testBucket,
		Key:    "test.txt",
		Reader: bytes.NewReader(content),
		Size:   int64(len(content)),
	})
	require.NoError(t, err)

	expected := fmt.Sprintf("%x", md5.Sum(content)) //nolint:gosec // MD5 is required for S3 ETag compatibility.
	require.Equal(t, expected, resp.ETag)

	obj, err := storage.GetObject(ctx, testBucket, "test.txt")
	require.NoError(t, err)

	defer func() { _ = obj.Reader.Close() }()

	require.Equal(t, expected, obj.ETag)
}

func testMetadata() fs.ObjectMetadata {
	return fs.ObjectMetadata{
		ContentType:        "text/plain; charset=utf-8",
		CacheControl:       "max-age=3600",
		ContentDisposition: `attachment; filename="report.txt"`,
		ContentEncoding:    "gzip",
		UserMetadata:       map[string]string{"color": "blue", "owner": "storagetest"},
	}
}

// testMetadataRoundTrip guards that all metadata fields survive a put/get cycle.
func testMetadataRoundTrip(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	_, err := storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket:   testBucket,
		Key:      metaKey,
		Reader:   strings.NewReader("content"),
		Size:     7,
		Metadata: testMetadata(),
	})
	require.NoError(t, err)

	obj, err := storage.GetObject(ctx, testBucket, metaKey)
	require.NoError(t, err)

	defer func() { _ = obj.Reader.Close() }()

	require.Equal(t, testMetadata(), obj.Metadata)
}

// testMetadataOverwriteReplaces guards that overwriting an object replaces its
// metadata and tags entirely instead of merging.
func testMetadataOverwriteReplaces(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	_, err := storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket:   testBucket,
		Key:      metaKey,
		Reader:   strings.NewReader("v1"),
		Size:     2,
		Metadata: testMetadata(),
		Tags:     []fs.Tag{{Key: tagEnv, Value: "dev"}},
	})
	require.NoError(t, err)

	_, err = storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket:   testBucket,
		Key:      metaKey,
		Reader:   strings.NewReader("v2"),
		Size:     2,
		Metadata: fs.ObjectMetadata{ContentType: "application/json"},
	})
	require.NoError(t, err)

	obj, err := storage.GetObject(ctx, testBucket, metaKey)
	require.NoError(t, err)

	defer func() { _ = obj.Reader.Close() }()

	require.Equal(t, fs.ObjectMetadata{ContentType: "application/json"}, obj.Metadata)

	tags, err := storage.GetObjectTagging(ctx, testBucket, metaKey)
	require.NoError(t, err)
	require.Empty(t, tags)
}

// testMetadataMultipart guards that metadata and tags captured at initiation
// are applied to the completed object.
func testMetadataMultipart(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	upload, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket:   testBucket,
		Key:      testKey,
		Metadata: testMetadata(),
		Tags:     []fs.Tag{{Key: tagEnv, Value: tagProd}},
	})
	require.NoError(t, err)

	part := uploadPart(t, storage, upload.UploadID, 1, []byte("data"))

	_, err = storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   testBucket,
		Key:      testKey,
		UploadID: upload.UploadID,
		Parts:    []fs.CompletedPart{{PartNumber: 1, ETag: part.ETag}},
	})
	require.NoError(t, err)

	obj, err := storage.GetObject(ctx, testBucket, testKey)
	require.NoError(t, err)

	defer func() { _ = obj.Reader.Close() }()

	require.Equal(t, testMetadata(), obj.Metadata)

	tags, err := storage.GetObjectTagging(ctx, testBucket, testKey)
	require.NoError(t, err)
	require.Equal(t, []fs.Tag{{Key: tagEnv, Value: tagProd}}, tags)
}

// testTaggingRoundTrip guards the tagging CRUD cycle.
func testTaggingRoundTrip(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	putObject(t, storage, "tagged.txt", []byte("content"))

	// Untagged objects report an empty set.
	tags, err := storage.GetObjectTagging(ctx, testBucket, "tagged.txt")
	require.NoError(t, err)
	require.Empty(t, tags)

	want := []fs.Tag{{Key: tagEnv, Value: tagProd}, {Key: "team", Value: "storage"}}
	require.NoError(t, storage.PutObjectTagging(ctx, testBucket, "tagged.txt", want))

	tags, err = storage.GetObjectTagging(ctx, testBucket, "tagged.txt")
	require.NoError(t, err)
	require.Equal(t, want, tags)

	// Replacing the set does not merge.
	want = []fs.Tag{{Key: "only", Value: "one"}}
	require.NoError(t, storage.PutObjectTagging(ctx, testBucket, "tagged.txt", want))

	tags, err = storage.GetObjectTagging(ctx, testBucket, "tagged.txt")
	require.NoError(t, err)
	require.Equal(t, want, tags)

	require.NoError(t, storage.DeleteObjectTagging(ctx, testBucket, "tagged.txt"))

	tags, err = storage.GetObjectTagging(ctx, testBucket, "tagged.txt")
	require.NoError(t, err)
	require.Empty(t, tags)

	// Tagging must not have altered the content or its readability.
	require.Equal(t, []byte("content"), readObject(t, storage, "tagged.txt"))
}

// testTaggingOnPut guards tags supplied at PutObject time.
func testTaggingOnPut(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	want := []fs.Tag{{Key: "k1", Value: "v1"}, {Key: "k2", Value: "v2"}}

	_, err := storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: testBucket,
		Key:    "tagged.txt",
		Reader: strings.NewReader("content"),
		Size:   7,
		Tags:   want,
	})
	require.NoError(t, err)

	tags, err := storage.GetObjectTagging(ctx, testBucket, "tagged.txt")
	require.NoError(t, err)
	require.Equal(t, want, tags)
}

// testTaggingNotFound guards tagging error mapping for missing buckets/objects.
func testTaggingNotFound(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	_, err := storage.GetObjectTagging(ctx, "nonexistent", "key")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	_, err = storage.GetObjectTagging(ctx, testBucket, "missing.txt")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)

	err = storage.PutObjectTagging(ctx, testBucket, "missing.txt", []fs.Tag{{Key: "k", Value: "v"}})
	require.ErrorIs(t, err, fs.ErrObjectNotFound)

	err = storage.DeleteObjectTagging(ctx, testBucket, "missing.txt")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
}

// putConditional writes an object with an If-None-Match / If-Match condition and
// returns the storage result (ETag or error).
func putConditional(t *testing.T, storage fs.Storage, key string, content []byte, ifNoneMatch, ifMatch string) (*fs.PutObjectResponse, error) {
	t.Helper()

	return storage.PutObject(t.Context(), &fs.PutObjectRequest{
		Bucket:      testBucket,
		Key:         key,
		Reader:      bytes.NewReader(content),
		Size:        int64(len(content)),
		IfNoneMatch: ifNoneMatch,
		IfMatch:     ifMatch,
	})
}

// testConditionalIfNoneMatch covers If-None-Match: * (put-if-absent).
func testConditionalIfNoneMatch(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	// First write to an absent key succeeds.
	_, err := putConditional(t, storage, "obj", []byte("first"), "*", "")
	require.NoError(t, err)

	// Second write must fail — the key now exists — and must not overwrite.
	_, err = putConditional(t, storage, "obj", []byte("second"), "*", "")
	require.ErrorIs(t, err, fs.ErrPreconditionFailed)

	require.Equal(t, []byte("first"), readObject(t, storage, "obj"))
}

// testConditionalIfMatch covers If-Match: * and If-Match: "<etag>".
func testConditionalIfMatch(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	// If-Match: * against a missing object fails.
	_, err := putConditional(t, storage, "obj", []byte("x"), "", "*")
	require.ErrorIs(t, err, fs.ErrPreconditionFailed)

	put, err := putConditional(t, storage, "obj", []byte("v1"), "*", "")
	require.NoError(t, err)

	// Wrong ETag fails and leaves the object unchanged.
	_, err = putConditional(t, storage, "obj", []byte("v2"), "", `"deadbeef"`)
	require.ErrorIs(t, err, fs.ErrPreconditionFailed)
	require.Equal(t, []byte("v1"), readObject(t, storage, "obj"))

	// Correct ETag succeeds.
	_, err = putConditional(t, storage, "obj", []byte("v2"), "", put.ETag)
	require.NoError(t, err)
	require.Equal(t, []byte("v2"), readObject(t, storage, "obj"))
}

// testConditionalConcurrentSingleWinner is the race regression: N goroutines
// race to create the same key with If-None-Match: *, and exactly one must win.
// A check-then-act backend lets several observe "absent" and all succeed.
func testConditionalConcurrentSingleWinner(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	const racers = 24

	var (
		wg        sync.WaitGroup
		winners   atomic.Int64
		conflicts atomic.Int64
		start     = make(chan struct{})
	)

	for i := range racers {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			<-start

			_, err := putConditional(t, storage, "race", []byte(fmt.Sprintf("body-%d", i)), "*", "")
			switch {
			case err == nil:
				winners.Add(1)
			case errors.Is(err, fs.ErrPreconditionFailed):
				conflicts.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}

	close(start)
	wg.Wait()

	require.Equal(t, int64(1), winners.Load(), "exactly one racer must win")
	require.Equal(t, int64(racers-1), conflicts.Load(), "all losers must see ErrPreconditionFailed")
}

func testACLBucketRoundTrip(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	require.NoError(t, storage.SetBucketACL(ctx, testBucket, fs.ACLPublicRead))

	acl, err := storage.BucketACL(ctx, testBucket)
	require.NoError(t, err)
	require.Equal(t, fs.ACLPublicRead, acl)

	require.NoError(t, storage.SetBucketACL(ctx, testBucket, fs.ACLPublicReadWrite))

	acl, err = storage.BucketACL(ctx, testBucket)
	require.NoError(t, err)
	require.Equal(t, fs.ACLPublicReadWrite, acl)
}

func testACLBucketDefaultPrivate(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	acl, err := storage.BucketACL(ctx, testBucket)
	require.NoError(t, err)
	require.Equal(t, fs.ACLPrivate, acl)
}

func testACLBucketNotFound(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	_, err := storage.BucketACL(ctx, "missing")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)

	err = storage.SetBucketACL(ctx, "missing", fs.ACLPublicRead)
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

func testACLObjectFromPut(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	_, err := storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: testBucket,
		Key:    "obj.txt",
		Reader: strings.NewReader("x"),
		Size:   1,
		ACL:    fs.ACLPublicRead,
	})
	require.NoError(t, err)

	acl, err := storage.ObjectACL(ctx, testBucket, "obj.txt")
	require.NoError(t, err)
	require.Equal(t, fs.ACLPublicRead, acl)

	// A missing object reports ErrObjectNotFound.
	_, err = storage.ObjectACL(ctx, testBucket, "nope.txt")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
}

func testACLObjectDefaultPrivate(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	putObject(t, storage, "obj.txt", []byte("x"))

	acl, err := storage.ObjectACL(ctx, testBucket, "obj.txt")
	require.NoError(t, err)
	require.Equal(t, fs.ACLPrivate, acl)
}
