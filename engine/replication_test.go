package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/meta"
)

func target(name string) ReplicationTarget {
	return ReplicationTarget{
		Name: name, Endpoint: "https://s3.example.com", Bucket: "replica",
		AccessKeyID: "AK", SecretAccessKey: "secret", DeleteMarkers: true,
	}
}

// replicated returns an engine of n nodes whose bucket "b" is versioned and
// replicates to targets.
func replicated(t *testing.T, n int, targets ...ReplicationTarget) *Engine {
	t.Helper()

	ctx := context.Background()
	e := newEngine(t, n)
	require.NoError(t, e.SetBucketVersioning(ctx, "b", fs.VersioningEnabled))
	require.NoError(t, e.SetBucketReplication(ctx, "b", targets))

	return e
}

// outboxOf returns bucket b's outbox rows, by key and version ID.
func outboxOf(t *testing.T, e *Engine) map[string]meta.Outbox {
	t.Helper()

	_, inc, err := e.bucket(context.Background(), "b")
	require.NoError(t, err)

	rows, err := e.outbox.Range(context.Background(), inc.ID, "", 1000)
	require.NoError(t, err)

	out := make(map[string]meta.Outbox, len(rows))
	for _, r := range rows {
		out[r.SK] = r.Row
	}

	return out
}

func TestSetBucketReplication(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, 1)

	err := e.SetBucketReplication(ctx, "b", []ReplicationTarget{target("dr")})
	require.ErrorIs(t, err, fs.ErrInvalidBucketState, "versioning is not enabled")

	require.NoError(t, e.SetBucketVersioning(ctx, "b", fs.VersioningEnabled))

	plain := target("dr")
	plain.Endpoint = "http://s3.example.com"

	for name, targets := range map[string][]ReplicationTarget{
		"http":       {plain},
		"twice":      {target("dr"), target("dr")},
		"no name":    {target("")},
		"no bucket":  {{Name: "dr", Endpoint: "https://s3.example.com", AccessKeyID: "AK", SecretAccessKey: "s"}},
		"no secret":  {{Name: "dr", Endpoint: "https://s3.example.com", Bucket: "r", AccessKeyID: "AK"}},
		"no host":    {{Name: "dr", Endpoint: "https://", Bucket: "r", AccessKeyID: "AK", SecretAccessKey: "s"}},
		"not a URL":  {{Name: "dr", Endpoint: "ftp://x", Bucket: "r", AccessKeyID: "AK", SecretAccessKey: "s"}},
		"no targets": nil,
	} {
		err := e.SetBucketReplication(ctx, "b", targets)
		if targets == nil {
			require.NoError(t, err, name)

			continue
		}

		require.ErrorIs(t, err, fs.ErrInvalidBucketState, name)
	}

	plain.Insecure = true
	require.NoError(t, e.SetBucketReplication(ctx, "b", []ReplicationTarget{plain, target("dr2")}))

	got, err := e.BucketReplication(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, []ReplicationTarget{plain, target("dr2")}, got)

	require.ErrorIs(t, e.SetBucketVersioning(ctx, "b", fs.VersioningSuspended), fs.ErrInvalidBucketState,
		"suspending versioning would leave replication nothing to ship")

	require.NoError(t, e.SetBucketReplication(ctx, "b", nil))
	require.NoError(t, e.SetBucketVersioning(ctx, "b", fs.VersioningSuspended))
}

func TestOutboxCapture(t *testing.T) {
	for _, n := range []int{1, 3} {
		t.Run(fmt.Sprint(n, "nodes"), func(t *testing.T) {
			ctx := context.Background()

			logs := target("logs")
			logs.Prefix = "logs/"
			logs.DeleteMarkers = false

			e := replicated(t, n, target("dr"), logs)

			resp, err := e.PutObject(ctx, &fs.PutObjectRequest{
				Bucket: "b", Key: "logs/1", Reader: strings.NewReader("hello"), Size: 5,
			})
			require.NoError(t, err)

			sk := meta.OutboxSK("logs/1", resp.VersionID)
			o := outboxOf(t, e)[sk]
			assert.Equal(t, []string{"dr", "logs"}, o.Targets)
			assert.True(t, o.Version)
			assert.Equal(t, int64(5), o.Size)
			assert.True(t, o.PendingFor("dr"))
			assert.True(t, o.PendingFor("logs"))

			require.NoError(t, putIn(ctx, e, "b", "photo", 3))

			for k, o := range outboxOf(t, e) {
				if strings.HasPrefix(k, "photo\x00") {
					assert.Equal(t, []string{"dr"}, o.Targets, "outside the logs target's prefix")
				}
			}

			// A change of tags moves the row on, without asking for the
			// version again.
			require.NoError(t, e.PutObjectTagging(ctx, "b", "logs/1", []fs.Tag{{Key: "k", Value: "v"}}))
			assert.Greater(t, outboxOf(t, e)[sk].Pending, o.Pending)

			// A delete marker, for the target that takes them.
			del, err := e.DeleteObjectVersion(ctx, "b", "logs/1", "")
			require.NoError(t, err)
			require.True(t, del.DeleteMarker)
			assert.Equal(t, []string{"dr"}, outboxOf(t, e)[meta.OutboxSK("logs/1", del.VersionID)].Targets)

			// Neither a version delete nor the lifecycle sweep's marker is
			// replicated.
			before := len(outboxOf(t, e))

			_, err = e.DeleteObjectVersion(ctx, "b", "logs/1", del.VersionID)
			require.NoError(t, err)

			_, err = e.DeleteObjectVersion(fs.WithLifecycleAction(ctx), "b", "photo", "")
			require.NoError(t, err)

			assert.Len(t, outboxOf(t, e), before)

			// A completed multipart upload.
			up, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "big"})
			require.NoError(t, err)

			part, err := e.UploadPart(ctx, &fs.UploadPartRequest{
				Bucket: "b", Key: "big", UploadID: up.UploadID, PartNumber: 1, Reader: strings.NewReader("part"), Size: 4,
			})
			require.NoError(t, err)

			_, err = e.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
				Bucket: "b", Key: "big", UploadID: up.UploadID, Parts: []fs.CompletedPart{{PartNumber: 1, ETag: part.ETag}},
			})
			require.NoError(t, err)

			var found bool

			for k, o := range outboxOf(t, e) {
				if strings.HasPrefix(k, "big\x00") {
					found = true

					assert.True(t, o.Version)
					assert.Equal(t, int64(4), o.Size)
				}
			}

			assert.True(t, found, "completion writes the outbox row")
		})
	}
}

func TestOutboxNotWrittenWithoutTargets(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, 1)
	require.NoError(t, e.SetBucketVersioning(ctx, "b", fs.VersioningEnabled))

	require.NoError(t, putIn(ctx, e, "b", "k", 3))
	require.NoError(t, e.PutObjectTagging(ctx, "b", "k", []fs.Tag{{Key: "k", Value: "v"}}))

	_, err := e.DeleteObjectVersion(ctx, "b", "k", "")
	require.NoError(t, err)

	assert.Empty(t, outboxOf(t, e))
}

func TestReplicationBacklog(t *testing.T) {
	ctx := context.Background()
	e := replicated(t, 3, target("dr"), target("dr2"))

	require.NoError(t, putIn(ctx, e, "b", "one", 10))
	require.NoError(t, putIn(ctx, e, "b", "two", 20))

	u, err := e.ReplicationBacklog(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, Usage{Objects: 2, Bytes: 30}, u)

	all, err := e.ReplicationBacklogs(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]Usage{"b": {Objects: 2, Bytes: 30}}, all)

	_, inc, err := e.bucket(ctx, "b")
	require.NoError(t, err)

	// A version leaves the backlog once every target has it.
	for sk, o := range outboxOf(t, e) {
		if !strings.HasPrefix(sk, "one\x00") {
			continue
		}

		require.NoError(t, e.outbox.Insert(ctx, inc.ID, sk, meta.Outbox{Shipped: map[string]int64{"dr": o.Pending}}))

		u, err = e.ReplicationBacklog(ctx, "b")
		require.NoError(t, err)
		assert.Equal(t, Usage{Objects: 2, Bytes: 30}, u, "one target still to go")

		require.NoError(t, e.outbox.Insert(ctx, inc.ID, sk, meta.Outbox{Shipped: map[string]int64{"dr2": o.Pending}}))
	}

	u, err = e.ReplicationBacklog(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, Usage{Objects: 1, Bytes: 20}, u)
}
