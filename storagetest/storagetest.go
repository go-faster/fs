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
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// Names used by every conformance subtest.
const (
	testBucket = "bucket"
	// missingBucket never exists, for the not-found cases.
	missingBucket = "nonexistent"
	// firstKey sorts first in the listing fixtures.
	firstKey = "a.txt"
	// testObjectKey is the key the single-object cases operate on.
	testObjectKey = "obj.txt"
	testKey       = "big.bin"

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
	"CreateBucket":                          testCreateBucket,
	"CreateBucket/AlreadyExists":            testCreateBucketAlreadyExists,
	"ListBuckets":                           testListBuckets,
	"BucketExists":                          testBucketExists,
	"DeleteBucket":                          testDeleteBucket,
	"DeleteBucket/NotFound":                 testDeleteBucketNotFound,
	"DeleteBucket/NotEmpty":                 testDeleteBucketNotEmpty,
	"DeleteBucket/EmptyAfterNested":         testDeleteBucketEmptyAfterNestedDelete,
	"PutObject":                             testPutObject,
	"PutObject/NestedKey":                   testPutObjectNestedKey,
	"PutObject/Overwrite":                   testPutObjectOverwrite,
	"PutObject/BucketNotFound":              testPutObjectBucketNotFound,
	"PutObject/EncryptionNeverIgnored":      testEncryptionNeverIgnored,
	"PutObject/EncryptionUnknownAlgorithm":  testEncryptionUnknownAlgorithmRefused,
	"Multipart/EncryptionNeverIgnored":      testMultipartEncryptionNeverIgnored,
	"GetObject":                             testGetObject,
	"GetObject/BucketNotFound":              testGetObjectBucketNotFound,
	"GetObject/ObjectNotFound":              testGetObjectObjectNotFound,
	"DeleteObject":                          testDeleteObject,
	"DeleteObject/BucketNotFound":           testDeleteObjectBucketNotFound,
	"DeleteObject/ObjectNotFound":           testDeleteObjectObjectNotFound,
	"ListObjects":                           testListObjects,
	"ListObjects/WithPrefix":                testListObjectsWithPrefix,
	"ListObjects/BucketNotFound":            testListObjectsBucketNotFound,
	"ListObjects/Paging":                    testListObjectsPaging,
	"ListObjects/Delimiter":                 testListObjectsDelimiter,
	"Multipart/Create":                      testMultipartCreate,
	"Multipart/Create/BucketNotFound":       testMultipartCreateBucketNotFound,
	"Multipart/UploadPart":                  testMultipartUploadPart,
	"Multipart/UploadPart/NotFound":         testMultipartUploadPartNotFound,
	"Multipart/Complete":                    testMultipartComplete,
	"Multipart/Complete/ETag":               testMultipartCompleteETag,
	"Multipart/Complete/OutOfOrder":         testMultipartCompleteOutOfOrder,
	"Multipart/Complete/NotFound":           testMultipartCompleteNotFound,
	"Multipart/Abort":                       testMultipartAbort,
	"Multipart/Abort/NotFound":              testMultipartAbortNotFound,
	"Multipart/ListParts":                   testMultipartListParts,
	"Multipart/ListParts/Overwrite":         testMultipartListPartsOverwrite,
	"Multipart/ListParts/NotFound":          testMultipartListPartsNotFound,
	"Multipart/ListParts/WrongKey":          testMultipartListPartsWrongKey,
	"Multipart/ListUploads":                 testMultipartListUploads,
	"Multipart/ListUploads/NotFound":        testMultipartListUploadsBucketNotFound,
	"Multipart/ListUploads/Lifecycle":       testMultipartListUploadsLifecycle,
	"Metadata/PutETag":                      testPutObjectETag,
	"Metadata/RoundTrip":                    testMetadataRoundTrip,
	"Metadata/OverwriteReplaces":            testMetadataOverwriteReplaces,
	"Metadata/Multipart":                    testMetadataMultipart,
	"Tagging/RoundTrip":                     testTaggingRoundTrip,
	"Tagging/PutObjectTags":                 testTaggingOnPut,
	"Tagging/NotFound":                      testTaggingNotFound,
	"Conditional/IfNoneMatch":               testConditionalIfNoneMatch,
	"Conditional/IfMatch":                   testConditionalIfMatch,
	"Conditional/ConcurrentSingleWinner":    testConditionalConcurrentSingleWinner,
	"Conditional/ConcurrentCASSingleWinner": testConditionalConcurrentCASSingleWinner,
	"Conditional/Delete":                    testConditionalDelete,
	"Conditional/CompleteMultipart":         testConditionalCompleteMultipart,
	"Attributes/PartLayout":                 testObjectAttributesPartLayout,
	"Keyspace/OverlappingKeys":              testOverlappingKeys,
	"Ownership/BucketOwner":                 testBucketOwner,
	"CORS/RoundTrip":                        testBucketCORS,
	"Lifecycle/RoundTrip":                   testBucketLifecycle,
	"Versioning/BucketState":                testBucketVersioningState,
	"Settings/PublicAccessAndOwnership":     testBucketSettings,
	"ACL/BucketRoundTrip":                   testACLBucketRoundTrip,
	"ACL/BucketDefaultPrivate":              testACLBucketDefaultPrivate,
	"ACL/BucketNotFound":                    testACLBucketNotFound,
	"ACL/ObjectFromPut":                     testACLObjectFromPut,
	"ACL/ObjectDefaultPrivate":              testACLObjectDefaultPrivate,
	"ACL/ObjectSetRoundTrip":                testACLObjectSetRoundTrip,
	"ACL/ObjectSetNotFound":                 testACLObjectSetNotFound,
	"Owner/ObjectRoundTrip":                 testOwnerObjectRoundTrip,
	"Owner/Unset":                           testOwnerUnset,
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
// listObjects returns every object under prefix, for assertions that want the
// whole bucket. The paged interface is exercised on its own in
// testListObjectsPaging.
func listObjects(t *testing.T, storage fs.Storage, prefix string) []fs.Object {
	t.Helper()

	res, err := storage.ListObjects(t.Context(), &fs.ListObjectsRequest{
		Bucket: testBucket,
		Prefix: prefix,
	})
	require.NoError(t, err)

	return res.Objects
}

func testDeleteBucketEmptyAfterNestedDelete(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	putObject(t, storage, "a/b/c/deep.txt", []byte("content"))
	require.NoError(t, storage.DeleteObject(ctx, testBucket, "a/b/c/deep.txt"))

	objects := listObjects(t, storage, "")
	require.Empty(t, objects)

	// The bucket has no remaining objects, so it must delete cleanly.
	require.NoError(t, storage.DeleteBucket(ctx, testBucket))
}

func testPutObject(t *testing.T, storage fs.Storage) {
	ctx := t.Context()
	content := []byte("hello, world!")

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	putObject(t, storage, "test.txt", content)

	objects := listObjects(t, storage, "")
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

	objects := listObjects(t, storage, "")
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

	objects := listObjects(t, storage, "")
	require.Len(t, objects, 1)
	require.Equal(t, int64(len("second version")), objects[0].Size)
}

func testPutObjectBucketNotFound(t *testing.T, storage fs.Storage) {
	_, err := storage.PutObject(t.Context(), &fs.PutObjectRequest{
		Bucket: missingBucket,
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
	_, err := storage.GetObject(t.Context(), missingBucket, "test.txt")
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

	objects := listObjects(t, storage, "")
	require.Empty(t, objects)

	_, err := storage.GetObject(ctx, testBucket, "test.txt")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
}

func testDeleteObjectBucketNotFound(t *testing.T, storage fs.Storage) {
	err := storage.DeleteObject(t.Context(), missingBucket, "test.txt")
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

	objects := listObjects(t, storage, "")
	require.Empty(t, objects)

	keys := []string{"file1.txt", "file2.txt", "dir/file3.txt"}
	for _, key := range keys {
		putObject(t, storage, key, []byte("content"))
	}

	objects = listObjects(t, storage, "")
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

	result := listObjects(t, storage, "docs/")
	require.Len(t, result, 2)

	result = listObjects(t, storage, "images/")
	require.Len(t, result, 2)

	result = listObjects(t, storage, "videos/")
	require.Empty(t, result)
}

// testListObjectsPaging covers the paging contract every backend has to get
// right: keys come back in order, the limit is respected, truncation reports
// whether anything was actually left behind, and paging through a bucket
// yields every key exactly once.
func testListObjectsPaging(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	keys := []string{firstKey, "b.txt", "c.txt", "d.txt", "e.txt"}
	for _, key := range keys {
		putObject(t, storage, key, []byte(key))
	}

	first, err := storage.ListObjects(ctx, &fs.ListObjectsRequest{Bucket: testBucket, Limit: 2})
	require.NoError(t, err)
	require.Len(t, first.Objects, 2)
	require.Equal(t, firstKey, first.Objects[0].Key)
	require.Equal(t, "b.txt", first.Objects[1].Key)
	require.True(t, first.IsTruncated, "three keys remain")
	require.Equal(t, "b.txt", first.NextStartAfter)

	// Paging through the bucket must see every key once, in order.
	var (
		seen  []string
		after string
	)

	for {
		page, err := storage.ListObjects(ctx, &fs.ListObjectsRequest{
			Bucket: testBucket, StartAfter: after, Limit: 2,
		})
		require.NoError(t, err)

		for _, o := range page.Objects {
			seen = append(seen, o.Key)
		}

		if !page.IsTruncated {
			break
		}

		require.NotEmpty(t, page.NextStartAfter, "a truncated page must say where to resume")
		after = page.NextStartAfter
	}

	require.Equal(t, keys, seen)

	// A page that ends exactly on the last key is not truncated: there is
	// nothing after it, and saying otherwise costs the caller a wasted request
	// and an incorrect IsTruncated in the S3 response.
	exact, err := storage.ListObjects(ctx, &fs.ListObjectsRequest{
		Bucket: testBucket, StartAfter: "c.txt", Limit: 2,
	})
	require.NoError(t, err)
	require.Len(t, exact.Objects, 2)
	require.False(t, exact.IsTruncated)

	// StartAfter is exclusive.
	afterAll, err := storage.ListObjects(ctx, &fs.ListObjectsRequest{
		Bucket: testBucket, StartAfter: "e.txt",
	})
	require.NoError(t, err)
	require.Empty(t, afterAll.Objects)
	require.False(t, afterAll.IsTruncated)

	// No limit means the whole listing.
	all, err := storage.ListObjects(ctx, &fs.ListObjectsRequest{Bucket: testBucket})
	require.NoError(t, err)
	require.Len(t, all.Objects, len(keys))
	require.False(t, all.IsTruncated)
}

// testListObjectsDelimiter covers folding, which the backend now owns: keys
// sharing a prefix collapse into one common prefix, folded entries count
// toward the limit exactly as S3 counts them, and resuming from a common
// prefix skips every key beneath it rather than folding them all over again.
func testListObjectsDelimiter(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	for _, key := range []string{firstKey, "docs/one.txt", "docs/two.txt", "docs/deep/three.txt", "z.txt"} {
		putObject(t, storage, key, []byte(key))
	}

	res, err := storage.ListObjects(ctx, &fs.ListObjectsRequest{Bucket: testBucket, Delimiter: "/"})
	require.NoError(t, err)
	require.Equal(t, []string{"docs/"}, res.CommonPrefixes, "three keys under docs/ fold into one entry")

	var keys []string
	for _, o := range res.Objects {
		keys = append(keys, o.Key)
	}

	require.Equal(t, []string{firstKey, "z.txt"}, keys)

	// The prefix itself is not folded away: listing inside it descends a level.
	inside, err := storage.ListObjects(ctx, &fs.ListObjectsRequest{
		Bucket: testBucket, Prefix: "docs/", Delimiter: "/",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"docs/deep/"}, inside.CommonPrefixes)
	require.Len(t, inside.Objects, 2)

	// A common prefix counts toward the limit like a key does.
	limited, err := storage.ListObjects(ctx, &fs.ListObjectsRequest{
		Bucket: testBucket, Delimiter: "/", Limit: 2,
	})
	require.NoError(t, err)
	require.Len(t, limited.Objects, 1, "a.txt")
	require.Len(t, limited.CommonPrefixes, 1, "docs/")
	require.True(t, limited.IsTruncated)
	require.Equal(t, "docs/", limited.NextStartAfter)

	// Resuming from the common prefix must not re-fold the keys under it.
	rest, err := storage.ListObjects(ctx, &fs.ListObjectsRequest{
		Bucket: testBucket, Delimiter: "/", StartAfter: limited.NextStartAfter,
	})
	require.NoError(t, err)
	require.Empty(t, rest.CommonPrefixes, "docs/ was already returned")
	require.Len(t, rest.Objects, 1)
	require.Equal(t, "z.txt", rest.Objects[0].Key)
	require.False(t, rest.IsTruncated)
}

func testListObjectsBucketNotFound(t *testing.T, storage fs.Storage) {
	_, err := storage.ListObjects(t.Context(), &fs.ListObjectsRequest{Bucket: missingBucket})
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
	_, err := storage.CreateMultipartUpload(t.Context(), &fs.CreateMultipartUploadRequest{Bucket: missingBucket, Key: testKey})
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

	// If-Match against a missing object is a miss, not a failed precondition:
	// S3 answers a conditional write to a key that is not there with 404
	// NoSuchKey.
	_, err := putConditional(t, storage, "obj", []byte("x"), "", "*")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)

	_, err = putConditional(t, storage, "obj", []byte("x"), "", `"deadbeef"`)
	require.ErrorIs(t, err, fs.ErrObjectNotFound)

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

// testBucketVersioningState covers the bucket half of fs.Versioner: the three
// states, and the fact that "never configured" is one of them.
func testBucketVersioningState(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	versioner, ok := storage.(fs.Versioner)
	if !ok {
		t.Skip("backend does not implement fs.Versioner")
	}

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	// A bucket nobody has configured is unset, which the S3 layer reports as
	// no status at all — not as Suspended.
	state, err := versioner.BucketVersioning(ctx, testBucket)
	require.NoError(t, err)
	require.Equal(t, fs.VersioningUnset, state)

	for _, want := range []fs.VersioningState{
		fs.VersioningSuspended, fs.VersioningEnabled, fs.VersioningEnabled, fs.VersioningSuspended,
	} {
		require.NoError(t, versioner.SetBucketVersioning(ctx, testBucket, want))

		got, err := versioner.BucketVersioning(ctx, testBucket)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}

	_, err = versioner.BucketVersioning(ctx, testBucket+"-absent")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

// testBucketCORS covers fs.BucketCORSStore: rules survive a round trip, and a
// bucket without them reports none rather than an error, which is what the S3
// layer turns into NoSuchCORSConfiguration.
func testBucketCORS(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	store, ok := storage.(fs.BucketCORSStore)
	if !ok {
		t.Skip("backend does not store CORS rules")
	}

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	rules, err := store.BucketCORS(ctx, testBucket)
	require.NoError(t, err)
	require.Empty(t, rules)

	want := []fs.CORSRule{{
		AllowedOrigins: []string{"https://example.com", "*.suffix"},
		AllowedMethods: []string{"GET", "PUT"},
		AllowedHeaders: []string{"*"},
		ExposeHeaders:  []string{"x-amz-meta-one"},
		MaxAgeSeconds:  3000,
	}}

	require.NoError(t, store.SetBucketCORS(ctx, testBucket, want))

	got, err := store.BucketCORS(ctx, testBucket)
	require.NoError(t, err)
	require.Equal(t, want, got)

	require.NoError(t, store.DeleteBucketCORS(ctx, testBucket))

	got, err = store.BucketCORS(ctx, testBucket)
	require.NoError(t, err)
	require.Empty(t, got)

	_, err = store.BucketCORS(ctx, testBucket+"-absent")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

// testBucketLifecycle covers fs.BucketLifecycleStore: rules survive a
// round-trip whole, since a rule that comes back missing its expiry is a rule
// that silently stops deleting.
func testBucketLifecycle(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	store, ok := storage.(fs.BucketLifecycleStore)
	if !ok {
		t.Skip("backend does not store lifecycle rules")
	}

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	rules, err := store.BucketLifecycle(ctx, testBucket)
	require.NoError(t, err)
	require.Empty(t, rules)

	want := []fs.LifecycleRule{
		{
			ID:             "expire-logs",
			Status:         fs.LifecycleEnabled,
			Prefix:         "logs/",
			ExpirationDays: 30,
		},
		{
			ID:                                 "clean-uploads",
			Status:                             fs.LifecycleDisabled,
			AbortIncompleteMultipartUploadDays: 7,
		},
		{
			ID:             "expire-on-date",
			Status:         fs.LifecycleEnabled,
			Prefix:         "archive/",
			ExpirationDate: time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	require.NoError(t, store.SetBucketLifecycle(ctx, testBucket, want))

	got, err := store.BucketLifecycle(ctx, testBucket)
	require.NoError(t, err)
	require.Len(t, got, len(want))

	for i := range want {
		require.Equal(t, want[i].ID, got[i].ID)
		require.Equal(t, want[i].Status, got[i].Status)
		require.Equal(t, want[i].Prefix, got[i].Prefix)
		require.Equal(t, want[i].ExpirationDays, got[i].ExpirationDays)
		require.Equal(t, want[i].AbortIncompleteMultipartUploadDays, got[i].AbortIncompleteMultipartUploadDays)
		require.True(t, want[i].ExpirationDate.Equal(got[i].ExpirationDate),
			"expiration date %s != %s", want[i].ExpirationDate, got[i].ExpirationDate)
	}

	require.NoError(t, store.DeleteBucketLifecycle(ctx, testBucket))

	got, err = store.BucketLifecycle(ctx, testBucket)
	require.NoError(t, err)
	require.Empty(t, got)

	_, err = store.BucketLifecycle(ctx, testBucket+"-absent")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

// testBucketSettings covers fs.BucketSettingsStore. Both settings have to
// distinguish "unset" from any value they can hold, because S3 reports an
// absent configuration as its own error rather than as a default.
func testBucketSettings(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	store, ok := storage.(fs.BucketSettingsStore)
	if !ok {
		t.Skip("backend does not store bucket settings")
	}

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	block, err := store.BucketPublicAccessBlock(ctx, testBucket)
	require.NoError(t, err)
	require.Nil(t, block)

	want := &fs.PublicAccessBlock{BlockPublicACLs: true, BlockPublicPolicy: true}
	require.NoError(t, store.SetBucketPublicAccessBlock(ctx, testBucket, want))

	block, err = store.BucketPublicAccessBlock(ctx, testBucket)
	require.NoError(t, err)
	require.Equal(t, want, block)

	require.NoError(t, store.SetBucketPublicAccessBlock(ctx, testBucket, nil))

	block, err = store.BucketPublicAccessBlock(ctx, testBucket)
	require.NoError(t, err)
	require.Nil(t, block)

	ownership, err := store.BucketObjectOwnership(ctx, testBucket)
	require.NoError(t, err)
	require.Empty(t, ownership)

	require.NoError(t, store.SetBucketObjectOwnership(ctx, testBucket, fs.OwnershipBucketOwnerEnforced))

	ownership, err = store.BucketObjectOwnership(ctx, testBucket)
	require.NoError(t, err)
	require.Equal(t, fs.OwnershipBucketOwnerEnforced, ownership)

	require.NoError(t, store.SetBucketObjectOwnership(ctx, testBucket, ""))

	ownership, err = store.BucketObjectOwnership(ctx, testBucket)
	require.NoError(t, err)
	require.Empty(t, ownership)

	_, err = store.BucketPublicAccessBlock(ctx, testBucket+"-absent")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

// testBucketOwner covers fs.BucketOwnership: a bucket remembers who created
// it, and says so consistently.
func testBucketOwner(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	ownership, ok := storage.(fs.BucketOwnership)
	if !ok {
		t.Skip("backend does not implement fs.BucketOwnership")
	}

	owner := fs.Owner{ID: "user-1", DisplayName: "User One"}
	require.NoError(t, ownership.CreateBucketOwned(ctx, testBucket, owner))

	got, err := ownership.BucketOwner(ctx, testBucket)
	require.NoError(t, err)
	require.Equal(t, owner, got)

	// Creating it again is still a conflict at this layer; deciding whether the
	// caller owns it is the S3 layer's job.
	require.ErrorIs(t, ownership.CreateBucketOwned(ctx, testBucket, owner), fs.ErrBucketAlreadyExists)

	// A bucket created without an identity is unowned, not owned by nobody in
	// particular — the distinction the S3 layer relies on to leave older data
	// reachable.
	require.NoError(t, storage.CreateBucket(ctx, testBucket+"-plain"))

	got, err = ownership.BucketOwner(ctx, testBucket+"-plain")
	require.NoError(t, err)
	require.True(t, got.IsZero())

	_, err = ownership.BucketOwner(ctx, testBucket+"-absent")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

// The key shapes a path-shaped keyspace cannot represent: one key that is a
// prefix of another at a delimiter boundary, one that ends in the delimiter,
// and one with an empty component.
const (
	keyFoo       = "foo"
	keyFooBar    = "foo/bar"
	keyFooBarBaz = "foo/bar/baz"
	keyTrailing  = "asdf/"
	keyDoubled   = "a//b"
	keyMultipart = "multi"
)

// testOverlappingKeys covers the flatness of the S3 keyspace: a key that is a
// prefix of another at a delimiter boundary, and a key that ends in one, are
// ordinary names. A backend that maps a key onto a filesystem path has to work
// for this, because the second write finds a file where it wants a directory.
func testOverlappingKeys(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	// Ordered parent-before-child, which is the order that breaks a
	// path-shaped keyspace.
	order := []string{keyFoo, keyFooBar, keyFooBarBaz, keyTrailing, keyDoubled}

	keys := map[string][]byte{
		keyFoo:       []byte("just foo"),
		keyFooBar:    []byte("under foo"),
		keyFooBarBaz: []byte("under foo/bar"),
		keyTrailing:  []byte("trailing delimiter"),
		keyDoubled:   []byte("empty component"),
	}

	for _, key := range order {
		_, err := putConditional(t, storage, key, keys[key], "", "")
		require.NoErrorf(t, err, "put %q", key)
	}

	for key, want := range keys {
		require.Equalf(t, want, readObject(t, storage, key), "read %q", key)
	}

	// Every key is listed exactly once, under the name it was written with.
	page, err := storage.ListObjects(ctx, &fs.ListObjectsRequest{Bucket: testBucket})
	require.NoError(t, err)

	listed := make([]string, 0, len(page.Objects))
	for _, o := range page.Objects {
		listed = append(listed, o.Key)
	}

	require.ElementsMatch(t, order, listed)

	// Deleting the parent leaves the child alone.
	require.NoError(t, storage.DeleteObject(ctx, testBucket, keyFooBar))
	require.Equal(t, keys[keyFooBarBaz], readObject(t, storage, keyFooBarBaz))
	require.Equal(t, keys[keyFoo], readObject(t, storage, keyFoo))
}

// testObjectAttributesPartLayout covers fs.ObjectAttributer: a completed
// multipart object keeps its part boundaries after the upload state is gone,
// and a single PUT reports none.
func testObjectAttributesPartLayout(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	attributer, ok := storage.(fs.ObjectAttributer)
	if !ok {
		t.Skip("backend does not implement fs.ObjectAttributer")
	}

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	// A single PUT has no layout: part 1 is the whole object.
	_, err := putConditional(t, storage, "single", []byte("body"), "", "")
	require.NoError(t, err)

	attrs, err := attributer.ObjectAttributes(ctx, testBucket, "single")
	require.NoError(t, err)
	require.Empty(t, attrs.Parts)
	require.Equal(t, int64(4), attrs.Size)

	offset, length, ok := attrs.PartRange(1)
	require.True(t, ok)
	require.Equal(t, int64(0), offset)
	require.Equal(t, int64(4), length)

	_, _, ok = attrs.PartRange(2)
	require.False(t, ok)

	// A multipart object keeps one entry per completed part, in order.
	upload, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: testBucket,
		Key:    keyMultipart,
	})
	require.NoError(t, err)

	sizes := []int{7, 3}
	completed := make([]fs.CompletedPart, 0, len(sizes))

	for i, size := range sizes {
		part, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket:     testBucket,
			Key:        keyMultipart,
			UploadID:   upload.UploadID,
			PartNumber: i + 1,
			Reader:     bytes.NewReader(bytes.Repeat([]byte("x"), size)),
			Size:       int64(size),
		})
		require.NoError(t, err)

		completed = append(completed, fs.CompletedPart{PartNumber: i + 1, ETag: part.ETag})
	}

	_, err = storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   testBucket,
		Key:      keyMultipart,
		UploadID: upload.UploadID,
		Parts:    completed,
	})
	require.NoError(t, err)

	attrs, err = attributer.ObjectAttributes(ctx, testBucket, keyMultipart)
	require.NoError(t, err)
	require.Len(t, attrs.Parts, 2)
	require.Equal(t, 2, attrs.PartsCount())
	require.Equal(t, int64(10), attrs.Size)
	require.Equal(t, int64(7), attrs.Parts[0].Size)
	require.Equal(t, int64(3), attrs.Parts[1].Size)

	// The ranges tile the object exactly.
	offset, length, ok = attrs.PartRange(2)
	require.True(t, ok)
	require.Equal(t, int64(7), offset)
	require.Equal(t, int64(3), length)

	// A missing object reports the usual error, not an empty layout.
	_, err = attributer.ObjectAttributes(ctx, testBucket, "absent")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
}

// testConditionalDelete covers fs.ConditionalDeleter: a guarded delete removes
// the object only while it still matches, and a guard against a key that is
// already gone is not an error — deletion is idempotent.
func testConditionalDelete(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	deleter, ok := storage.(fs.ConditionalDeleter)
	if !ok {
		t.Skip("backend does not implement fs.ConditionalDeleter")
	}

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	_, err := putConditional(t, storage, "obj", []byte("body"), "", "")
	require.NoError(t, err)

	// Wrong ETag fails and leaves the object in place.
	err = deleter.DeleteObjectIf(ctx, testBucket, "obj", fs.Conditions{IfMatch: `"deadbeef"`})
	require.ErrorIs(t, err, fs.ErrPreconditionFailed)
	require.Equal(t, []byte("body"), readObject(t, storage, "obj"))

	// Wrong size fails too.
	badSize := int64(9999)
	err = deleter.DeleteObjectIf(ctx, testBucket, "obj", fs.Conditions{Size: &badSize})
	require.ErrorIs(t, err, fs.ErrPreconditionFailed)

	// Matching size passes the guard.
	size := int64(len("body"))
	err = deleter.DeleteObjectIf(ctx, testBucket, "obj", fs.Conditions{Size: &size})
	require.NoError(t, err)

	// The key is gone: a guard has nothing left to protect, so it is not a
	// failed precondition but a plain miss the caller reports as success.
	err = deleter.DeleteObjectIf(ctx, testBucket, "obj", fs.Conditions{IfMatch: `"deadbeef"`})
	require.ErrorIs(t, err, fs.ErrObjectNotFound)

	// Recreate and delete with the matching ETag.
	put, err := putConditional(t, storage, "obj", []byte("body"), "", "")
	require.NoError(t, err)
	require.NoError(t, deleter.DeleteObjectIf(ctx, testBucket, "obj", fs.Conditions{IfMatch: put.ETag}))
}

// testConditionalCompleteMultipart covers conditions on multipart completion:
// the same guards a conditional PUT gets, evaluated at the moment the assembled
// object lands.
func testConditionalCompleteMultipart(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	complete := func(key string, cond fs.Conditions) error {
		upload, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
			Bucket: testBucket,
			Key:    key,
		})
		require.NoError(t, err)

		body := []byte("part-body")

		part, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket:     testBucket,
			Key:        key,
			UploadID:   upload.UploadID,
			PartNumber: 1,
			Reader:     bytes.NewReader(body),
			Size:       int64(len(body)),
		})
		require.NoError(t, err)

		_, err = storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
			Bucket:     testBucket,
			Key:        key,
			UploadID:   upload.UploadID,
			Parts:      []fs.CompletedPart{{PartNumber: 1, ETag: part.ETag}},
			Conditions: cond,
		})

		return err
	}

	// If-None-Match: * completes onto an absent key, and refuses the second.
	require.NoError(t, complete("mp", fs.Conditions{IfNoneMatch: "*"}))
	require.ErrorIs(t, complete("mp", fs.Conditions{IfNoneMatch: "*"}), fs.ErrPreconditionFailed)

	// If-Match against a key that is not there is a miss, as it is for PUT.
	require.ErrorIs(t, complete("absent", fs.Conditions{IfMatch: "*"}), fs.ErrObjectNotFound)
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

// testConditionalConcurrentCASSingleWinner is the compare-and-swap counterpart:
// an object exists at ETag E0; N goroutines each read E0 and race to replace it
// with distinct content under If-Match: "E0". Exactly one must win — its write
// moves the object off E0, so every other If-Match: "E0" must then fail. A
// check-then-act backend lets several observe E0 and all overwrite.
func testConditionalConcurrentCASSingleWinner(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	// Seed the object; every racer targets its ETag.
	put, err := putConditional(t, storage, "cas", []byte("v0"), "*", "")
	require.NoError(t, err)

	e0 := put.ETag

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

			// Distinct bodies so each successful write yields a distinct ETag —
			// otherwise identical content keeps the ETag at E0 and multiple
			// idempotent writers legitimately match.
			_, err := putConditional(t, storage, "cas", []byte(fmt.Sprintf("cas-body-%d", i)), "", e0)
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

	require.Equal(t, int64(1), winners.Load(), "exactly one CAS racer must win")
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
		Key:    testObjectKey,
		Reader: strings.NewReader("x"),
		Size:   1,
		ACL:    fs.ACLPublicRead,
	})
	require.NoError(t, err)

	acl, err := storage.ObjectACL(ctx, testBucket, testObjectKey)
	require.NoError(t, err)
	require.Equal(t, fs.ACLPublicRead, acl)

	// A missing object reports ErrObjectNotFound.
	_, err = storage.ObjectACL(ctx, testBucket, "nope.txt")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
}

func testACLObjectDefaultPrivate(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	putObject(t, storage, testObjectKey, []byte("x"))

	acl, err := storage.ObjectACL(ctx, testBucket, testObjectKey)
	require.NoError(t, err)
	require.Equal(t, fs.ACLPrivate, acl)
}

// SetObjectACL changes only the access level: content, metadata and tags must
// survive, since PUT ?acl carries none of them.
func testACLObjectSetRoundTrip(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	_, err := storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket:   testBucket,
		Key:      testObjectKey,
		Reader:   strings.NewReader("payload"),
		Size:     int64(len("payload")),
		Metadata: fs.ObjectMetadata{ContentType: "text/plain"},
		Tags:     []fs.Tag{{Key: "env", Value: "prod"}},
	})
	require.NoError(t, err)

	require.NoError(t, storage.SetObjectACL(ctx, testBucket, testObjectKey, fs.ACLPublicRead))

	acl, err := storage.ObjectACL(ctx, testBucket, testObjectKey)
	require.NoError(t, err)
	require.Equal(t, fs.ACLPublicRead, acl)

	got, err := storage.GetObject(ctx, testBucket, testObjectKey)
	require.NoError(t, err)

	defer func() { require.NoError(t, got.Reader.Close()) }()

	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, "payload", string(body), "PUT ?acl must not touch object content")
	require.Equal(t, "text/plain", got.Metadata.ContentType)

	tags, err := storage.GetObjectTagging(ctx, testBucket, testObjectKey)
	require.NoError(t, err)
	require.Equal(t, []fs.Tag{{Key: "env", Value: "prod"}}, tags)

	// The level is replaced, not merged.
	require.NoError(t, storage.SetObjectACL(ctx, testBucket, testObjectKey, fs.ACLPrivate))

	acl, err = storage.ObjectACL(ctx, testBucket, testObjectKey)
	require.NoError(t, err)
	require.Equal(t, fs.ACLPrivate, acl)
}

// The owner is recorded at write time and is a property of the object, not of
// whoever reads it back — ACL responses depend on that.
func testOwnerObjectRoundTrip(t *testing.T, storage fs.Storage) {
	ctx := t.Context()
	owner := fs.Owner{ID: "user-1", DisplayName: "User One"}

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	_, err := storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: testBucket,
		Key:    testObjectKey,
		Reader: strings.NewReader("x"),
		Size:   1,
		Owner:  owner,
	})
	require.NoError(t, err)

	got, err := storage.ObjectOwner(ctx, testBucket, testObjectKey)
	require.NoError(t, err)
	require.Equal(t, owner, got)

	// Listing reports it too, so a fetch-owner listing does not need a stat per key.
	objects := listObjects(t, storage, "")
	require.Len(t, objects, 1)
	require.Equal(t, owner, objects[0].Owner)

	// Changing the ACL must not disturb the owner.
	require.NoError(t, storage.SetObjectACL(ctx, testBucket, testObjectKey, fs.ACLPublicRead))

	got, err = storage.ObjectOwner(ctx, testBucket, testObjectKey)
	require.NoError(t, err)
	require.Equal(t, owner, got)

	_, err = storage.ObjectOwner(ctx, testBucket, "nope.txt")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)

	_, err = storage.ObjectOwner(ctx, "missing", testObjectKey)
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

// An object written without an owner reports the zero owner rather than failing.
func testOwnerUnset(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	putObject(t, storage, testObjectKey, []byte("x"))

	got, err := storage.ObjectOwner(ctx, testBucket, testObjectKey)
	require.NoError(t, err)
	require.True(t, got.IsZero())
}

func testACLObjectSetNotFound(t *testing.T, storage fs.Storage) {
	ctx := t.Context()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	require.ErrorIs(t, storage.SetObjectACL(ctx, testBucket, "nope.txt", fs.ACLPublicRead), fs.ErrObjectNotFound)
	require.ErrorIs(t, storage.SetObjectACL(ctx, "missing", testObjectKey, fs.ACLPublicRead), fs.ErrBucketNotFound)
}

// encryptedMultipartKey is the object the multipart encryption contract
// writes.
const encryptedMultipartKey = "big"

// testEncryptionNeverIgnored holds every backend to the same rule: a write
// asking for server-side encryption is either encrypted or refused, never
// stored in the clear and reported as done.
//
// It is written as a contract rather than a per-backend test because ignoring
// an unknown field is the natural thing for a backend to do, and the result is
// invisible — an object stored in the clear reads back perfectly, so nothing
// downstream notices that the encryption a client asked for did not happen.
func testEncryptionNeverIgnored(t *testing.T, storage fs.Storage) {
	ctx := t.Context()
	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	body := []byte("should never be stored in the clear")

	_, err := storage.PutObject(ctx, &fs.PutObjectRequest{
		Reader: bytes.NewReader(body), Bucket: testBucket, Key: "obj", Size: int64(len(body)),
		ServerSideEncryption: "AES256",
	})
	if err != nil {
		// Refused: the backend cannot encrypt and says so. Nothing may have
		// been written.
		require.ErrorIs(t, err, fs.ErrUnsupportedOperation)

		_, err = storage.GetObject(ctx, testBucket, "obj")
		require.ErrorIs(t, err, fs.ErrObjectNotFound,
			"a refused encrypted write must not leave an object behind")

		return
	}

	// Accepted: the backend must report the object as encrypted, and must
	// still return the plaintext.
	resp, err := storage.GetObject(ctx, testBucket, "obj")
	require.NoError(t, err)

	defer func() { _ = resp.Reader.Close() }()

	require.NotEmpty(t, resp.ServerSideEncryption,
		"the write was accepted as encrypted but the object does not report an algorithm")

	got, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)
	require.Equal(t, body, got)
}

// testEncryptionUnknownAlgorithmRefused: an algorithm no backend implements
// must never be accepted, whatever the backend does about AES256.
func testEncryptionUnknownAlgorithmRefused(t *testing.T, storage fs.Storage) {
	ctx := t.Context()
	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	_, err := storage.PutObject(ctx, &fs.PutObjectRequest{
		Reader: bytes.NewReader([]byte("x")), Bucket: testBucket, Key: "obj", Size: 1,
		ServerSideEncryption: "aws:kms",
	})
	require.ErrorIs(t, err, fs.ErrUnsupportedOperation)
}

// testMultipartEncryptionNeverIgnored is the same contract as
// testEncryptionNeverIgnored, on the path a large object actually takes.
//
// Multipart is where ignoring the field is easiest: the algorithm is named
// when the upload starts and the bytes are written by later calls that never
// see it, so a backend can drop it without anything looking wrong until
// someone reads the disk.
func testMultipartEncryptionNeverIgnored(t *testing.T, storage fs.Storage) {
	ctx := t.Context()
	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	up, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: testBucket, Key: encryptedMultipartKey, ServerSideEncryption: "AES256",
	})
	if err != nil {
		require.ErrorIs(t, err, fs.ErrUnsupportedOperation)
		return
	}

	body := bytes.Repeat([]byte("multipart payload;"), 500)

	part, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket: testBucket, Key: encryptedMultipartKey, UploadID: up.UploadID,
		PartNumber: 1, Reader: bytes.NewReader(body), Size: int64(len(body)),
	})
	require.NoError(t, err)

	resp, err := storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: testBucket, Key: encryptedMultipartKey, UploadID: up.UploadID,
		Parts: []fs.CompletedPart{{PartNumber: 1, ETag: part.ETag}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.ServerSideEncryption,
		"the upload was accepted as encrypted but the completion does not report an algorithm")

	got, err := storage.GetObject(ctx, testBucket, encryptedMultipartKey)
	require.NoError(t, err)

	defer func() { _ = got.Reader.Close() }()

	require.NotEmpty(t, got.ServerSideEncryption,
		"the completed object does not report the encryption it was written with")

	data, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, body, data)
	require.Equal(t, int64(len(body)), got.Size)
}
