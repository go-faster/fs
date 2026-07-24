package adminhandler

import (
	"context"
	"net/http"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/adminapi"
)

// ErrPublicReadRejected is returned by a PublicReadStore when a bucket name is
// invalid; the handler maps it to 400.
var ErrPublicReadRejected = errors.New("public-read bucket list rejected")

// PublicReadStore reads and writes the cluster-wide list of anonymously-readable
// buckets in the control plane. It is nil unless the server uses cluster-wide
// credentials (auth.source: etcd), where the public-read endpoints return 501.
type PublicReadStore interface {
	// PublicReadBuckets returns the current public-read bucket list.
	PublicReadBuckets(ctx context.Context) ([]string, error)
	// SetPublicReadBuckets replaces the list. It returns ErrPublicReadRejected
	// when a bucket name is invalid.
	SetPublicReadBuckets(ctx context.Context, buckets []string) error
}

// GetPublicReadBuckets returns the cluster-wide public-read bucket list.
func (a *AdminAPI) GetPublicReadBuckets(ctx context.Context) (*adminapi.PublicReadBuckets, error) {
	if a.opts.PublicRead == nil {
		return nil, a.errNoPublicReadStore()
	}

	buckets, err := a.opts.PublicRead.PublicReadBuckets(ctx)
	if err != nil {
		return nil, publicReadError(err)
	}

	return publicReadResponse(buckets), nil
}

// SetPublicReadBuckets replaces the cluster-wide public-read bucket list and
// returns the stored result.
func (a *AdminAPI) SetPublicReadBuckets(ctx context.Context, req *adminapi.SetPublicReadBucketsRequest) (*adminapi.PublicReadBuckets, error) {
	if a.opts.PublicRead == nil {
		return nil, a.errNoPublicReadStore()
	}

	if err := a.opts.PublicRead.SetPublicReadBuckets(ctx, req.Buckets); err != nil {
		return nil, publicReadError(err)
	}

	// Read back so the response reflects the stored, normalized list.
	buckets, err := a.opts.PublicRead.PublicReadBuckets(ctx)
	if err != nil {
		return nil, publicReadError(err)
	}

	return publicReadResponse(buckets), nil
}

// publicReadResponse builds the wire response, normalizing nil to an empty list.
func publicReadResponse(buckets []string) *adminapi.PublicReadBuckets {
	if buckets == nil {
		buckets = []string{}
	}

	return &adminapi.PublicReadBuckets{Buckets: buckets}
}

// errNoPublicReadStore reports that cluster-wide public-read management is
// unavailable (not using cluster-wide credentials).
func (a *AdminAPI) errNoPublicReadStore() *adminapi.ErrorStatusCode {
	return apiErr(http.StatusNotImplemented, errors.New("cluster-wide public-read buckets require auth.source: etcd"))
}

// publicReadError maps a store error to its HTTP status.
func publicReadError(err error) *adminapi.ErrorStatusCode {
	if errors.Is(err, ErrPublicReadRejected) {
		return apiErr(http.StatusBadRequest, err)
	}

	return apiErr(http.StatusInternalServerError, err)
}
