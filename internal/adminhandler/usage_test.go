package adminhandler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/adminapi"
)

type stubBucketUsage struct {
	records []BucketUsage
	err     error
}

func (s stubBucketUsage) BucketUsage(context.Context) ([]BucketUsage, error) {
	return s.records, s.err
}

func TestBucketUsageReported(t *testing.T) {
	counted := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	updated := counted.Add(time.Hour)

	a := NewAdminAPI(Options{BucketUsage: stubBucketUsage{records: []BucketUsage{
		{Bucket: "photos", Objects: 1200, Bytes: 8 << 30, Updated: updated, Counted: counted},
		// Never recounted: maintained only by deltas so far.
		{Bucket: "logs", Objects: 4, Bytes: 512, Updated: updated},
	}}})

	out, err := a.GetBucketUsage(t.Context())
	require.NoError(t, err)
	require.Len(t, out.Buckets, 2)

	assert.Equal(t, "photos", out.Buckets[0].Bucket)
	assert.EqualValues(t, 1200, out.Buckets[0].Objects)
	assert.EqualValues(t, 8<<30, out.Buckets[0].Bytes)
	assert.Equal(t, counted, out.Buckets[0].Counted.Or(time.Time{}))

	_, ok := out.Buckets[1].Counted.Get()
	assert.False(t, ok, "a bucket nothing has verified must not look recounted at the epoch")

	// The cluster-wide totals are summed here so a caller does not have to.
	assert.EqualValues(t, 1204, out.Objects.Or(0))
	assert.EqualValues(t, (8<<30)+512, out.Bytes.Or(0))
}

// TestBucketUsageDisabled: a single-node server has no usage index — the
// counters live in the cluster control plane — so the endpoint says so rather
// than reporting an empty cluster.
func TestBucketUsageDisabled(t *testing.T) {
	a := NewAdminAPI(Options{})

	_, err := a.GetBucketUsage(t.Context())
	require.Error(t, err)

	var status *adminapi.ErrorStatusCode
	require.ErrorAs(t, err, &status)
	assert.Equal(t, http.StatusNotImplemented, status.StatusCode)
}

func TestBucketUsageSourceFailure(t *testing.T) {
	a := NewAdminAPI(Options{BucketUsage: stubBucketUsage{err: errors.New("etcd unavailable")}})

	_, err := a.GetBucketUsage(t.Context())
	require.Error(t, err)

	var status *adminapi.ErrorStatusCode
	require.ErrorAs(t, err, &status)
	assert.Equal(t, http.StatusInternalServerError, status.StatusCode)
}
