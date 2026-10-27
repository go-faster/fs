package adminhandler

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/adminapi"
	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/internal/replication"
)

func TestBucketReplication(t *testing.T) {
	ctx := context.Background()
	api := newTestAPI(t)

	_, err := api.GetBucketReplication(ctx, adminapi.GetBucketReplicationParams{Bucket: "b"})
	requireStatus(t, err, http.StatusNotImplemented)

	e, err := engine.Open(t.TempDir(), engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })

	// The handshake is the replication package's, tested there; here it
	// accepts every target but one.
	var checked []engine.ReplicationTarget

	api.opts.Engine = e
	api.opts.ReplicationCheck = func(_ context.Context, t engine.ReplicationTarget) error {
		checked = append(checked, t)
		if t.Bucket == "unreachable" {
			return errors.New("connection refused")
		}

		return nil
	}

	_, err = api.GetBucketReplication(ctx, adminapi.GetBucketReplicationParams{Bucket: "b"})
	requireStatus(t, err, http.StatusNotFound)

	require.NoError(t, e.CreateBucket(ctx, "b"))

	got, err := api.GetBucketReplication(ctx, adminapi.GetBucketReplicationParams{Bucket: "b"})
	require.NoError(t, err)
	assert.Empty(t, got.Targets)

	dr := adminapi.ReplicationTarget{
		Name: "dr", Endpoint: "https://s3.example.com", Bucket: "replica",
		AccessKeyID: "AK", SecretAccessKey: adminapi.NewOptString("secret"),
		DeleteMarkers: adminapi.NewOptBool(true),
	}
	set := func(targets ...adminapi.ReplicationTarget) (*adminapi.Replication, error) {
		return api.SetBucketReplication(ctx, &adminapi.Replication{Targets: targets}, adminapi.SetBucketReplicationParams{Bucket: "b"})
	}

	_, err = set(dr)
	requireStatus(t, err, http.StatusConflict) // Not versioned.

	require.NoError(t, e.SetBucketVersioning(ctx, "b", fs.VersioningEnabled))

	got, err = set(dr)
	require.NoError(t, err)
	require.Len(t, got.Targets, 1)
	assert.False(t, got.Targets[0].SecretAccessKey.Set, "the secret is never returned")
	assert.True(t, got.Targets[0].DeleteMarkers.Or(false))

	// Without a secret, the target keeps its own.
	kept := dr
	kept.SecretAccessKey = adminapi.OptString{}
	kept.Prefix = adminapi.NewOptString("logs/")

	_, err = set(kept)
	require.NoError(t, err)

	stored, err := e.BucketReplication(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, "secret", stored[0].SecretAccessKey)
	assert.Equal(t, "logs/", stored[0].Prefix)
	assert.Equal(t, "secret", checked[len(checked)-1].SecretAccessKey, "checked with the kept secret")

	// Refused: a target failing the handshake, plain http, no secret at all.
	bad := dr
	bad.Name, bad.Bucket = "other", "unreachable"
	_, err = set(dr, bad)
	requireStatus(t, err, http.StatusBadRequest)

	plain := dr
	plain.Endpoint = "http://s3.example.com"
	_, err = set(plain)
	requireStatus(t, err, http.StatusBadRequest)

	anon := dr
	anon.Name, anon.SecretAccessKey = "new", adminapi.OptString{}
	_, err = set(anon)
	requireStatus(t, err, http.StatusBadRequest)

	stored, err = e.BucketReplication(ctx, "b")
	require.NoError(t, err)
	require.Len(t, stored, 1, "a refused set changes nothing")

	// Status: one version pending for dr, read from the outbox.
	_, err = e.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "logs/1", Reader: bytes.NewReader([]byte("x")), Size: 1})
	require.NoError(t, err)

	st, err := api.GetBucketReplicationStatus(ctx, adminapi.GetBucketReplicationStatusParams{Bucket: "b"})
	require.NoError(t, err)
	assert.True(t, st.Complete)
	assert.False(t, st.Runner)
	assert.Equal(t, int64(1), st.Backlog)
	require.Len(t, st.Targets, 1)
	assert.Equal(t, int64(1), st.Targets[0].Pending)
	assert.GreaterOrEqual(t, st.Targets[0].LagSeconds, 0.0)
	assert.False(t, st.Targets[0].Shipped.Set, "no runner here")

	// A new target asked for a backfill, which has not started yet.
	bf, ok := st.Targets[0].Backfill.Get()
	require.True(t, ok)
	assert.False(t, bf.Done)
	assert.False(t, bf.Requested.IsZero())

	require.NoError(t, api.BackfillBucketReplication(ctx, adminapi.BackfillBucketReplicationParams{Bucket: "b"}))
	requireStatus(t, api.BackfillBucketReplication(ctx, adminapi.BackfillBucketReplicationParams{
		Bucket: "b", Target: adminapi.NewOptString("nope"),
	}), http.StatusConflict)

	st2, err := api.GetBucketReplicationStatus(ctx, adminapi.GetBucketReplicationStatusParams{Bucket: "b"})
	require.NoError(t, err)
	assert.True(t, st2.Targets[0].Backfill.Value.Requested.After(bf.Requested), "asked again")

	// With a runner on this node, its view too.
	api.opts.Replication = &replication.Runner{Source: e}

	st, err = api.GetBucketReplicationStatus(ctx, adminapi.GetBucketReplicationStatusParams{Bucket: "b"})
	require.NoError(t, err)
	assert.True(t, st.Runner)

	require.NoError(t, api.DeleteBucketReplication(ctx, adminapi.DeleteBucketReplicationParams{Bucket: "b"}))

	got, err = api.GetBucketReplication(ctx, adminapi.GetBucketReplicationParams{Bucket: "b"})
	require.NoError(t, err)
	assert.Empty(t, got.Targets)
}
