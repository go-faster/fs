package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/internal/replication"
	"github.com/go-faster/fs/server"
)

// replicationSite is a deployment for the replication tests: its engine, for
// the source side the runner will take, and its S3 endpoint.
func replicationSite(t *testing.T, cfg auth.Config) (storage *engine.Engine, host string) {
	t.Helper()

	var err error

	storage, err = engine.Open(t.TempDir(), engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = storage.Close() })

	store, err := auth.NewStore(cfg)
	require.NoError(t, err)

	srv := httptest.NewServer(server.NewHandler(storage, server.WithAuth(store)))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	return storage, u.Host
}

func versionedBucket(t *testing.T, ctx context.Context, c *awss3.Client, bucket string) {
	t.Helper()

	_, err := c.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)

	_, err = c.PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	})
	require.NoError(t, err)
}

// shipVersion is one version through the protocol, as the runner sends it.
func shipVersion(t *testing.T, ctx context.Context, src *engine.Engine, c *replication.Client, bucket, key, versionID string) {
	t.Helper()

	rv, err := src.ExportReplica(ctx, bucket, key, versionID, false)
	require.NoError(t, err)

	if len(rv.Blocks) > 0 {
		claims, err := c.Claim(ctx, rv.VersionID, rv.Blocks)
		require.NoError(t, err)

		for i, cl := range claims {
			if cl.Missing {
				data, err := src.ReadReplicaBlock(ctx, rv.Blocks[i])
				require.NoError(t, err)

				require.NoError(t, c.PutBlock(ctx, rv.VersionID, fs.ReplicaBlock{Hash: cl.Hash, Scheme: cl.Scheme}, data))
			}

			rv.Blocks[i].Scheme = cl.Scheme
		}
	}

	res, err := c.Apply(ctx, []fs.ReplicaVersion{rv})
	require.NoError(t, err)
	require.Empty(t, res[0].Error)
}

func TestReplicationProtocol(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	const writer = "AKIAWRITERONLY000000"

	cfg := adminConfig()
	cfg.Keys = append(cfg.Keys, auth.Key{
		AccessKey: writer, SecretKey: authSecretKey,
		Grants: []auth.Grant{{Pattern: "*", Permission: auth.Write}},
	})

	srcEngine, srcHost := replicationSite(t, cfg)
	_, dstHost := replicationSite(t, cfg)

	src, dst := awsClient(t, srcHost), awsClient(t, dstHost)
	versionedBucket(t, ctx, src, "photos")
	versionedBucket(t, ctx, dst, "replica")

	client, err := replication.NewClient("http://"+dstHost, "replica", authAccessKey, authSecretKey, nil)
	require.NoError(t, err)

	info, err := client.Info(ctx)
	require.NoError(t, err)
	assert.Equal(t, fs.ReplicaInfo{Protocol: fs.ReplicaProtocol, Versioning: fs.VersioningEnabled}, info)

	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<17) // 2 MiB: blocks.
	bodies := map[string][]byte{"big": big, "small": []byte("hello")}

	var ids []string

	for _, key := range []string{"big", "small"} {
		out, err := src.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String("photos"), Key: aws.String(key), Body: bytes.NewReader(bodies[key]),
			ContentType: aws.String("text/plain"), Metadata: map[string]string{"site": "a"},
			Tagging: aws.String("k=v"),
		})
		require.NoError(t, err)

		shipVersion(t, ctx, srcEngine, client, "photos", key, aws.ToString(out.VersionId))
		ids = append(ids, aws.ToString(out.VersionId))
	}

	for i, key := range []string{"big", "small"} {
		got, err := dst.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("replica"), Key: aws.String(key)})
		require.NoError(t, err)

		data, err := io.ReadAll(got.Body)
		require.NoError(t, err)
		require.NoError(t, got.Body.Close())

		want, err := src.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String("photos"), Key: aws.String(key)})
		require.NoError(t, err)

		assert.Equal(t, bodies[key], data)
		assert.Equal(t, ids[i], aws.ToString(got.VersionId), "the version ID travels")
		assert.Equal(t, aws.ToString(want.ETag), aws.ToString(got.ETag))
		assert.Equal(t, aws.ToTime(want.LastModified), aws.ToTime(got.LastModified))
		assert.Equal(t, "text/plain", aws.ToString(got.ContentType))
		assert.Equal(t, map[string]string{"site": "a"}, got.Metadata)
		assert.Equal(t, aws.Int32(1), got.TagCount)
	}

	// Only the admin grant may replicate: a writer is refused, and so is
	// anyone unsigned.
	weak, err := replication.NewClient("http://"+dstHost, "replica", writer, authSecretKey, nil)
	require.NoError(t, err)

	_, err = weak.Info(ctx)

	var refused *replication.Error
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, http.StatusForbidden, refused.Status)
	assert.Equal(t, "AccessDenied", refused.Code)

	resp, err := http.Post("http://"+dstHost+"/replica?x-fs-replica=versions", "multipart/form-data; boundary=x", bytes.NewReader(nil))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a form post does not skip the signature check")

	// A block that does not hash to its name is refused.
	err = client.PutBlock(ctx, ids[0], fs.ReplicaBlock{Hash: "00" + string(bytes.Repeat([]byte("ab"), 31))}, []byte("x"))
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, "BadDigest", refused.Code)

	// An unversioned target bucket answers, and refuses versions.
	_, err = dst.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String("plain")})
	require.NoError(t, err)

	plain, err := replication.NewClient("http://"+dstHost, "plain", authAccessKey, authSecretKey, nil)
	require.NoError(t, err)

	info, err = plain.Info(ctx)
	require.NoError(t, err)
	assert.Equal(t, fs.VersioningUnset, info.Versioning)

	_, err = plain.Apply(ctx, nil)
	require.True(t, errors.As(err, &refused))
	assert.Equal(t, "InvalidBucketState", refused.Code)
}

// TestReplicationRunner writes through both SDKs at the source and lets the
// runner ship it: versions, IDs, metadata and tags arrive, a delete marker
// too, and a version deleted at the source before shipping does not.
func TestReplicationRunner(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	srcEngine, srcHost := replicationSite(t, adminConfig())
	_, dstHost := replicationSite(t, adminConfig())

	src, dst := awsClient(t, srcHost), awsClient(t, dstHost)
	versionedBucket(t, ctx, src, "photos")
	versionedBucket(t, ctx, dst, "replica")

	require.NoError(t, srcEngine.SetBucketReplication(ctx, "photos", []engine.ReplicationTarget{{
		Name: "dr", Endpoint: "http://" + dstHost, Bucket: "replica",
		AccessKeyID: authAccessKey, SecretAccessKey: authSecretKey,
		DeleteMarkers: true, Insecure: true,
	}}))

	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<17)

	_, err := src.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String("photos"), Key: aws.String("big"), Body: bytes.NewReader(big),
		Metadata: map[string]string{"site": "a"}, Tagging: aws.String("k=v"),
	})
	require.NoError(t, err)

	mc := minioClient(t, srcHost, authAccessKey, authSecretKey)
	_, err = mc.PutObject(ctx, "photos", "small", bytes.NewReader([]byte("hello")), 5, minio.PutObjectOptions{})
	require.NoError(t, err)

	_, err = src.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("small")})
	require.NoError(t, err)

	brief, err := src.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String("photos"), Key: aws.String("brief"), Body: bytes.NewReader([]byte("x")),
	})
	require.NoError(t, err)

	_, err = src.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String("photos"), Key: aws.String("brief"), VersionId: brief.VersionId,
	})
	require.NoError(t, err)

	head := func(c *awss3.Client, bucket string) types.ReplicationStatus {
		out, err := c.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("big")})
		require.NoError(t, err)

		return out.ReplicationStatus
	}

	assert.Equal(t, types.ReplicationStatusPending, head(src, "photos"))

	r := &replication.Runner{Source: srcEngine}
	_, err = r.Pass(ctx)
	require.NoError(t, err)

	assert.Equal(t, types.ReplicationStatusCompleted, head(src, "photos"))
	assert.Equal(t, types.ReplicationStatusReplica, head(dst, "replica"))

	// minio-go reads it from GET as well.
	st, err := mc.StatObject(ctx, "photos", "big", minio.StatObjectOptions{})
	require.NoError(t, err)
	assert.Equal(t, "COMPLETED", st.ReplicationStatus)

	obj, err := minioClient(t, dstHost, authAccessKey, authSecretKey).GetObject(ctx, "replica", "big", minio.GetObjectOptions{})
	require.NoError(t, err)

	info, err := obj.Stat()
	require.NoError(t, err)
	require.NoError(t, obj.Close())
	assert.Equal(t, "REPLICA", info.ReplicationStatus)

	list := func(c *awss3.Client, bucket string) []string {
		out, err := c.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
		require.NoError(t, err)

		var ids []string
		for _, v := range out.Versions {
			ids = append(ids, aws.ToString(v.Key)+"@"+aws.ToString(v.VersionId))
		}

		for _, m := range out.DeleteMarkers {
			ids = append(ids, aws.ToString(m.Key)+"@"+aws.ToString(m.VersionId)+" marker")
		}

		return ids
	}

	assert.ElementsMatch(t, list(src, "photos"), list(dst, "replica"))

	got, err := dst.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("replica"), Key: aws.String("big")})
	require.NoError(t, err)

	data, err := io.ReadAll(got.Body)
	require.NoError(t, err)
	require.NoError(t, got.Body.Close())

	assert.Equal(t, big, data)
	assert.Equal(t, map[string]string{"site": "a"}, got.Metadata)
	assert.Equal(t, aws.Int32(1), got.TagCount)

	backlog, err := srcEngine.ReplicationBacklog(ctx, "photos")
	require.NoError(t, err)
	assert.Zero(t, backlog.Objects)
}

// TestReplicationBackfill sets a target on a bucket that already holds
// versions: the backfill ships them.
func TestReplicationBackfill(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	srcEngine, srcHost := replicationSite(t, adminConfig())
	_, dstHost := replicationSite(t, adminConfig())

	src, dst := awsClient(t, srcHost), awsClient(t, dstHost)
	versionedBucket(t, ctx, src, "photos")
	versionedBucket(t, ctx, dst, "replica")

	for i, body := range []string{"one", "two"} {
		_, err := src.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String("photos"), Key: aws.String("k"), Body: strings.NewReader(body),
			Metadata: map[string]string{"n": strconv.Itoa(i)},
		})
		require.NoError(t, err)
	}

	require.NoError(t, srcEngine.SetBucketReplication(ctx, "photos", []engine.ReplicationTarget{{
		Name: "dr", Endpoint: "http://" + dstHost, Bucket: "replica",
		AccessKeyID: authAccessKey, SecretAccessKey: authSecretKey, Insecure: true,
	}}))

	r := &replication.Runner{Source: srcEngine, BackfillDelay: time.Nanosecond}

	// The walk, then the shipment. The walk starts once its delay has
	// passed by the wall clock, which ticks coarsely on some systems.
	require.Eventually(t, func() bool {
		_, err := r.Pass(ctx)
		require.NoError(t, err)

		bfs, err := srcEngine.ReplicationBackfills(ctx, "photos")
		require.NoError(t, err)

		backlog, err := srcEngine.ReplicationBacklog(ctx, "photos")
		require.NoError(t, err)

		return !bfs["dr"].Due() && backlog.Objects == 0
	}, 5*time.Second, 5*time.Millisecond)

	want, err := src.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String("photos")})
	require.NoError(t, err)

	got, err := dst.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String("replica")})
	require.NoError(t, err)

	require.Len(t, got.Versions, 2)

	for i := range want.Versions {
		assert.Equal(t, aws.ToString(want.Versions[i].VersionId), aws.ToString(got.Versions[i].VersionId))
		assert.Equal(t, aws.ToString(want.Versions[i].ETag), aws.ToString(got.Versions[i].ETag))
		assert.Equal(t, aws.ToBool(want.Versions[i].IsLatest), aws.ToBool(got.Versions[i].IsLatest))
	}
}
