package storagefs

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// TestMigrateLayout_LegacyObjects covers opening a store written by an older
// binary, where a key named a file directly. Without the migration every one of
// those objects reads as missing, so this is the difference between finding the
// data and losing it.
func TestMigrateLayout_LegacyObjects(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Lay out a store the old way: the key is the path.
	legacy := map[string]string{
		"flat.txt":              "flat content",
		"path/to/nested.txt":    "nested content",
		"another/deep/file.bin": "deep content",
	}

	for key, content := range legacy {
		path := filepath.Join(root, "legacy-bucket", filepath.FromSlash(key))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}

	s, err := New(root)
	require.NoError(t, err)

	ctx := t.Context()

	for key, content := range legacy {
		obj, err := s.GetObject(ctx, "legacy-bucket", key)
		require.NoErrorf(t, err, "get %q", key)

		data, err := io.ReadAll(obj.Reader)
		require.NoError(t, err)
		require.NoError(t, obj.Reader.Close())
		require.Equal(t, content, string(data))
	}

	// The objects are listed under the keys they were written with.
	page, err := s.ListObjects(ctx, &fs.ListObjectsRequest{Bucket: "legacy-bucket"})
	require.NoError(t, err)

	keys := make([]string, 0, len(page.Objects))
	for _, o := range page.Objects {
		keys = append(keys, o.Key)
	}

	require.ElementsMatch(t, []string{"flat.txt", "path/to/nested.txt", "another/deep/file.bin"}, keys)

	// Running again is a no-op: nothing is left in the old shape.
	require.NoError(t, s.migrateLayout())

	obj, err := s.GetObject(ctx, "legacy-bucket", "flat.txt")
	require.NoError(t, err)
	require.NoError(t, obj.Reader.Close())
}

// TestMigrateLayout_KeepsNewLayout checks the migration leaves an
// already-converted store alone, including keys whose components look like the
// reserved names.
func TestMigrateLayout_KeepsNewLayout(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	s, err := New(root)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	for _, key := range []string{"foo", "foo/bar", "#obj", "asdf/"} {
		content := "content of " + key
		_, err := s.PutObject(ctx, &fs.PutObjectRequest{
			Bucket: "bucket-a",
			Key:    key,
			Reader: strings.NewReader(content),
			Size:   int64(len(content)),
		})
		require.NoError(t, err)
	}

	require.NoError(t, s.migrateLayout())

	for _, key := range []string{"foo", "foo/bar", "#obj", "asdf/"} {
		obj, err := s.GetObject(ctx, "bucket-a", key)
		require.NoErrorf(t, err, "get %q", key)

		data, err := io.ReadAll(obj.Reader)
		require.NoError(t, err)
		require.NoError(t, obj.Reader.Close())
		require.Equal(t, "content of "+key, string(data))
	}
}
