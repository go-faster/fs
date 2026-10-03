package adminhandler

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/adminapi"
	"github.com/go-faster/fs/engine"
)

func TestBucketScheme(t *testing.T) {
	ctx := context.Background()
	api := newTestAPI(t)

	_, err := api.GetBucketScheme(ctx, adminapi.GetBucketSchemeParams{Bucket: "b"})
	requireStatus(t, err, http.StatusNotImplemented)

	e, err := engine.Open(t.TempDir(), engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })

	api.opts.Engine = e

	_, err = api.GetBucketScheme(ctx, adminapi.GetBucketSchemeParams{Bucket: "b"})
	requireStatus(t, err, http.StatusNotFound)

	require.NoError(t, e.CreateBucket(ctx, "b"))

	got, err := api.GetBucketScheme(ctx, adminapi.GetBucketSchemeParams{Bucket: "b"})
	require.NoError(t, err)
	assert.Equal(t, "rf3", got.Scheme)

	// A single node cannot spread shards.
	_, err = api.SetBucketScheme(ctx, &adminapi.BucketScheme{Scheme: "ec:2,1"}, adminapi.SetBucketSchemeParams{Bucket: "b"})
	requireStatus(t, err, http.StatusBadRequest)

	_, err = api.SetBucketScheme(ctx, &adminapi.BucketScheme{Scheme: "raid5"}, adminapi.SetBucketSchemeParams{Bucket: "b"})
	requireStatus(t, err, http.StatusBadRequest)

	got, err = api.SetBucketScheme(ctx, &adminapi.BucketScheme{Scheme: "rf3"}, adminapi.SetBucketSchemeParams{Bucket: "b"})
	require.NoError(t, err)
	assert.Equal(t, "rf3", got.Scheme)
}
