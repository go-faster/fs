package adminhandler

import (
	"context"
	"net/http"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/adminapi"
	"github.com/go-faster/fs/engine"
)

// GetBucketQuota reports a bucket's quota and what it stores.
func (a *AdminAPI) GetBucketQuota(ctx context.Context, params adminapi.GetBucketQuotaParams) (*adminapi.QuotaStatus, error) {
	if a.opts.Engine == nil {
		return nil, apiErr(http.StatusNotImplemented, errors.New("no storage engine"))
	}

	q, u, err := a.opts.Engine.BucketQuota(ctx, params.Bucket)
	if err != nil {
		return nil, settingErr(err)
	}

	return quotaStatus(q, u), nil
}

// SetBucketQuota replaces a bucket's quota.
func (a *AdminAPI) SetBucketQuota(
	ctx context.Context, req *adminapi.Quota, params adminapi.SetBucketQuotaParams,
) (*adminapi.QuotaStatus, error) {
	if a.opts.Engine == nil {
		return nil, apiErr(http.StatusNotImplemented, errors.New("no storage engine"))
	}

	if err := a.opts.Engine.SetBucketQuota(ctx, params.Bucket, quotaFromAPI(req)); err != nil {
		return nil, settingErr(err)
	}

	return a.GetBucketQuota(ctx, adminapi.GetBucketQuotaParams(params))
}

// GetAccountQuota reports an account's quota and what its buckets store.
func (a *AdminAPI) GetAccountQuota(ctx context.Context, params adminapi.GetAccountQuotaParams) (*adminapi.QuotaStatus, error) {
	if a.opts.Engine == nil {
		return nil, apiErr(http.StatusNotImplemented, errors.New("no storage engine"))
	}

	q, u, err := a.opts.Engine.AccountQuota(ctx, params.Account)
	if err != nil {
		return nil, settingErr(err)
	}

	return quotaStatus(q, u), nil
}

// SetAccountQuota replaces an account's quota.
func (a *AdminAPI) SetAccountQuota(
	ctx context.Context, req *adminapi.Quota, params adminapi.SetAccountQuotaParams,
) (*adminapi.QuotaStatus, error) {
	if a.opts.Engine == nil {
		return nil, apiErr(http.StatusNotImplemented, errors.New("no storage engine"))
	}

	if err := a.opts.Engine.SetAccountQuota(ctx, params.Account, quotaFromAPI(req)); err != nil {
		return nil, settingErr(err)
	}

	return a.GetAccountQuota(ctx, adminapi.GetAccountQuotaParams(params))
}

func quotaFromAPI(q *adminapi.Quota) engine.Quota {
	return engine.Quota{Enabled: q.Enabled, MaxSize: q.MaxSize.Or(-1), MaxObjects: q.MaxObjects.Or(-1)}
}

func quotaStatus(q engine.Quota, u engine.Usage) *adminapi.QuotaStatus {
	return &adminapi.QuotaStatus{
		Quota: adminapi.Quota{
			Enabled:    q.Enabled,
			MaxSize:    adminapi.NewOptInt64(q.MaxSize),
			MaxObjects: adminapi.NewOptInt64(q.MaxObjects),
		},
		Usage: adminapi.QuotaUsage{Objects: u.Objects, Bytes: u.Bytes},
	}
}
