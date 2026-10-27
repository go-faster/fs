package replication

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/internal/sse"
	"github.com/go-faster/fs/server"
)

const (
	accessKey = "AKIAREPLICATION00000"
	secretKey = "replication-secret-key"
)

// deployment is an fs deployment for the runner's tests: its engine with
// bucket "data" versioned, served over S3 by an admin key. down makes it answer
// 503 to everything.
type deployment struct {
	engine *engine.Engine
	url    string
	down   atomic.Bool
}

func newDeployment(t *testing.T, kr *sse.Keyring) *deployment {
	t.Helper()

	e, err := engine.Open(t.TempDir(), engine.Options{NoSync: true, Keyring: kr})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })

	ctx := context.Background()
	require.NoError(t, e.CreateBucket(ctx, "data"))
	require.NoError(t, e.SetBucketVersioning(ctx, "data", fs.VersioningEnabled))

	store, err := auth.NewStore(auth.Config{Keys: []auth.Key{{
		AccessKey: accessKey, SecretKey: secretKey,
		Grants: []auth.Grant{{Pattern: "*", Permission: auth.Admin}},
	}}})
	require.NoError(t, err)

	d := &deployment{engine: e}
	h := server.NewHandler(e, server.WithAuth(store))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)

			return
		}

		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	d.url = srv.URL

	return d
}

func (d *deployment) target(name string) engine.ReplicationTarget {
	return engine.ReplicationTarget{
		Name: name, Endpoint: d.url, Bucket: "data",
		AccessKeyID: accessKey, SecretAccessKey: secretKey,
		DeleteMarkers: true, Insecure: true,
	}
}

func (d *deployment) put(t *testing.T, key string, body []byte) string {
	t.Helper()

	resp, err := d.engine.PutObject(context.Background(), &fs.PutObjectRequest{
		Bucket: "data", Key: key, Reader: bytes.NewReader(body), Size: int64(len(body)),
	})
	require.NoError(t, err)

	return resp.VersionID
}

func (d *deployment) versions(t *testing.T) []fs.ObjectVersion {
	t.Helper()

	resp, err := d.engine.ListObjectVersions(context.Background(), &fs.ListObjectVersionsRequest{Bucket: "data", Limit: 1000, Tags: true})
	require.NoError(t, err)

	return resp.Versions
}

func (d *deployment) backlog(t *testing.T) int64 {
	t.Helper()

	u, err := d.engine.ReplicationBacklog(context.Background(), "data")
	require.NoError(t, err)

	return u.Objects
}

func random(t *testing.T, n int) []byte {
	t.Helper()

	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)

	return b
}

func pass(t *testing.T, r *Runner) {
	t.Helper()

	_, err := r.Pass(context.Background())
	require.NoError(t, err)
}

func TestRunnerDelivers(t *testing.T) {
	ctx := context.Background()
	src, dst := newDeployment(t, nil), newDeployment(t, nil)
	require.NoError(t, src.engine.SetBucketReplication(ctx, "data", []engine.ReplicationTarget{dst.target("dr")}))

	big := random(t, 3<<20) // Blocks.
	src.put(t, "big", big)
	src.put(t, "small", []byte("hello"))
	require.NoError(t, src.engine.PutObjectTagging(ctx, "data", "small", []fs.Tag{{Key: "k", Value: "v"}}))

	_, err := src.engine.DeleteObjectVersion(ctx, "data", "gone", "")
	require.NoError(t, err)

	r := &Runner{Source: src.engine}
	pass(t, r)

	assert.Equal(t, src.versions(t), dst.versions(t), "every version, its ID, time and tags")
	assert.Zero(t, src.backlog(t))

	st := r.Stats()[Target{"data", "dr"}]
	assert.Equal(t, int64(3), st.Shipped)
	assert.Positive(t, st.BlocksSent)
	assert.Equal(t, st.BlocksOffered, st.BlocksSent, "the target had none of them")
	assert.GreaterOrEqual(t, st.BytesSent, int64(len(big)))
	assert.False(t, st.Failing)

	// The same content under another key sends no blocks.
	src.put(t, "copy", big)
	pass(t, r)

	st2 := r.Stats()[Target{"data", "dr"}]
	assert.Equal(t, st.BlocksSent, st2.BlocksSent, "dedupe")
	assert.Greater(t, st2.BlocksOffered, st.BlocksOffered)

	// A change of tags ships the tags alone.
	require.NoError(t, src.engine.PutObjectTagging(ctx, "data", "big", []fs.Tag{{Key: "new", Value: "1"}}))
	pass(t, r)

	tags, err := dst.engine.GetObjectTagging(ctx, "data", "big")
	require.NoError(t, err)
	assert.Equal(t, []fs.Tag{{Key: "new", Value: "1"}}, tags)
	assert.Equal(t, st2.BlocksOffered, r.Stats()[Target{"data", "dr"}].BlocksOffered, "no blocks for tags")

	// A version deleted at the source before it shipped is not sent, and
	// does not stay pending.
	id := src.put(t, "brief", []byte("x"))
	_, err = src.engine.DeleteObjectVersion(ctx, "data", "brief", id)
	require.NoError(t, err)

	pass(t, r)
	assert.Zero(t, src.backlog(t))

	_, err = dst.engine.GetObjectVersion(ctx, "data", "brief", id)
	require.Error(t, err)
}

func TestRunnerTargetDown(t *testing.T) {
	ctx := context.Background()
	src, dst := newDeployment(t, nil), newDeployment(t, nil)
	require.NoError(t, src.engine.SetBucketReplication(ctx, "data", []engine.ReplicationTarget{dst.target("dr")}))

	dst.down.Store(true)
	src.put(t, "k", []byte("v"))

	r := &Runner{Source: src.engine}
	_, err := r.Pass(ctx)
	require.NoError(t, err, "a target's failure is its own, not the pass's")

	st := r.Stats()[Target{"data", "dr"}]
	assert.True(t, st.Failing)
	assert.NotEmpty(t, st.LastError)
	assert.Equal(t, int64(1), st.Pending)
	assert.Equal(t, int64(1), src.backlog(t))

	// Backing off: the next pass leaves it alone.
	pass(t, r)
	assert.Equal(t, st.LastFailure, r.Stats()[Target{"data", "dr"}].LastFailure)

	dst.down.Store(false)

	require.Eventually(t, func() bool {
		pass(t, r)

		return src.backlog(t) == 0
	}, 5*time.Second, 50*time.Millisecond, "catches up after the backoff")

	st = r.Stats()[Target{"data", "dr"}]
	assert.False(t, st.Failing)
	assert.Equal(t, src.versions(t), dst.versions(t))
}

func TestRunnerRefusedDoesNotBlock(t *testing.T) {
	ctx := context.Background()

	mk := make([]byte, sse.KeySize)
	_, err := rand.Read(mk)
	require.NoError(t, err)

	key, err := sse.NewMasterKey(mk)
	require.NoError(t, err)

	kr, err := sse.NewKeyring(key)
	require.NoError(t, err)

	// The target has no master key: it refuses SSE-S3 versions.
	src, dst := newDeployment(t, kr), newDeployment(t, nil)
	require.NoError(t, src.engine.SetBucketReplication(ctx, "data", []engine.ReplicationTarget{dst.target("dr")}))

	_, err = src.engine.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: "data", Key: "sealed", Reader: bytes.NewReader([]byte("secret")), Size: 6, ServerSideEncryption: "AES256",
	})
	require.NoError(t, err)

	plain := src.put(t, "plain", []byte("open"))

	r := &Runner{Source: src.engine}
	busy, err := r.Pass(ctx)
	require.NoError(t, err)
	assert.True(t, busy)

	_, err = dst.engine.GetObjectVersion(ctx, "data", "plain", plain)
	require.NoError(t, err, "delivered past the refusal")

	st := r.Stats()[Target{"data", "dr"}]
	assert.Equal(t, int64(1), st.Shipped)
	assert.Equal(t, int64(1), st.Refused)
	assert.False(t, st.Failing, "a refusal is not the target failing")

	// The refusal is recorded in the outbox, and waits before a retry.
	entries, err := src.engine.Outbox(ctx, "data", "", 10)
	require.NoError(t, err)

	var refused int

	for _, e := range entries {
		if r, ok := e.Row.RefusedBy("dr"); ok {
			refused++

			assert.Equal(t, "sealed", e.Key)
			assert.Contains(t, r.Reason, "master key")
		}
	}

	assert.Equal(t, 1, refused)

	busy, err = r.Pass(ctx)
	require.NoError(t, err)
	assert.False(t, busy, "the refused version waits")
	assert.Equal(t, int64(1), src.backlog(t))
}

func TestRunnerRemovedTarget(t *testing.T) {
	ctx := context.Background()
	src, dst := newDeployment(t, nil), newDeployment(t, nil)

	gone := dst.target("gone")
	gone.Endpoint = "http://127.0.0.1:1" // Never answers.

	require.NoError(t, src.engine.SetBucketReplication(ctx, "data", []engine.ReplicationTarget{dst.target("dr"), gone}))
	src.put(t, "k", []byte("v"))

	r := &Runner{Source: src.engine}
	pass(t, r)
	assert.Equal(t, int64(1), src.backlog(t), "pending for the target that fails")

	require.NoError(t, src.engine.SetBucketReplication(ctx, "data", []engine.ReplicationTarget{dst.target("dr")}))
	pass(t, r)
	assert.Zero(t, src.backlog(t), "a removed target is not waited for")
	assert.NotContains(t, r.Stats(), Target{"data", "gone"})
}

func TestRunnerTwoWay(t *testing.T) {
	ctx := context.Background()
	a, b := newDeployment(t, nil), newDeployment(t, nil)
	require.NoError(t, a.engine.SetBucketReplication(ctx, "data", []engine.ReplicationTarget{b.target("b")}))
	require.NoError(t, b.engine.SetBucketReplication(ctx, "data", []engine.ReplicationTarget{a.target("a")}))

	a.put(t, "from-a", []byte("1"))
	b.put(t, "from-b", []byte("2"))

	ra, rb := &Runner{Source: a.engine}, &Runner{Source: b.engine}

	for range 3 {
		pass(t, ra)
		pass(t, rb)
	}

	assert.Equal(t, a.versions(t), b.versions(t))
	assert.Len(t, a.versions(t), 2)
	assert.Equal(t, int64(1), ra.Stats()[Target{"data", "b"}].Shipped, "replicas are not shipped back")
	assert.Equal(t, int64(1), rb.Stats()[Target{"data", "a"}].Shipped)
}

func TestRunnerOwns(t *testing.T) {
	ctx := context.Background()
	src, dst := newDeployment(t, nil), newDeployment(t, nil)
	require.NoError(t, src.engine.SetBucketReplication(ctx, "data", []engine.ReplicationTarget{dst.target("dr")}))
	src.put(t, "k", []byte("v"))

	r := &Runner{Source: src.engine, Owns: func(string) bool { return false }}
	pass(t, r)
	assert.Equal(t, int64(1), src.backlog(t), "another node's bucket")
	assert.Empty(t, r.Stats())
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	dst := newDeployment(t, nil)

	require.NoError(t, Check(ctx, dst.target("dr"), nil))

	bad := dst.target("dr")
	bad.SecretAccessKey = "wrong"
	require.Error(t, Check(ctx, bad, nil))

	require.NoError(t, dst.engine.CreateBucket(ctx, "plain"))

	plain := dst.target("dr")
	plain.Bucket = "plain"
	require.ErrorContains(t, Check(ctx, plain, nil), "versioning")
}

func TestRunnerBackfill(t *testing.T) {
	ctx := context.Background()
	src, dst := newDeployment(t, nil), newDeployment(t, nil)

	// Written before the target exists: no outbox rows.
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		src.put(t, k, []byte(k))
	}

	_, err := src.engine.DeleteObjectVersion(ctx, "data", "e", "")
	require.NoError(t, err)

	require.NoError(t, src.engine.SetBucketReplication(ctx, "data", []engine.ReplicationTarget{dst.target("dr")}))
	assert.Zero(t, src.backlog(t))

	bfs, err := src.engine.ReplicationBackfills(ctx, "data")
	require.NoError(t, err)
	require.True(t, bfs["dr"].Due(), "a new target is backfilled")
	assert.Equal(t, int64(5), bfs["dr"].Remaining)

	// Not before the delay: a write still committing with the settings from
	// before the target must land first.
	r := &Runner{Source: src.engine}
	pass(t, r)
	assert.Empty(t, dst.versions(t))
	assert.True(t, r.Stats()[Target{"data", "dr"}].Backfilling)

	// A walk interrupted after two keys, as by a restart, resumes from the
	// saved cursor with another runner.
	req := bfs["dr"].Requested.UnixNano()
	p, err := src.engine.BackfillReplication(ctx, "data", "dr", req, engine.BackfillProgress{}, 2)
	require.NoError(t, err)
	require.False(t, p.Done)
	require.NoError(t, src.engine.SaveBackfillProgress(ctx, "data", "dr", p))

	r = &Runner{Source: src.engine, BackfillDelay: time.Nanosecond}
	pass(t, r) // Walks.
	pass(t, r) // Ships what the walk queued.

	assert.Equal(t, src.versions(t), dst.versions(t), "every existing version, the marker too")
	assert.Zero(t, src.backlog(t))

	bfs, err = src.engine.ReplicationBackfills(ctx, "data")
	require.NoError(t, err)
	assert.False(t, bfs["dr"].Due())
	assert.Equal(t, int64(5), bfs["dr"].Progress.Walked, "the first two keys were not walked again")
	assert.Equal(t, int64(6), bfs["dr"].Progress.Queued)
	assert.False(t, r.Stats()[Target{"data", "dr"}].Backfilling)

	// Asked again, it walks again from the start; what the target holds
	// costs no blocks and changes nothing.
	require.NoError(t, src.engine.RequestReplicationBackfill(ctx, "data", "dr"))
	require.ErrorIs(t, src.engine.RequestReplicationBackfill(ctx, "data", "nope"), fs.ErrInvalidBucketState)

	pass(t, r)
	pass(t, r)

	bfs, err = src.engine.ReplicationBackfills(ctx, "data")
	require.NoError(t, err)
	assert.False(t, bfs["dr"].Due())
	assert.Equal(t, int64(5), bfs["dr"].Progress.Walked)
	assert.Equal(t, src.versions(t), dst.versions(t))

	// Unchanged targets are not backfilled again when the setting is
	// rewritten.
	before := bfs["dr"].Requested

	require.NoError(t, src.engine.SetBucketReplication(ctx, "data", []engine.ReplicationTarget{dst.target("dr")}))

	bfs, err = src.engine.ReplicationBackfills(ctx, "data")
	require.NoError(t, err)
	assert.Equal(t, before, bfs["dr"].Requested)
}

func TestRunnerBackfillPrefix(t *testing.T) {
	ctx := context.Background()
	src, dst := newDeployment(t, nil), newDeployment(t, nil)

	src.put(t, "img/1", []byte("1"))
	src.put(t, "log/1", []byte("2"))
	src.put(t, "img/2", []byte("3"))

	tg := dst.target("dr")
	tg.Prefix = "img/"
	require.NoError(t, src.engine.SetBucketReplication(ctx, "data", []engine.ReplicationTarget{tg}))

	r := &Runner{Source: src.engine, BackfillDelay: time.Nanosecond}
	pass(t, r)
	pass(t, r)

	var keys []string
	for _, v := range dst.versions(t) {
		keys = append(keys, v.Key)
	}

	assert.Equal(t, []string{"img/1", "img/2"}, keys)
}
