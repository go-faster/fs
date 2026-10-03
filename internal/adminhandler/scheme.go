package adminhandler

import (
	"context"
	"net/http"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/adminapi"
)

// GetBucketScheme reports how a bucket's new blocks are stored.
func (a *AdminAPI) GetBucketScheme(ctx context.Context, params adminapi.GetBucketSchemeParams) (*adminapi.BucketScheme, error) {
	if a.opts.Engine == nil {
		return nil, apiErr(http.StatusNotImplemented, errors.New("no storage engine"))
	}

	s, err := a.opts.Engine.BucketScheme(ctx, params.Bucket)
	if err != nil {
		return nil, schemeErr(err)
	}

	return &adminapi.BucketScheme{Scheme: s}, nil
}

// SetBucketScheme sets how a bucket's new blocks are stored.
func (a *AdminAPI) SetBucketScheme(
	ctx context.Context, req *adminapi.BucketScheme, params adminapi.SetBucketSchemeParams,
) (*adminapi.BucketScheme, error) {
	if a.opts.Engine == nil {
		return nil, apiErr(http.StatusNotImplemented, errors.New("no storage engine"))
	}

	if err := a.opts.Engine.SetBucketScheme(ctx, params.Bucket, req.Scheme); err != nil {
		return nil, schemeErr(err)
	}

	return a.GetBucketScheme(ctx, adminapi.GetBucketSchemeParams(params))
}

func schemeErr(err error) error {
	switch {
	case errors.Is(err, fs.ErrBucketNotFound):
		return apiErr(http.StatusNotFound, err)
	case errors.Is(err, fs.ErrUnsupportedOperation):
		return apiErr(http.StatusBadRequest, err)
	default:
		return err
	}
}
