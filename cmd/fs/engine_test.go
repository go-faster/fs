package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func TestSoloMemberKeepsItsLayout(t *testing.T) {
	dir := t.TempDir()

	m, err := soloMember(dir)
	require.NoError(t, err)
	require.NotNil(t, m.Layout())

	first := m.Layout()
	assert.Equal(t, uint64(1), first.Version)
	assert.Len(t, first.Slots, 1)

	again, err := soloMember(dir)
	require.NoError(t, err)
	assert.Equal(t, first, again.Layout(), "a restart keeps the layout it applied, and applies none")
}

func TestBuildEngineSingleNode(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	e, err := buildEngine(root, nil, nil)
	require.NoError(t, err)

	require.NoError(t, e.CreateBucket(ctx, "b"))

	_, err = e.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "k", Reader: bytes.NewReader([]byte("hello")), Size: 5})
	require.NoError(t, err)

	require.FileExists(t, filepath.Join(root, ".engine", "meta.db"))
	require.DirExists(t, filepath.Join(root, ".engine", "blocks"))

	// A restart over the same root finds the object.
	db := e.DB()
	require.NoError(t, db.Close())

	again, err := buildEngine(root, nil, nil)
	require.NoError(t, err)

	// Windows cannot remove the temp dir while the database is open.
	t.Cleanup(func() { _ = again.DB().Close() })

	resp, err := again.GetObject(ctx, "b", "k")
	require.NoError(t, err)

	body, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(body))
}

func TestValidateStorageType(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Storage.Type = StorageTypeEngine
	require.NoError(t, cfg.Validate())

	cfg.Storage.Type = "tape"
	require.ErrorContains(t, cfg.Validate(), "unsupported storage type")
}
