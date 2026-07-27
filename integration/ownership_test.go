package integration

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagefs"
)

// ownershipConfig gives three principals the shapes that matter: the bucket's
// creator, a stranger holding the blanket wildcard grant an operator writes to
// mean "my buckets", and a guest whose grant names one bucket.
func ownershipConfig() auth.Config {
	return auth.Config{
		Keys: []auth.Key{
			{
				AccessKey: "OWNERKEYOWNERKEYOWNE", SecretKey: "owner-secret-owner-secret-owner-secret00",
				UserID: "owner", DisplayName: "owner",
				Grants: []auth.Grant{{Pattern: "*", Permission: auth.Admin}},
			},
			{
				AccessKey: "STRANGERSTRANGERSTRA", SecretKey: "stranger-secret-stranger-secret-strange0",
				UserID: "stranger", DisplayName: "stranger",
				Grants: []auth.Grant{{Pattern: "*", Permission: auth.Admin}},
			},
			{
				AccessKey: "GUESTKEYGUESTKEYGUES", SecretKey: "guest-secret-guest-secret-guest-secret00",
				UserID: "guest", DisplayName: "guest",
				Grants: []auth.Grant{{Pattern: "shared", Permission: auth.Admin}},
			},
		},
	}
}

// newOwnershipServer starts a server with ownership enforcement configurable.
func newOwnershipServer(t testing.TB, isolation bool) string {
	t.Helper()

	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	store, err := auth.NewStore(ownershipConfig())
	require.NoError(t, err)

	opts := []server.HandlerOption{server.WithAuth(store)}
	if isolation {
		opts = append(opts, server.WithOwnerIsolation(true))
	}

	srv := httptest.NewServer(server.NewHandler(storage, opts...))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	return u.Host
}

// TestBucketOwnership_RecreateIsIdempotent covers the create rules: repeating a
// create you own succeeds (a retried create must not fail), while the same call
// from another principal reports the name as taken.
func TestBucketOwnership_RecreateIsIdempotent(t *testing.T) {
	t.Parallel()

	endpoint := newOwnershipServer(t, false)
	ctx := t.Context()

	owner := minioClient(t, endpoint, "OWNERKEYOWNERKEYOWNE", "owner-secret-owner-secret-owner-secret00")
	stranger := minioClient(t, endpoint, "STRANGERSTRANGERSTRA", "stranger-secret-stranger-secret-strange0")

	require.NoError(t, owner.MakeBucket(ctx, "mine", minio.MakeBucketOptions{}))

	// The owner repeating itself is not an error.
	require.NoError(t, owner.MakeBucket(ctx, "mine", minio.MakeBucketOptions{}))

	// Someone else gets told the name is unavailable.
	err := stranger.MakeBucket(ctx, "mine", minio.MakeBucketOptions{})
	require.Error(t, err)
	require.Equal(t, "BucketAlreadyExists", minio.ToErrorResponse(err).Code)
}

// TestBucketOwnership_Isolation covers the gate: with isolation on, a wildcard
// grant does not reach a bucket its holder does not own, while a grant naming
// the bucket does.
func TestBucketOwnership_Isolation(t *testing.T) {
	t.Parallel()

	endpoint := newOwnershipServer(t, true)
	ctx := t.Context()

	owner := minioClient(t, endpoint, "OWNERKEYOWNERKEYOWNE", "owner-secret-owner-secret-owner-secret00")
	stranger := minioClient(t, endpoint, "STRANGERSTRANGERSTRA", "stranger-secret-stranger-secret-strange0")
	guest := minioClient(t, endpoint, "GUESTKEYGUESTKEYGUES", "guest-secret-guest-secret-guest-secret00")

	require.NoError(t, owner.MakeBucket(ctx, "mine", minio.MakeBucketOptions{}))
	require.NoError(t, owner.MakeBucket(ctx, "shared", minio.MakeBucketOptions{}))

	// The owner reaches its own buckets.
	_, err := owner.ListObjects(ctx, "mine", minio.ListObjectsOptions{}), error(nil)
	require.NoError(t, err)

	// A wildcard grant is not a claim on someone else's bucket.
	_, err = stranger.StatObject(ctx, "mine", "whatever", minio.StatObjectOptions{})
	require.Equal(t, "AccessDenied", minio.ToErrorResponse(err).Code)

	// A grant that names the bucket still reaches it.
	_, err = guest.StatObject(ctx, "shared", "whatever", minio.StatObjectOptions{})
	require.Equal(t, "NoSuchKey", minio.ToErrorResponse(err).Code)
}

// TestBucketOwnership_IsolationOffByDefault pins the default: ownership is
// recorded, but a wildcard grant keeps working, so an upgrade changes nothing
// for a deployment that has not opted in.
func TestBucketOwnership_IsolationOffByDefault(t *testing.T) {
	t.Parallel()

	endpoint := newOwnershipServer(t, false)
	ctx := t.Context()

	owner := minioClient(t, endpoint, "OWNERKEYOWNERKEYOWNE", "owner-secret-owner-secret-owner-secret00")
	stranger := minioClient(t, endpoint, "STRANGERSTRANGERSTRA", "stranger-secret-stranger-secret-strange0")

	require.NoError(t, owner.MakeBucket(ctx, "mine", minio.MakeBucketOptions{}))

	_, err := stranger.StatObject(ctx, "mine", "whatever", minio.StatObjectOptions{})
	require.Equal(t, "NoSuchKey", minio.ToErrorResponse(err).Code)
}
