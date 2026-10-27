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
	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/replication"
	"github.com/go-faster/fs/server"
)

func TestReplicationCommand(t *testing.T) {
	const (
		token = "s3cr3t"
		ak    = "AKIAREPLICATION00000"
		sk    = "replication-secret"
	)

	ctx := context.Background()

	open := func() *engine.Engine {
		e, err := engine.Open(t.TempDir(), engine.Options{NoSync: true})
		require.NoError(t, err)
		t.Cleanup(func() { _ = e.Close() })

		require.NoError(t, e.CreateBucket(ctx, "photos"))
		require.NoError(t, e.SetBucketVersioning(ctx, "photos", fs.VersioningEnabled))

		return e
	}

	src, dst := open(), open()

	store, err := auth.NewStore(auth.Config{Keys: []auth.Key{{
		AccessKey: ak, SecretKey: sk, Grants: []auth.Grant{{Pattern: "*", Permission: auth.Admin}},
	}}})
	require.NoError(t, err)

	target := httptest.NewServer(server.NewHandler(dst, server.WithAuth(store)))
	t.Cleanup(target.Close)

	runner := &replication.Runner{Source: src}

	s, err := adminapi.NewServer(adminhandler.NewAdminAPI(adminhandler.Options{Engine: src, Replication: runner}))
	require.NoError(t, err)

	srv := httptest.NewServer(bearerAuth(token, s))
	t.Cleanup(srv.Close)

	run := func(args ...string) (string, error) {
		t.Helper()

		var out bytes.Buffer

		cmd := Replication()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append(args, "--admin-addr", srv.URL, "--token", token))
		err := cmd.Execute()

		return out.String(), err
	}

	out, err := run("get", "photos")
	require.NoError(t, err)
	assert.Equal(t, "no targets\n", out)

	_, err = run("set", "photos", "--name", "dr", "--endpoint", target.URL, "--target-bucket", "photos",
		"--access-key", ak, "--secret-key", "wrong", "--insecure")
	require.Error(t, err, "the handshake refuses a wrong secret")

	out, err = run("set", "photos", "--name", "dr", "--endpoint", target.URL, "--target-bucket", "photos",
		"--access-key", ak, "--secret-key", sk, "--insecure", "--delete-markers")
	require.NoError(t, err)
	assert.Equal(t, "dr\t"+target.URL+"/photos\tkey "+ak+"\tdelete markers\tinsecure\n", out)
	assert.NotContains(t, out, sk)

	// Replaced without a secret: it keeps its own, and passes the handshake.
	out, err = run("set", "photos", "--name", "dr", "--endpoint", target.URL, "--target-bucket", "photos",
		"--access-key", ak, "--insecure", "--prefix", "img/")
	require.NoError(t, err)
	assert.Contains(t, out, "prefix \"img/\"")

	_, err = src.PutObject(ctx, &fs.PutObjectRequest{Bucket: "photos", Key: "img/1", Reader: strings.NewReader("x"), Size: 1})
	require.NoError(t, err)

	out, err = run("status", "photos")
	require.NoError(t, err)
	assert.Contains(t, out, "backlog      1 versions, 1 B\n")
	assert.Contains(t, out, "pending      1 versions, 1 B\n")

	_, err = runner.Pass(ctx)
	require.NoError(t, err)

	out, err = run("status", "photos")
	require.NoError(t, err)
	assert.Contains(t, out, "backlog      0 versions, 0 B\n")
	assert.Contains(t, out, "shipped      1 versions")

	assert.Contains(t, out, "backfill     ")

	out, err = run("backfill", "photos", "dr")
	require.NoError(t, err)
	assert.Equal(t, "backfill requested; it starts in a minute\n", out)

	_, err = run("backfill", "photos", "nope")
	require.Error(t, err)

	_, err = run("rm", "photos", "nope")
	require.Error(t, err)

	out, err = run("rm", "photos", "dr")
	require.NoError(t, err)
	assert.Equal(t, "no targets\n", out)
}
