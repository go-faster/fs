package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap/zaptest"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// testEtcdClient dials the in-process etcd started by startTestEtcd.
func testEtcdClient(t *testing.T, endpoint string) *clientv3.Client {
	t.Helper()

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{endpoint},
		DialTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	return client
}

const testClusterSecret = "0123456789abcdef0123456789abcdef"

// TestClusterCredentialsPropagate is the §6.8 acceptance for runtime key
// management: a key created on one node's credential store propagates to every
// other node's live auth store with no restart, and so does a deletion.
func TestClusterCredentialsPropagate(t *testing.T) {
	endpoint := startTestEtcd(t)
	client := testEtcdClient(t, endpoint)
	lg := zaptest.NewLogger(t)
	ctx := t.Context()

	etcdCfg := etcd.Config{Prefix: "/fs-auth-prop"}

	// Two independent nodes sharing the control plane: each has its own watch
	// and its own live auth store.
	nodeA, err := newClusterCredentials(ctx, lg, client, etcdCfg, testClusterSecret, nil, nil)
	require.NoError(t, err)

	defer func() { _ = nodeA.Close() }()

	nodeB, err := newClusterCredentials(ctx, lg, client, etcdCfg, testClusterSecret, nil, nil)
	require.NoError(t, err)

	defer func() { _ = nodeB.Close() }()

	// Create on A.
	created, err := nodeA.Create(auth.CreateInput{
		AccessKey: "AKIAPROPAGATE",
		SecretKey: "top-secret-value",
		Grants:    []auth.Grant{{Pattern: "*", Permission: auth.Admin}},
	})
	require.NoError(t, err)
	require.Equal(t, "AKIAPROPAGATE", created.AccessKey)
	require.Equal(t, "top-secret-value", created.SecretKey)

	// The secret becomes resolvable on both nodes' live stores (SigV4 path).
	for _, node := range []*clusterCredentials{nodeA, nodeB} {
		require.Eventually(t, func() bool {
			secret, ok := node.Store().Secret("AKIAPROPAGATE")

			return ok && secret == "top-secret-value"
		}, 10*time.Second, 20*time.Millisecond, "key must propagate to the live store")

		// And the grant is authorized.
		assert.True(t, node.Store().Allow("AKIAPROPAGATE", "any-bucket", auth.ActionWrite))
	}

	// B lists the key as managed, without the secret.
	require.Eventually(t, func() bool {
		infos := nodeB.List()

		return len(infos) == 1 && infos[0].AccessKey == "AKIAPROPAGATE" && infos[0].Source == auth.SourceManaged
	}, 10*time.Second, 20*time.Millisecond)

	// The stored secret is sealed at rest: the raw etcd value must not contain
	// the plaintext.
	resp, err := client.Get(ctx, etcdCfg.Prefix+"/auth/keys/AKIAPROPAGATE")
	require.NoError(t, err)
	require.Len(t, resp.Kvs, 1)
	assert.NotContains(t, string(resp.Kvs[0].Value), "top-secret-value", "secret must be sealed at rest")

	// Delete on B propagates to A too.
	require.NoError(t, nodeB.Delete("AKIAPROPAGATE"))

	require.Eventually(t, func() bool {
		_, ok := nodeA.Store().Secret("AKIAPROPAGATE")

		return !ok
	}, 10*time.Second, 20*time.Millisecond, "deletion must propagate to the live store")
}

// TestClusterPublicReadPropagates verifies public-read buckets are cluster-wide
// state: seeded from one node's config and hot-reloaded onto every node.
func TestClusterPublicReadPropagates(t *testing.T) {
	endpoint := startTestEtcd(t)
	client := testEtcdClient(t, endpoint)
	lg := zaptest.NewLogger(t)
	ctx := t.Context()

	etcdCfg := etcd.Config{Prefix: "/fs-public-read"}

	// nodeA seeds public-read from its config bootstrap.
	nodeA, err := newClusterCredentials(ctx, lg, client, etcdCfg, testClusterSecret, []string{"assets"}, nil)
	require.NoError(t, err)

	defer func() { _ = nodeA.Close() }()

	// nodeB has no bootstrap, yet learns the seeded set from etcd.
	nodeB, err := newClusterCredentials(ctx, lg, client, etcdCfg, testClusterSecret, nil, nil)
	require.NoError(t, err)

	defer func() { _ = nodeB.Close() }()

	for _, node := range []*clusterCredentials{nodeA, nodeB} {
		require.Eventually(t, func() bool {
			return node.Store().PublicRead("assets")
		}, 10*time.Second, 20*time.Millisecond, "seeded public-read must reach every node")
	}

	// Updating the list through the admin store (SetPublicReadBuckets) on nodeA
	// propagates to both live stores.
	require.NoError(t, nodeA.SetPublicReadBuckets(ctx, []string{"downloads"}))

	// Read-your-writes on the managing node: the admin read reflects it at once.
	got, err := nodeA.PublicReadBuckets(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"downloads"}, got)

	for _, node := range []*clusterCredentials{nodeA, nodeB} {
		require.Eventually(t, func() bool {
			return node.Store().PublicRead("downloads") && !node.Store().PublicRead("assets")
		}, 10*time.Second, 20*time.Millisecond, "public-read update must propagate")
	}

	// An invalid bucket name is rejected without touching the stored list.
	err = nodeA.SetPublicReadBuckets(ctx, []string{"Invalid_Name"})
	require.ErrorIs(t, err, adminhandler.ErrPublicReadRejected)

	got, err = nodeA.PublicReadBuckets(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"downloads"}, got)
}

func TestClusterCredentialsCreateConflictAndMissingDelete(t *testing.T) {
	endpoint := startTestEtcd(t)
	client := testEtcdClient(t, endpoint)
	lg := zaptest.NewLogger(t)

	etcdCfg := etcd.Config{Prefix: "/fs-auth-conflict"}

	node, err := newClusterCredentials(t.Context(), lg, client, etcdCfg, testClusterSecret, nil, nil)
	require.NoError(t, err)

	defer func() { _ = node.Close() }()

	_, err = node.Create(auth.CreateInput{AccessKey: "AKIADUP", SecretKey: "s"})
	require.NoError(t, err)

	_, err = node.Create(auth.CreateInput{AccessKey: "AKIADUP", SecretKey: "s2"})
	require.ErrorIs(t, err, auth.ErrKeyExists)

	require.ErrorIs(t, node.Delete("AKIAMISSING"), auth.ErrKeyNotFound)

	// An access key with a slash is rejected (would break the flat namespace).
	_, err = node.Create(auth.CreateInput{AccessKey: "bad/key", SecretKey: "s"})
	require.Error(t, err)
}

func TestClusterCredentialsSeedOnlyWhenEmpty(t *testing.T) {
	endpoint := startTestEtcd(t)
	client := testEtcdClient(t, endpoint)
	lg := zaptest.NewLogger(t)
	ctx := t.Context()

	etcdCfg := etcd.Config{Prefix: "/fs-auth-seed"}

	bootstrap := []auth.Key{{
		AccessKey: "AKIAROOT",
		SecretKey: "root-secret",
		Grants:    []auth.Grant{{Pattern: "*", Permission: auth.Admin}},
	}}

	// First node seeds the empty namespace.
	first, err := newClusterCredentials(ctx, lg, client, etcdCfg, testClusterSecret, nil, bootstrap)
	require.NoError(t, err)

	defer func() { _ = first.Close() }()

	require.Eventually(t, func() bool {
		secret, ok := first.Store().Secret("AKIAROOT")

		return ok && secret == "root-secret"
	}, 10*time.Second, 20*time.Millisecond)

	// A second node with a *different* bootstrap must not overwrite or merge:
	// etcd already holds credentials, so its bootstrap is ignored.
	second, err := newClusterCredentials(ctx, lg, client, etcdCfg, testClusterSecret, nil, []auth.Key{{
		AccessKey: "AKIAOTHER",
		SecretKey: "other-secret",
		Grants:    []auth.Grant{{Pattern: "*", Permission: auth.Read}},
	}})
	require.NoError(t, err)

	defer func() { _ = second.Close() }()

	records, _, err := etcd.ListAuthKeys(ctx, client, etcdCfg)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "AKIAROOT", records[0].AccessKey)

	_, ok := second.Store().Secret("AKIAOTHER")
	assert.False(t, ok, "second node's bootstrap must be ignored when etcd is non-empty")
}

// TestClusterCredentialsWrongSecretSkipsKey verifies a node whose cluster
// secret cannot unseal a record simply omits that key rather than failing the
// whole snapshot.
func TestClusterCredentialsWrongSecretSkipsKey(t *testing.T) {
	endpoint := startTestEtcd(t)
	client := testEtcdClient(t, endpoint)
	lg := zaptest.NewLogger(t)
	ctx := t.Context()

	etcdCfg := etcd.Config{Prefix: "/fs-auth-wrongsecret"}

	writer, err := newClusterCredentials(ctx, lg, client, etcdCfg, testClusterSecret, nil, nil)
	require.NoError(t, err)

	defer func() { _ = writer.Close() }()

	_, err = writer.Create(auth.CreateInput{AccessKey: "AKIASEALED", SecretKey: "s"})
	require.NoError(t, err)

	// A reader with a different cluster secret cannot open the sealed record.
	reader, err := newClusterCredentials(ctx, lg, client, etcdCfg, "a-totally-different-secret-1234", nil, nil)
	require.NoError(t, err)

	defer func() { _ = reader.Close() }()

	// Give the watch a moment; the key must never appear on the reader.
	assert.Never(t, func() bool {
		_, ok := reader.Store().Secret("AKIASEALED")

		return ok
	}, time.Second, 50*time.Millisecond)
}
