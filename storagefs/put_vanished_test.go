package storagefs_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/storagefs"
)

// duringBody runs fn once, part-way through the body, and then finishes it.
//
// A PutObject checks its bucket before reading and renames the staged file into
// place after — so anything that happens to the tree in between happens while
// the request is committed to a path that may no longer exist. That window is
// the whole subject here, and it needs the body to still be arriving.
func duringBody(body string, fn func()) io.Reader {
	var once bool

	return io.MultiReader(
		strings.NewReader(body[:1]),
		readerFunc(func([]byte) (int, error) {
			if !once {
				once = true

				fn()
			}

			return 0, io.EOF
		}),
		strings.NewReader(body[1:]),
	)
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// TestPutIntoADeletedBucketIsNotFound is go-faster/fs#143.
//
// Deleting the bucket while the body is still streaming left the rename with
// nowhere to go, and the ENOENT was wrapped as a generic failure — so a client
// that raced a bucket delete got 500 InternalError. AWS answers 404
// NoSuchBucket, and it is the honest answer: the bucket is gone, which is the
// client's problem to see rather than the server's to apologize for.
func TestPutIntoADeletedBucketIsNotFound(t *testing.T) {
	ctx := context.Background()

	s, err := storagefs.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	body := "the body of an object being written into a bucket that will not exist"

	_, err = s.PutObject(ctx, &fs.PutObjectRequest{
		Reader: duringBody(body, func() {
			require.NoError(t, s.DeleteBucket(ctx, "bucket-a"))
		}),
		Bucket: "bucket-a",
		Key:    "foo/obj",
		Size:   int64(len(body)),
	})

	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

// TestPutSurvivesASiblingBeingDeleted is the other way the rename's directory
// vanishes, and the one that must *not* be an error at all.
//
// A key is a directory here, so two keys under one prefix share a parent —
// and deleting one prunes that parent when it empties. A write to the other,
// staged and waiting to be renamed, then finds its destination gone. Nothing is
// wrong: the bucket is there, the key is unrelated, and the write was going to
// succeed.
func TestPutSurvivesASiblingBeingDeleted(t *testing.T) {
	ctx := context.Background()

	s, err := storagefs.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	// The sibling exists first, so its delete is what empties the shared prefix.
	_, err = s.PutObject(ctx, &fs.PutObjectRequest{
		Reader: strings.NewReader("sibling"),
		Bucket: "bucket-a",
		Key:    "shared/one",
		Size:   7,
	})
	require.NoError(t, err)

	body := "the body of an object whose parent directory is pruned underneath it"

	_, err = s.PutObject(ctx, &fs.PutObjectRequest{
		Reader: duringBody(body, func() {
			require.NoError(t, s.DeleteObject(ctx, "bucket-a", "shared/one"))
		}),
		Bucket: "bucket-a",
		Key:    "shared/two",
		Size:   int64(len(body)),
	})
	require.NoError(t, err, "a write lost to an unrelated delete of a neighboring key")

	got, err := s.GetObject(ctx, "bucket-a", "shared/two")
	require.NoError(t, err)

	defer func() { _ = got.Reader.Close() }()

	read, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	assert.Equal(t, body, string(read))
}

// TestCompleteMultipartIntoADeletedBucket is the same window one operation
// along: the parts are staged, the bucket goes, and the assembled object has
// nowhere to be renamed to. Reported the same way for the same reason.
func TestCompleteMultipartIntoADeletedBucket(t *testing.T) {
	ctx := context.Background()

	s, err := storagefs.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	up, err := s.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: "bucket-a", Key: "foo/obj",
	})
	require.NoError(t, err)

	part, err := s.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket: "bucket-a", Key: "foo/obj", UploadID: up.UploadID, PartNumber: 1,
		Reader: strings.NewReader(strings.Repeat("x", 16)), Size: 16,
	})
	require.NoError(t, err)

	require.NoError(t, s.DeleteBucket(ctx, "bucket-a"))

	_, err = s.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "bucket-a", Key: "foo/obj", UploadID: up.UploadID,
		Parts: []fs.CompletedPart{{PartNumber: 1, ETag: part.ETag}},
	})

	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

// TestCompleteMultipartDoesNotResurrectTheBucket is the sharper half of the
// same bug, and the reason the check is at the top of the completion rather
// than only on the rename.
//
// Assembling the object makes the key's directory, and making that directory
// makes every parent of it — including the bucket. So a completion after a
// delete did not fail at all: it recreated the bucket, reported success, and
// left an object readable inside something the client had removed. Silent, and
// wrong in the direction that loses a delete rather than a write.
func TestCompleteMultipartDoesNotResurrectTheBucket(t *testing.T) {
	ctx := context.Background()

	s, err := storagefs.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	up, err := s.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: "bucket-a", Key: "foo/obj",
	})
	require.NoError(t, err)

	part, err := s.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket: "bucket-a", Key: "foo/obj", UploadID: up.UploadID, PartNumber: 1,
		Reader: strings.NewReader(strings.Repeat("x", 16)), Size: 16,
	})
	require.NoError(t, err)

	require.NoError(t, s.DeleteBucket(ctx, "bucket-a"))

	_, err = s.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "bucket-a", Key: "foo/obj", UploadID: up.UploadID,
		Parts: []fs.CompletedPart{{PartNumber: 1, ETag: part.ETag}},
	})
	require.Error(t, err)

	exists, err := s.BucketExists(ctx, "bucket-a")
	require.NoError(t, err)
	assert.False(t, exists, "completing an upload brought a deleted bucket back")

	_, err = s.GetObject(ctx, "bucket-a", "foo/obj")
	assert.Error(t, err, "an object was left readable in a deleted bucket")
}
