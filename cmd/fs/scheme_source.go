package main

import (
	"context"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

// clusterDefaultScheme resolves the cluster's default object scheme from the
// config, falling back to the built-in default when unset or unparsable. It is
// the scheme the admin API echoes for buckets without an override.
func clusterDefaultScheme(cfg Config) string {
	if cfg.Cluster.Scheme != "" {
		if s, err := scheme.Parse(cfg.Cluster.Scheme); err == nil {
			return s.String()
		}
	}

	return scheme.Default.String()
}

// bucketSchemeSource adapts the cluster coordinator to
// adminhandler.BucketSchemeStore, translating its errors to the handler's
// sentinels so each maps to the right HTTP status. It backs the scheme
// endpoints on both the per-node admin and the headless `fs admin`.
type bucketSchemeSource struct {
	coord *clusterstore.Coordinator
}

var _ adminhandler.BucketSchemeStore = bucketSchemeSource{}

func newBucketSchemeSource(coord *clusterstore.Coordinator) bucketSchemeSource {
	return bucketSchemeSource{coord: coord}
}

// SchemeOverride returns the bucket's explicit scheme override, empty for the
// cluster default.
func (s bucketSchemeSource) SchemeOverride(ctx context.Context, bucket string) (string, error) {
	info, err := s.coord.Bucket(ctx, bucket)
	if err != nil {
		if errors.Is(err, fs.ErrBucketNotFound) {
			return "", errors.Wrap(adminhandler.ErrBucketNotFound, bucket)
		}

		return "", err
	}

	return info.Scheme, nil
}

// SetScheme sets or clears the bucket's override.
func (s bucketSchemeSource) SetScheme(ctx context.Context, bucket, schemeID string) error {
	switch err := s.coord.SetBucketScheme(ctx, bucket, schemeID); {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrBucketNotFound):
		return errors.Wrap(adminhandler.ErrBucketNotFound, bucket)
	case errors.Is(err, clusterstore.ErrSchemeRejected):
		// Re-attribute the rejection to the handler's sentinel (→ 400), keeping
		// the coordinator's reason as the message.
		return errors.Wrap(adminhandler.ErrSchemeRejected, err.Error())
	default:
		return err
	}
}
