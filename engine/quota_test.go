package engine

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func putIn(ctx context.Context, e *Engine, bucket, key string, size int) error {
	_, err := e.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: bucket, Key: key, Reader: strings.NewReader(strings.Repeat("x", size)), Size: int64(size),
	})

	return err
}

func TestQuotaExceeded(t *testing.T) {
	q := Quota{Enabled: true, MaxSize: 10, MaxObjects: 2}

	for _, tt := range []struct {
		u, d Usage
		want bool
	}{
		{Usage{1, 5}, Usage{1, 5}, false},
		{Usage{2, 5}, Usage{1, 1}, true},
		{Usage{1, 5}, Usage{1, 6}, true},
		{Usage{2, 10}, Usage{0, 0}, false},
		{Usage{3, 20}, Usage{-1, -5}, false}, // Freeing space past the limit is allowed.
		{Usage{3, 20}, Usage{0, 1}, true},
	} {
		assert.Equal(t, tt.want, q.exceeded(tt.u, tt.d), "%+v + %+v", tt.u, tt.d)
	}

	assert.False(t, Quota{MaxSize: 0, MaxObjects: 0}.exceeded(Usage{}, Usage{1, 1}), "disabled")
	assert.False(t, Quota{Enabled: true, MaxSize: -1, MaxObjects: -1}.exceeded(Usage{}, Usage{1 << 40, 1 << 40}))
	assert.True(t, Quota{Enabled: true, MaxSize: -1, MaxObjects: 0}.exceeded(Usage{}, Usage{1, 0}), "zero allows nothing")
}

func TestBucketQuota(t *testing.T) {
	for _, n := range []int{1, 3} {
		t.Run(fmt.Sprint(n, "nodes"), func(t *testing.T) {
			ctx := context.Background()
			e := newEngine(t, n)

			q, u, err := e.BucketQuota(ctx, "b")
			require.NoError(t, err)
			assert.Equal(t, Unlimited, q, "no quota until one is set")
			assert.Zero(t, u)

			require.NoError(t, e.SetBucketQuota(ctx, "b", Quota{Enabled: true, MaxSize: 100, MaxObjects: 2}))

			require.NoError(t, putIn(ctx, e, "b", "one", 40))
			require.NoError(t, putIn(ctx, e, "b", "two", 40))
			require.ErrorIs(t, putIn(ctx, e, "b", "three", 1), fs.ErrQuotaExceeded, "a third object")

			// Overwriting replaces: the count stays, and only growth counts.
			require.NoError(t, putIn(ctx, e, "b", "one", 60))
			require.ErrorIs(t, putIn(ctx, e, "b", "one", 61), fs.ErrQuotaExceeded, "past 100 bytes")

			q, u, err = e.BucketQuota(ctx, "b")
			require.NoError(t, err)
			assert.Equal(t, Quota{Enabled: true, MaxSize: 100, MaxObjects: 2}, q)
			assert.Equal(t, Usage{Objects: 2, Bytes: 100}, u)

			// Deleting frees what it held at once.
			require.NoError(t, e.DeleteObject(ctx, "b", "two"))
			require.NoError(t, putIn(ctx, e, "b", "three", 40))

			// Disabled, the limits stay but refuse nothing.
			require.NoError(t, e.SetBucketQuota(ctx, "b", Quota{MaxSize: 100, MaxObjects: 2}))
			require.NoError(t, putIn(ctx, e, "b", "four", 40))

			require.ErrorIs(t, e.SetBucketQuota(ctx, "b", Quota{MaxSize: -2}), fs.ErrUnsupportedOperation)
			require.ErrorIs(t, e.SetBucketQuota(ctx, "missing", Unlimited), fs.ErrBucketNotFound)
		})
	}
}

func TestBucketQuotaVersions(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, 1)

	require.NoError(t, e.SetBucketVersioning(ctx, "b", fs.VersioningEnabled))
	require.NoError(t, e.SetBucketQuota(ctx, "b", Quota{Enabled: true, MaxSize: -1, MaxObjects: 2}))

	// Every version is stored, and counts.
	require.NoError(t, putIn(ctx, e, "b", "k", 1))
	require.NoError(t, putIn(ctx, e, "b", "k", 1))
	require.ErrorIs(t, putIn(ctx, e, "b", "k", 1), fs.ErrQuotaExceeded)

	// A delete marker holds nothing, and removes nothing either.
	require.NoError(t, e.DeleteObject(ctx, "b", "k"))
	require.ErrorIs(t, putIn(ctx, e, "b", "k", 1), fs.ErrQuotaExceeded)

	versions, err := e.ListObjectVersions(ctx, &fs.ListObjectVersionsRequest{Bucket: "b"})
	require.NoError(t, err)

	for _, v := range versions.Versions {
		if !v.DeleteMarker {
			_, err := e.DeleteObjectVersion(ctx, "b", "k", v.VersionID)
			require.NoError(t, err)

			break
		}
	}

	require.NoError(t, putIn(ctx, e, "b", "k", 1))
}

func TestAccountQuota(t *testing.T) {
	ctx := context.Background()
	e := cluster(t, 3, small)[0].engine

	alice, bob := fs.Owner{ID: "alice"}, fs.Owner{ID: "bob"}
	require.NoError(t, e.CreateBucketOwned(ctx, "a1", alice))
	require.NoError(t, e.CreateBucketOwned(ctx, "a2", alice))
	require.NoError(t, e.CreateBucketOwned(ctx, "b1", bob))

	q, _, err := e.AccountQuota(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, Unlimited, q)

	require.NoError(t, e.SetAccountQuota(ctx, "alice", Quota{Enabled: true, MaxSize: 100, MaxObjects: -1}))

	// Alice's buckets count together.
	require.NoError(t, putIn(ctx, e, "a1", "k", 60))
	require.ErrorIs(t, putIn(ctx, e, "a2", "k", 60), fs.ErrQuotaExceeded)
	require.NoError(t, putIn(ctx, e, "a2", "k", 40))

	// Bob is not Alice.
	require.NoError(t, putIn(ctx, e, "b1", "k", 1000))

	// A bucket Alice creates counts at once.
	require.NoError(t, e.CreateBucketOwned(ctx, "a3", alice))
	require.ErrorIs(t, putIn(ctx, e, "a3", "k", 1), fs.ErrQuotaExceeded)

	q, u, err := e.AccountQuota(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, Quota{Enabled: true, MaxSize: 100, MaxObjects: -1}, q)
	assert.Equal(t, Usage{Objects: 2, Bytes: 100}, u)

	// A bucket's own quota applies alongside its owner's.
	require.NoError(t, e.SetAccountQuota(ctx, "alice", Unlimited))
	require.NoError(t, e.SetBucketQuota(ctx, "a1", Quota{Enabled: true, MaxSize: -1, MaxObjects: 1}))
	require.ErrorIs(t, putIn(ctx, e, "a1", "other", 1), fs.ErrQuotaExceeded)
	require.NoError(t, putIn(ctx, e, "a2", "other", 1000))

	stats := e.QuotaStats()
	assert.Equal(t, int64(2), stats.RejectedAccount)
	assert.Equal(t, int64(1), stats.RejectedBucket)
	assert.Positive(t, stats.Checks)

	require.ErrorIs(t, e.SetAccountQuota(ctx, "", Unlimited), fs.ErrUnsupportedOperation)
}

func TestQuotaMultipart(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, 1)

	require.NoError(t, e.SetBucketQuota(ctx, "b", Quota{Enabled: true, MaxSize: 100, MaxObjects: -1}))

	up, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "k"})
	require.NoError(t, err)

	part := func(n, size int) (*fs.Part, error) {
		return e.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket: "b", Key: "k", UploadID: up.UploadID, PartNumber: n,
			Reader: bytes.NewReader(bytes.Repeat([]byte("p"), size)), Size: int64(size),
		})
	}

	_, err = part(1, 101)
	require.ErrorIs(t, err, fs.ErrQuotaExceeded, "a part that cannot fit")

	p1, err := part(1, 60)
	require.NoError(t, err)

	p2, err := part(2, 60)
	require.NoError(t, err, "parts are not stored usage until completed")

	complete := func(parts ...*fs.Part) error {
		var cps []fs.CompletedPart
		for _, p := range parts {
			cps = append(cps, fs.CompletedPart{PartNumber: p.PartNumber, ETag: p.ETag})
		}

		_, err := e.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
			Bucket: "b", Key: "k", UploadID: up.UploadID, Parts: cps,
		})

		return err
	}

	require.ErrorIs(t, complete(p1, p2), fs.ErrQuotaExceeded)

	// The upload is still there, and completes within the quota.
	require.NoError(t, complete(p1))

	_, u, err := e.BucketQuota(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, Usage{Objects: 1, Bytes: 60}, u)
}

// TestQuotaAcrossNodes: what one node writes counts on another once the
// other's cache expires, and the usage every node reads is the same.
func TestQuotaAcrossNodes(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t, 3, Config{BlockSize: 64, InlineLimit: 16, QuotaCacheTTL: time.Nanosecond})

	require.NoError(t, nodes[0].engine.CreateBucket(ctx, "b"))
	require.NoError(t, nodes[0].engine.SetBucketQuota(ctx, "b", Quota{Enabled: true, MaxSize: -1, MaxObjects: 3}))

	for i, n := range nodes {
		require.NoError(t, putIn(ctx, n.engine, "b", fmt.Sprint(i), 100))
	}

	for _, n := range nodes {
		require.ErrorIs(t, putIn(ctx, n.engine, "b", "more", 1), fs.ErrQuotaExceeded)

		_, u, err := n.engine.BucketQuota(ctx, "b")
		require.NoError(t, err)
		assert.Equal(t, Usage{Objects: 3, Bytes: 300}, u)
	}
}
