package adminhandler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/adminapi"
	"github.com/go-faster/fs/engine"
)

func TestQuota(t *testing.T) {
	ctx := context.Background()
	api := newTestAPI(t)

	_, err := api.GetBucketQuota(ctx, adminapi.GetBucketQuotaParams{Bucket: "b"})
	requireStatus(t, err, http.StatusNotImplemented)

	_, err = api.GetAccountQuota(ctx, adminapi.GetAccountQuotaParams{Account: "alice"})
	requireStatus(t, err, http.StatusNotImplemented)

	e, err := engine.Open(t.TempDir(), engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })

	api.opts.Engine = e

	_, err = api.GetBucketQuota(ctx, adminapi.GetBucketQuotaParams{Bucket: "b"})
	requireStatus(t, err, http.StatusNotFound)

	require.NoError(t, e.CreateBucketOwned(ctx, "b", fs.Owner{ID: "alice"}))

	_, err = e.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "k", Reader: strings.NewReader("hello"), Size: 5})
	require.NoError(t, err)

	got, err := api.GetBucketQuota(ctx, adminapi.GetBucketQuotaParams{Bucket: "b"})
	require.NoError(t, err)
	assert.Equal(t, &adminapi.QuotaStatus{
		Quota: adminapi.Quota{MaxSize: adminapi.NewOptInt64(-1), MaxObjects: adminapi.NewOptInt64(-1)},
		Usage: adminapi.QuotaUsage{Objects: 1, Bytes: 5},
	}, got)

	// An omitted limit is no limit.
	got, err = api.SetBucketQuota(ctx, &adminapi.Quota{Enabled: true, MaxObjects: adminapi.NewOptInt64(1)},
		adminapi.SetBucketQuotaParams{Bucket: "b"})
	require.NoError(t, err)
	assert.Equal(t, adminapi.Quota{
		Enabled: true, MaxSize: adminapi.NewOptInt64(-1), MaxObjects: adminapi.NewOptInt64(1),
	}, got.Quota)

	_, err = api.SetBucketQuota(ctx, &adminapi.Quota{MaxSize: adminapi.NewOptInt64(-5)},
		adminapi.SetBucketQuotaParams{Bucket: "b"})
	requireStatus(t, err, http.StatusBadRequest)

	got, err = api.SetAccountQuota(ctx, &adminapi.Quota{Enabled: true, MaxSize: adminapi.NewOptInt64(1 << 20)},
		adminapi.SetAccountQuotaParams{Account: "alice"})
	require.NoError(t, err)
	assert.Equal(t, &adminapi.QuotaStatus{
		Quota: adminapi.Quota{Enabled: true, MaxSize: adminapi.NewOptInt64(1 << 20), MaxObjects: adminapi.NewOptInt64(-1)},
		Usage: adminapi.QuotaUsage{Objects: 1, Bytes: 5},
	}, got)
}
