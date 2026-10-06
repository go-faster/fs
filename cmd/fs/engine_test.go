package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func TestBuildEngineSingleNode(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	e, err := buildEngine(root, nil, nil, false)
	require.NoError(t, err)

	require.NoError(t, e.CreateBucket(ctx, "b"))

	_, err = e.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "k", Reader: bytes.NewReader([]byte("hello")), Size: 5})
	require.NoError(t, err)

	require.FileExists(t, filepath.Join(root, ".engine", "meta.db"))
	require.DirExists(t, filepath.Join(root, ".engine", "blocks"))

	// A restart over the same root finds the object.
	require.NoError(t, e.Close())

	again, err := buildEngine(root, nil, nil, false)
	require.NoError(t, err)

	// Windows cannot remove the temp dir while the database is open.
	t.Cleanup(func() { _ = again.Close() })

	resp, err := again.GetObject(ctx, "b", "k")
	require.NoError(t, err)

	body, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(body))
}

func TestValidateFsync(t *testing.T) {
	cfg := DefaultConfig()
	require.NoError(t, cfg.Validate())

	cfg.Storage.Fsync = "none"
	require.NoError(t, cfg.Validate())

	cfg.Storage.Fsync = "sometimes"
	require.ErrorContains(t, cfg.Validate(), "storage.fsync")
}

func TestRefuseLegacyLayout(t *testing.T) {
	for name, layout := range map[string][]string{
		"staging dir":  {".tmp/"},
		"sidecars":     {".meta/"},
		"bucket dir":   {"photos/"},
		"versions dir": {".versions/"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for _, p := range layout {
				require.NoError(t, os.MkdirAll(filepath.Join(root, p), 0o750))
			}

			_, err := buildEngine(root, nil, nil, true)
			require.ErrorContains(t, err, "filesystem backend")
		})
	}

	// What the engine and the server write themselves is not legacy.
	root := t.TempDir()
	for _, p := range []string{".engine/", ".cluster/", ".lastrun/"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, p), 0o750))
	}

	e, err := buildEngine(root, nil, nil, true)
	require.NoError(t, err)
	require.NoError(t, e.Close())
}
