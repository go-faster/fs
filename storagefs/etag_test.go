package storagefs_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // MD5 is the S3 ETag algorithm under test.
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/storagefs"
)

func TestStorage_ETag(t *testing.T) {
	ctx := context.Background()

	s, err := storagefs.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	put := func(content string) {
		t.Helper()
		require.NoError(t, s.PutObject(ctx, &fs.PutObjectRequest{
			Reader: bytes.NewReader([]byte(content)),
			Bucket: "bucket-a",
			Key:    "obj",
			Size:   int64(len(content)),
		}))
	}

	etag := func() string {
		t.Helper()

		resp, err := s.GetObject(ctx, "bucket-a", "obj")
		require.NoError(t, err)
		require.NoError(t, resp.Reader.Close())

		return resp.ETag
	}

	md5hex := func(content string) string {
		sum := md5.Sum([]byte(content)) //nolint:gosec // S3 ETag algorithm.
		return hex.EncodeToString(sum[:])
	}

	put("hello")

	e1 := etag()
	require.Equal(t, md5hex("hello"), e1, "ETag is the content MD5")
	require.Equal(t, e1, etag(), "ETag is stable across reads (cache hit)")

	// Listing reports the same ETag.
	objs, err := s.ListObjects(ctx, "bucket-a", "")
	require.NoError(t, err)
	require.Len(t, objs, 1)
	require.Equal(t, e1, objs[0].ETag)

	// Overwriting changes the ETag (cache invalidated by size/modtime).
	put("hello world")
	require.Equal(t, md5hex("hello world"), etag())
}
