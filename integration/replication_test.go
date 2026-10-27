package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
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
