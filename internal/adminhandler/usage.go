package adminhandler

import (
	"context"
	"net/http"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/adminapi"
)

// BucketUsageSource reads the cluster's durable per-bucket object accounting.
type BucketUsageSource interface {
	// BucketUsage returns every accounted bucket, sorted by name.
	BucketUsage(ctx context.Context) ([]BucketUsage, error)
}

// BucketUsage is one bucket's object count and total size, as the usage index
// holds it.
type BucketUsage struct {
	Bucket  string
	Objects int64
	Bytes   int64
	// Updated is when an incremental change last moved the counters.
	Updated time.Time
	// Counted is when a full recount last anchored them; zero when none has.
	Counted time.Time
}

// GetBucketUsage reports per-bucket object counts and sizes, plus the totals
// across every bucket.
func (a *AdminAPI) GetBucketUsage(ctx context.Context) (*adminapi.BucketUsageList, error) {
	if a.opts.BucketUsage == nil {
		return nil, a.errNoBucketUsage()
	}

	records, err := a.opts.BucketUsage.BucketUsage(ctx)
	if err != nil {
		return nil, apiErr(http.StatusInternalServerError, err)
	}

	out := &adminapi.BucketUsageList{
		Buckets: make([]adminapi.BucketUsage, 0, len(records)),
	}

	var objects, bytes int64

	for _, rec := range records {
		objects += rec.Objects
		bytes += rec.Bytes

		bucket := adminapi.BucketUsage{
			Bucket:  rec.Bucket,
			Objects: rec.Objects,
			Bytes:   rec.Bytes,
		}

		if !rec.Updated.IsZero() {
			bucket.Updated = adminapi.NewOptDateTime(rec.Updated)
		}

		// Absent rather than zero-valued: a bucket whose totals have never been
		// verified against the objects themselves must be distinguishable from
		// one recounted at the epoch.
		if !rec.Counted.IsZero() {
			bucket.Counted = adminapi.NewOptDateTime(rec.Counted)
		}

		out.Buckets = append(out.Buckets, bucket)
	}

	out.Objects = adminapi.NewOptInt64(objects)
	out.Bytes = adminapi.NewOptInt64(bytes)

	return out, nil
}

// errNoBucketUsage reports that usage accounting is unavailable (not in cluster
// mode, where the index lives in the control plane).
func (a *AdminAPI) errNoBucketUsage() *adminapi.ErrorStatusCode {
	return apiErr(http.StatusNotImplemented, errors.New("bucket usage accounting is not available on this admin listener"))
}
