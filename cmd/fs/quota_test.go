package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/adminapi"
	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/internal/adminhandler"
)

func TestQuotaCommand(t *testing.T) {
	const token = "s3cr3t"

	ctx := context.Background()

	e, err := engine.Open(t.TempDir(), engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })

	require.NoError(t, e.CreateBucketOwned(ctx, "b", fs.Owner{ID: "alice"}))

	_, err = e.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "k", Reader: strings.NewReader("hello"), Size: 5})
	require.NoError(t, err)

	s, err := adminapi.NewServer(adminhandler.NewAdminAPI(adminhandler.Options{Engine: e}))
	require.NoError(t, err)

	srv := httptest.NewServer(bearerAuth(token, s))
	t.Cleanup(srv.Close)

	run := func(args ...string) string {
		t.Helper()

		var out bytes.Buffer

		cmd := Quota()
		cmd.SetOut(&out)
		cmd.SetArgs(append(args, "--admin-addr", srv.URL, "--token", token))
		require.NoError(t, cmd.Execute())

		return out.String()
	}

	assert.Equal(t, `enabled      false
max size     unlimited
max objects  unlimited
usage        1 objects, 5 B (5 bytes)
`, run("bucket", "b"))

	run("bucket", "b", "--max-objects", "1")
	assert.Contains(t, run("bucket", "b", "--enable", "--max-size", "1KiB"), "max size     1.0 KiB (1024 bytes)\n")

	// A flag changes only what it names.
	assert.Equal(t, `enabled      true
max size     1.0 KiB (1024 bytes)
max objects  1
usage        1 objects, 5 B (5 bytes)
`, run("bucket", "b"))

	_, err = e.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "more", Reader: strings.NewReader("x"), Size: 1})
	require.ErrorIs(t, err, fs.ErrQuotaExceeded)

	assert.Contains(t, run("account", "alice", "--enable", "--max-size", "-1", "--max-objects", "10"),
		"enabled      true\nmax size     unlimited\nmax objects  10\n")

	q, _, err := e.AccountQuota(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, engine.Quota{Enabled: true, MaxSize: -1, MaxObjects: 10}, q)
}

func TestParseLimit(t *testing.T) {
	for in, want := range map[string]int64{"-1": -1, "0": 0, "10GiB": 10 << 30, "1G": 1e9, "512": 512} {
		got, err := parseLimit(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}

	_, err := parseLimit("lots")
	require.Error(t, err)
}
