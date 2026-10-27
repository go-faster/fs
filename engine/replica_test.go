package engine

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/meta"
)

// site is a deployment's engine with bucket "b" versioned.
func site(t *testing.T, e *Engine) *Engine {
	t.Helper()

	ctx := context.Background()
	require.NoError(t, e.CreateBucket(ctx, "b"))
	require.NoError(t, e.SetBucketVersioning(ctx, "b", fs.VersioningEnabled))

	return e
}

// ship does what the replication runner does for one version: export it,
// claim its blocks on dst, send the missing ones, apply it. It returns the
// blocks sent and the version's result.
func ship(t *testing.T, ctx context.Context, src, dst *Engine, key, versionID string, attrsOnly bool) (int, fs.ReplicaResult) {
	t.Helper()

	rv, err := src.ExportReplica(ctx, "b", key, versionID, attrsOnly)
	require.NoError(t, err)

	sent := 0

	if len(rv.Blocks) > 0 {
		claims, err := dst.ClaimReplicaBlocks(ctx, "b", rv.VersionID, rv.Blocks)
		require.NoError(t, err)
		require.Len(t, claims, len(rv.Blocks))

		for i, c := range claims {
			if c.Missing {
				data, err := src.ReadReplicaBlock(ctx, rv.Blocks[i])
				require.NoError(t, err)

				require.NoError(t, dst.PutReplicaBlock(ctx, "b", rv.VersionID,
					fs.ReplicaBlock{Hash: c.Hash, Size: rv.Blocks[i].Size, Scheme: c.Scheme}, data))

				sent++
			}

			rv.Blocks[i].Scheme = c.Scheme
		}
	}

	res, err := dst.ApplyReplicas(ctx, "b", []fs.ReplicaVersion{rv})
	require.NoError(t, err)
	require.Len(t, res, 1)

	return sent, res[0]
}

func read(t *testing.T, ctx context.Context, e *Engine, key, versionID string) []byte {
	t.Helper()

	resp, err := e.GetObjectVersion(ctx, "b", key, versionID)
	require.NoError(t, err)

	defer func() { _ = resp.Reader.Close() }()

	data, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)

	return data
}

func versionsOf(t *testing.T, ctx context.Context, e *Engine) []fs.ObjectVersion {
	t.Helper()

	resp, err := e.ListObjectVersions(ctx, &fs.ListObjectVersionsRequest{Bucket: "b", Limit: 1000, Tags: true})
	require.NoError(t, err)

	out := resp.Versions
	for i := range out {
		out[i].IsLatest = false // Compared separately.
	}

	return out
}

func TestReplicaRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := site(t, cluster(t, 3, small)[0].engine)
	dst := site(t, cluster(t, 3, small)[0].engine)

	body := pattern(300) // Five blocks of 64 bytes.

	put1, err := src.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: "b", Key: "k", Reader: bytes.NewReader(body), Size: int64(len(body)),
		Metadata: fs.ObjectMetadata{ContentType: "text/plain", UserMetadata: map[string]string{"a": "b"}},
		Tags:     []fs.Tag{{Key: "t", Value: "1"}},
	})
	require.NoError(t, err)

	put2, err := src.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "k", Reader: strings.NewReader("small"), Size: 5})
	require.NoError(t, err)

	marker, err := src.DeleteObjectVersion(ctx, "b", "gone", "")
	require.NoError(t, err)

	// Out of order: the newer version first.
	for _, id := range []string{put2.VersionID, put1.VersionID} {
		_, res := ship(t, ctx, src, dst, "k", id, false)
		assert.Empty(t, res.Error)
	}

	_, res := ship(t, ctx, src, dst, "gone", marker.VersionID, false)
	assert.Empty(t, res.Error)

	assert.Equal(t, versionsOf(t, ctx, src), versionsOf(t, ctx, dst), "the same history, IDs, times and tags")
	assert.Equal(t, body, read(t, ctx, dst, "k", put1.VersionID))
	assert.Equal(t, []byte("small"), read(t, ctx, dst, "k", put2.VersionID))

	cur, err := dst.GetObject(ctx, "b", "k")
	require.NoError(t, err)
	assert.Equal(t, put2.ETag, cur.ETag, "the newest version is current")
	require.NoError(t, cur.Reader.Close())

	// Again, and nothing changes: no blocks travel, the history is the same.
	sent, res := ship(t, ctx, src, dst, "k", put1.VersionID, false)
	assert.Empty(t, res.Error)
	assert.Zero(t, sent, "every block is already stored")
	assert.Equal(t, versionsOf(t, ctx, src), versionsOf(t, ctx, dst))

	// The replicas wrote no outbox rows, even with targets of their own.
	require.NoError(t, dst.SetBucketReplication(ctx, "b", []ReplicationTarget{target("back")}))

	_, res = ship(t, ctx, src, dst, "k", put1.VersionID, false)
	assert.Empty(t, res.Error)
	assert.Empty(t, outboxOf(t, dst))
}

func TestReplicaDedupe(t *testing.T) {
	ctx := context.Background()
	src := site(t, cluster(t, 1, small)[0].engine)
	dst := site(t, cluster(t, 1, small)[0].engine)

	body := pattern(300)

	put := func(key string) string {
		resp, err := src.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: key, Reader: bytes.NewReader(body), Size: int64(len(body))})
		require.NoError(t, err)

		return resp.VersionID
	}

	sent, res := ship(t, ctx, src, dst, "one", put("one"), false)
	assert.Empty(t, res.Error)
	assert.Equal(t, 5, sent)

	sent, res = ship(t, ctx, src, dst, "copy", put("copy"), false)
	assert.Empty(t, res.Error)
	assert.Zero(t, sent, "a copy's blocks are already there")
	assert.Equal(t, body, read(t, ctx, dst, "copy", versionsOf(t, ctx, dst)[0].VersionID))
}

func TestReplicaDeletedStaysDeleted(t *testing.T) {
	ctx := context.Background()
	src := site(t, cluster(t, 1, small)[0].engine)
	dst := site(t, cluster(t, 1, small)[0].engine)

	put, err := src.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "k", Reader: bytes.NewReader(pattern(200)), Size: 200})
	require.NoError(t, err)

	_, res := ship(t, ctx, src, dst, "k", put.VersionID, false)
	require.Empty(t, res.Error)

	_, err = dst.DeleteObjectVersion(ctx, "b", "k", put.VersionID)
	require.NoError(t, err)

	_, res = ship(t, ctx, src, dst, "k", put.VersionID, false)
	assert.Empty(t, res.Error)
	assert.Empty(t, versionsOf(t, ctx, dst), "a version deleted on the target is not brought back")

	// Nor by a change of its tags.
	require.NoError(t, src.PutObjectTagging(ctx, "b", "k", []fs.Tag{{Key: "t", Value: "2"}}))

	_, res = ship(t, ctx, src, dst, "k", put.VersionID, true)
	assert.Empty(t, res.Error)
	assert.Empty(t, versionsOf(t, ctx, dst))
}

func TestReplicaAttrs(t *testing.T) {
	ctx := context.Background()
	src := site(t, cluster(t, 1, small)[0].engine)
	dst := site(t, cluster(t, 1, small)[0].engine)

	put, err := src.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "k", Reader: strings.NewReader("x"), Size: 1})
	require.NoError(t, err)

	// An attributes change of a version the target lacks creates nothing.
	_, res := ship(t, ctx, src, dst, "k", put.VersionID, true)
	assert.Empty(t, res.Error)
	assert.Empty(t, versionsOf(t, ctx, dst))

	_, res = ship(t, ctx, src, dst, "k", put.VersionID, false)
	require.Empty(t, res.Error)

	tags := []fs.Tag{{Key: "t", Value: "2"}}
	require.NoError(t, src.PutObjectTagging(ctx, "b", "k", tags))

	_, res = ship(t, ctx, src, dst, "k", put.VersionID, true)
	assert.Empty(t, res.Error)

	got, err := dst.GetObjectTagging(ctx, "b", "k")
	require.NoError(t, err)
	assert.Equal(t, tags, got)

	// A later change made on the target wins over an older one shipped late.
	mine := []fs.Tag{{Key: "t", Value: "target"}}
	require.NoError(t, dst.PutObjectTagging(ctx, "b", "k", mine))

	_, res = ship(t, ctx, src, dst, "k", put.VersionID, true)
	assert.Empty(t, res.Error)

	got, err = dst.GetObjectTagging(ctx, "b", "k")
	require.NoError(t, err)
	assert.Equal(t, mine, got)
}

func TestReplicaEncrypted(t *testing.T) {
	ctx := context.Background()
	src := site(t, cluster(t, 1, Config{BlockSize: 4096, InlineLimit: 256, Keyring: keyring(t, masterKey(t))})[0].engine)

	// The target's master key is its own.
	dst := cluster(t, 1, Config{BlockSize: 4096, InlineLimit: 256, Keyring: keyring(t, masterKey(t))})[0].engine
	site(t, dst)

	body := pattern(20000)

	s3, err := src.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: "b", Key: "s3", Reader: bytes.NewReader(body), Size: int64(len(body)), ServerSideEncryption: "AES256",
	})
	require.NoError(t, err)

	ck := fs.CustomerKey(bytes.Repeat([]byte{7}, 32))
	withKey := fs.WithCustomerKey(ctx, ck)

	c, err := src.PutObject(withKey, &fs.PutObjectRequest{Bucket: "b", Key: "c", Reader: bytes.NewReader(body), Size: int64(len(body))})
	require.NoError(t, err)

	_, res := ship(t, ctx, src, dst, "s3", s3.VersionID, false)
	require.Empty(t, res.Error)

	_, res = ship(t, ctx, src, dst, "c", c.VersionID, false)
	require.Empty(t, res.Error)

	assert.Equal(t, body, read(t, ctx, dst, "s3", s3.VersionID))
	assert.Equal(t, body, read(t, withKey, dst, "c", c.VersionID), "readable with the client's key")

	_, err = dst.GetObjectVersion(ctx, "b", "c", c.VersionID)
	require.ErrorIs(t, err, fs.ErrCustomerKeyMismatch, "and only with it")

	// A target without a master key refuses an SSE-S3 version, and only it.
	bare := site(t, cluster(t, 1, Config{BlockSize: 4096, InlineLimit: 256})[0].engine)

	_, res = ship(t, ctx, src, bare, "s3", s3.VersionID, false)
	assert.Contains(t, res.Error, "no master key")

	_, res = ship(t, ctx, src, bare, "c", c.VersionID, false)
	assert.Empty(t, res.Error)
}

func TestReplicaCodedTarget(t *testing.T) {
	ctx := context.Background()
	src := site(t, cluster(t, 1, Config{BlockSize: 4096, InlineLimit: 64})[0].engine)
	dst := site(t, clusterWide(t, 3, Config{BlockSize: 4096, InlineLimit: 64, CodedMinSize: 1024}, 3)[0].engine)
	require.NoError(t, dst.SetBucketScheme(ctx, "b", "ec:2,1"))

	body := pattern(10000)

	put, err := src.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "k", Reader: bytes.NewReader(body), Size: int64(len(body))})
	require.NoError(t, err)

	_, res := ship(t, ctx, src, dst, "k", put.VersionID, false)
	require.Empty(t, res.Error)
	assert.Equal(t, body, read(t, ctx, dst, "k", put.VersionID))

	_, p, err := dst.current(ctx, "b", "k")
	require.NoError(t, err)

	for _, b := range p.Blocks {
		if b.Size >= 1024 {
			assert.Equal(t, "ec:2,1", b.Scheme, "stored as the target's bucket stores")
		}
	}
}

func TestReplicaRefused(t *testing.T) {
	ctx := context.Background()
	src := site(t, cluster(t, 1, small)[0].engine)
	dst := site(t, cluster(t, 1, small)[0].engine)

	put, err := src.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "k", Reader: bytes.NewReader(pattern(200)), Size: 200})
	require.NoError(t, err)

	rv, err := src.ExportReplica(ctx, "b", "k", put.VersionID, false)
	require.NoError(t, err)

	// Blocks never sent: refused, and the other version of the request is
	// applied all the same.
	marker, err := src.DeleteObjectVersion(ctx, "b", "m", "")
	require.NoError(t, err)

	mv, err := src.ExportReplica(ctx, "b", "m", marker.VersionID, false)
	require.NoError(t, err)

	res, err := dst.ApplyReplicas(ctx, "b", []fs.ReplicaVersion{rv, mv})
	require.NoError(t, err)
	assert.Contains(t, res[0].Error, "not stored")
	assert.Empty(t, res[1].Error)
	assert.Equal(t, ReplicaStats{Applied: 1, Refused: 1}, dst.ReplicaStats())

	_, err = src.ExportReplica(ctx, "b", "k", "0123456789abcdef0123456789abcdef", false)
	require.ErrorIs(t, err, fs.ErrVersionNotFound)

	// A target that is not versioned refuses everything.
	plain := cluster(t, 1, small)[0].engine
	require.NoError(t, plain.CreateBucket(ctx, "b"))

	_, err = plain.ApplyReplicas(ctx, "b", []fs.ReplicaVersion{mv})
	require.ErrorIs(t, err, fs.ErrInvalidBucketState)

	info, err := plain.ReplicaInfo(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, fs.ReplicaInfo{Protocol: fs.ReplicaProtocol, Versioning: fs.VersioningUnset}, info)
}

func TestReplicationStatus(t *testing.T) {
	ctx := context.Background()
	src := replicated(t, 3, target("dr"), ReplicationTarget{
		Name: "logs", Endpoint: "https://s3.example.com", Bucket: "replica",
		AccessKeyID: "AK", SecretAccessKey: "secret", Prefix: "logs/",
	})
	dst := site(t, cluster(t, 1, small)[0].engine)

	status := func(e *Engine, key, versionID string) string {
		t.Helper()

		resp, err := e.GetObjectVersion(ctx, "b", key, versionID)
		require.NoError(t, err)
		require.NoError(t, resp.Reader.Close())

		cur, err := e.GetObject(ctx, "b", key)
		require.NoError(t, err)
		require.NoError(t, cur.Reader.Close())
		assert.Equal(t, resp.ReplicationStatus, cur.ReplicationStatus, "the current version, either way")

		return resp.ReplicationStatus
	}

	put, err := src.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "logs/1", Reader: strings.NewReader("x"), Size: 1})
	require.NoError(t, err)

	assert.Equal(t, fs.ReplicationStatusPending, status(src, "logs/1", put.VersionID))

	mark := func(target, refused string) {
		t.Helper()

		o := outboxOf(t, src)[meta.OutboxSK("logs/1", put.VersionID)]
		require.NoError(t, src.MarkReplicated(ctx, "b", []ReplicationMark{{
			Key: "logs/1", VersionID: put.VersionID, Target: target, Pending: o.Pending, Refused: refused,
		}}))
	}

	// One target has it; the other refused it: the failure shows.
	mark("dr", "")
	assert.Equal(t, fs.ReplicationStatusPending, status(src, "logs/1", put.VersionID))

	mark("logs", "no master key")
	assert.Equal(t, fs.ReplicationStatusFailed, status(src, "logs/1", put.VersionID))

	mark("logs", "")
	assert.Equal(t, fs.ReplicationStatusCompleted, status(src, "logs/1", put.VersionID))

	// The version that arrives elsewhere is a replica there.
	_, res := ship(t, ctx, src, dst, "logs/1", put.VersionID, false)
	require.Empty(t, res.Error)
	assert.Equal(t, fs.ReplicationStatusReplica, status(dst, "logs/1", put.VersionID))

	// A key no target takes, and a bucket without targets, have none.
	other, err := src.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "x", Reader: strings.NewReader("x"), Size: 1})
	require.NoError(t, err)
	assert.Equal(t, fs.ReplicationStatusPending, status(src, "x", other.VersionID), "dr takes every key")

	require.NoError(t, src.SetBucketReplication(ctx, "b", []ReplicationTarget{{
		Name: "logs", Endpoint: "https://s3.example.com", Bucket: "replica",
		AccessKeyID: "AK", SecretAccessKey: "secret", Prefix: "logs/",
	}}))
	assert.Empty(t, status(src, "x", other.VersionID), "outside every prefix")

	require.NoError(t, src.SetBucketReplication(ctx, "b", nil))
	assert.Empty(t, status(src, "logs/1", put.VersionID))

	// A version stored before its target was set is pending until the
	// backfill has walked and shipped it.
	old, err := src.PutObject(ctx, &fs.PutObjectRequest{Bucket: "b", Key: "old", Reader: strings.NewReader("x"), Size: 1})
	require.NoError(t, err)
	require.NoError(t, src.SetBucketReplication(ctx, "b", []ReplicationTarget{target("late")}))
	assert.Equal(t, fs.ReplicationStatusPending, status(src, "old", old.VersionID))
}
