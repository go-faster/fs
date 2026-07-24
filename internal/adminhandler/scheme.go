package adminhandler

import (
	"context"
	"net/http"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/adminapi"
)

// ErrBucketNotFound is returned by a BucketSchemeStore when the named bucket
// does not exist; the handler maps it to 404.
var ErrBucketNotFound = errors.New("bucket not found")

// ErrSchemeRejected is returned by a BucketSchemeStore when a scheme is invalid
// or the topology cannot host it; the handler maps it to 400.
var ErrSchemeRejected = errors.New("scheme rejected")

// BucketSchemeStore reads and writes a bucket's replication-scheme override in
// the cluster control plane. It is nil outside cluster mode, where the scheme
// endpoints return 501.
type BucketSchemeStore interface {
	// SchemeOverride returns the bucket's explicit scheme override, empty when
	// the bucket follows the cluster default. It returns ErrBucketNotFound when
	// the bucket does not exist.
	SchemeOverride(ctx context.Context, bucket string) (string, error)
	// SetScheme sets the bucket's override, or clears it when scheme is empty.
	// It returns ErrBucketNotFound when the bucket does not exist and
	// ErrSchemeRejected when the scheme is invalid or the topology cannot host
	// it.
	SetScheme(ctx context.Context, bucket, scheme string) error
}

// GetBucketScheme reports a bucket's effective scheme, its override and the
// cluster default.
func (a *AdminAPI) GetBucketScheme(ctx context.Context, params adminapi.GetBucketSchemeParams) (*adminapi.BucketScheme, error) {
	if a.opts.BucketSchemes == nil {
		return nil, a.errNoBucketSchemes()
	}

	override, err := a.opts.BucketSchemes.SchemeOverride(ctx, params.Bucket)
	if err != nil {
		return nil, schemeError(err)
	}

	return a.bucketScheme(params.Bucket, override), nil
}

// SetBucketScheme sets or clears a bucket's scheme override and returns the
// effective scheme after applying.
func (a *AdminAPI) SetBucketScheme(ctx context.Context, req *adminapi.SetBucketSchemeRequest, params adminapi.SetBucketSchemeParams) (*adminapi.BucketScheme, error) {
	if a.opts.BucketSchemes == nil {
		return nil, a.errNoBucketSchemes()
	}

	// "default" is the CLI spelling for "clear the override"; accept it as a
	// synonym for the empty string so both forms work.
	override := req.Scheme.Or("")
	if override == "default" {
		override = ""
	}

	if err := a.opts.BucketSchemes.SetScheme(ctx, params.Bucket, override); err != nil {
		return nil, schemeError(err)
	}

	// Read back so the response carries the stored, normalized form (e.g.
	// "rf2_5" becomes "rf2.5") rather than the request's spelling.
	stored, err := a.opts.BucketSchemes.SchemeOverride(ctx, params.Bucket)
	if err != nil {
		return nil, schemeError(err)
	}

	return a.bucketScheme(params.Bucket, stored), nil
}

// bucketScheme assembles the wire response from an override (empty = default).
func (a *AdminAPI) bucketScheme(bucket, override string) *adminapi.BucketScheme {
	out := &adminapi.BucketScheme{
		Bucket:         bucket,
		ClusterDefault: a.opts.ClusterDefaultScheme,
		IsDefault:      override == "",
	}

	if override == "" {
		out.Scheme = a.opts.ClusterDefaultScheme
	} else {
		out.Scheme = override
		out.Override = adminapi.NewOptString(override)
	}

	return out
}

// errNoBucketSchemes reports that per-bucket schemes are unavailable (not in
// cluster mode).
func (a *AdminAPI) errNoBucketSchemes() *adminapi.ErrorStatusCode {
	return apiErr(http.StatusNotImplemented, errors.New("per-bucket schemes are not available on this admin listener"))
}

// schemeError maps a store error to its HTTP status.
func schemeError(err error) *adminapi.ErrorStatusCode {
	switch {
	case errors.Is(err, ErrBucketNotFound):
		return apiErr(http.StatusNotFound, err)
	case errors.Is(err, ErrSchemeRejected):
		return apiErr(http.StatusBadRequest, err)
	default:
		return apiErr(http.StatusInternalServerError, err)
	}
}
