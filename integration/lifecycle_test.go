package integration

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/engine"
	sweep "github.com/go-faster/fs/internal/lifecycle"
)

// TestAWS_NoncurrentExpiration sets the version rules through aws-sdk-go-v2,
// reads them back, and has a sweep on a moved clock enforce them on what the
// SDK then lists.
func TestAWS_NoncurrentExpiration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	storage, err := engine.Open(t.TempDir(), engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = storage.Close() })

	srv := newTestServer(t, storage)
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
	})

	const bucket = "versions"

	require.NoError(t, createBucket(ctx, client, bucket))

	_, err = client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	})
	require.NoError(t, err)

	_, err = client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(bucket),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{
			{
				ID:     aws.String("history"),
				Status: types.ExpirationStatusEnabled,
				Filter: &types.LifecycleRuleFilter{Prefix: aws.String("docs/")},
				NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{
					NoncurrentDays:          aws.Int32(1),
					NewerNoncurrentVersions: aws.Int32(1),
				},
			},
			{
				ID:         aws.String("markers"),
				Status:     types.ExpirationStatusEnabled,
				Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("tmp/")},
				Expiration: &types.LifecycleExpiration{ExpiredObjectDeleteMarker: aws.Bool(true)},
				NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{
					NoncurrentDays: aws.Int32(1),
				},
			},
		}},
	})
	require.NoError(t, err)

	got, err := client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, got.Rules, 2)
	require.Equal(t, int32(1), aws.ToInt32(got.Rules[0].NoncurrentVersionExpiration.NoncurrentDays))
	require.Equal(t, int32(1), aws.ToInt32(got.Rules[0].NoncurrentVersionExpiration.NewerNoncurrentVersions))
	require.True(t, aws.ToBool(got.Rules[1].Expiration.ExpiredObjectDeleteMarker))

	put := func(key, body string) {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader([]byte(body)),
		})
		require.NoError(t, err)
	}

	put("docs/a", "v1")
	put("docs/a", "v2")
	put("docs/a", "v3")
	put("tmp/b", "v1")

	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("tmp/b")})
	require.NoError(t, err)

	sweeper := &sweep.Sweeper{Storage: storage, Now: func() time.Time { return time.Now().Add(2 * fs.LifecycleDay) }}
	report, err := sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, report.Noncurrent, "docs/a v1, tmp/b v1")
	require.Equal(t, 1, report.Markers, "tmp/b's marker, nothing left under it")

	list, err := client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Empty(t, list.DeleteMarkers)
	require.Len(t, list.Versions, 2, "docs/a v3 and the newest noncurrent, v2")

	for _, v := range list.Versions {
		require.Equal(t, "docs/a", aws.ToString(v.Key))
	}
}

// TestMinio_NoncurrentExpiration round-trips the version rules through
// minio-go, which spells them its own way.
func TestMinio_NoncurrentExpiration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	storage, err := engine.Open(t.TempDir(), engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = storage.Close() })

	client := NewClient(t, newTestServer(t, storage))

	const bucket = "versions"

	require.NoError(t, client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))

	cfg := lifecycle.NewConfiguration()
	cfg.Rules = []lifecycle.Rule{
		{
			ID:         "history",
			Status:     "Enabled",
			RuleFilter: lifecycle.Filter{Prefix: "docs/"},
			NoncurrentVersionExpiration: lifecycle.NoncurrentVersionExpiration{
				NoncurrentDays:          30,
				NewerNoncurrentVersions: 5,
			},
		},
		{
			ID:         "markers",
			Status:     "Enabled",
			RuleFilter: lifecycle.Filter{Prefix: "tmp/"},
			Expiration: lifecycle.Expiration{DeleteMarker: true},
		},
	}

	require.NoError(t, client.SetBucketLifecycle(ctx, bucket, cfg))

	got, err := client.GetBucketLifecycle(ctx, bucket)
	require.NoError(t, err)
	require.Len(t, got.Rules, 2)
	require.Equal(t, lifecycle.ExpirationDays(30), got.Rules[0].NoncurrentVersionExpiration.NoncurrentDays)
	require.Equal(t, 5, got.Rules[0].NoncurrentVersionExpiration.NewerNoncurrentVersions)
	require.True(t, got.Rules[1].Expiration.DeleteMarker.IsEnabled())
}
