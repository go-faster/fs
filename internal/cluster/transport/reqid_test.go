package transport_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/zctx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/transport"
	"github.com/go-faster/fs/internal/reqid"
)

// recordingStore reports the request ID it was called under, and optionally
// fails every write — the peer-side half of a coordinator's "peer returned
// status 500".
type recordingStore struct {
	*transport.MemStore

	seen    chan string
	failPut error
}

func newRecordingStore() *recordingStore {
	return &recordingStore{MemStore: transport.NewMemStore(), seen: make(chan string, 8)}
}

func (s *recordingStore) Create(ctx context.Context, disk cluster.DiskID, name string) (io.WriteCloser, error) {
	s.seen <- reqid.FromContext(ctx)

	if s.failPut != nil {
		return nil, s.failPut
	}

	return s.MemStore.Create(ctx, disk, name)
}

func (s *recordingStore) Stat(ctx context.Context, disk cluster.DiskID, name string) (int64, error) {
	s.seen <- reqid.FromContext(ctx)

	return s.MemStore.Stat(ctx, disk, name)
}

// newPeerWithStore serves store over a test listener whose request contexts
// carry lg, mirroring how the cluster listener is wired.
func newPeerWithStore(t *testing.T, store transport.Store, lg *zap.Logger) *transport.Client {
	t.Helper()

	srv := httptest.NewUnstartedServer(transport.NewServer(store, secret))
	srv.Config.BaseContext = func(net.Listener) context.Context { return zctx.Base(context.Background(), lg) }
	srv.Start()
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, secret, "node-a", srv.Client())
	require.NoError(t, err)

	return client
}

// TestRequestIDReachesPeer is the property the whole plumbing exists for: the
// ID the S3 client was handed is the ID the peer serves the fragment under, so
// one grep finds both sides of the request.
func TestRequestIDReachesPeer(t *testing.T) {
	store := newRecordingStore()
	client := newPeerWithStore(t, store, zap.NewNop())

	ctx := reqid.NewContext(context.Background(), "55690797AEB458C9")

	require.NoError(t, client.Put(ctx, "d0", "bucket/obj/0", 3, bytes.NewReader([]byte("abc"))))
	assert.Equal(t, "55690797AEB458C9", <-store.seen)

	_, err := client.Stat(ctx, "d0", "bucket/obj/0")
	require.NoError(t, err)
	assert.Equal(t, "55690797AEB458C9", <-store.seen)
}

// TestNoRequestIDForBackgroundWork keeps scrub, repair and rebalance traffic —
// which no client request originated — from being labeled with someone else's
// ID or with an empty one.
func TestNoRequestIDForBackgroundWork(t *testing.T) {
	store := newRecordingStore()
	client := newPeerWithStore(t, store, zap.NewNop())

	require.NoError(t, client.Put(context.Background(), "d0", "bucket/obj/0", 3, bytes.NewReader([]byte("abc"))))
	assert.Empty(t, <-store.seen)
}

// TestHostileRequestIDIsDropped covers a peer — one that holds the cluster
// secret but is compromised or buggy — trying to use the ID as a write channel
// into this node's log. The request is still served; only the label is refused.
func TestHostileRequestIDIsDropped(t *testing.T) {
	// Control characters are absent here on purpose: net/http rejects them as
	// header values before they reach the wire at all. Everything below is a
	// perfectly legal HTTP header that the server must still refuse to log.
	for name, id := range map[string]string{
		"oversized":     strings.Repeat("A", 64*1024),
		"just-over-max": strings.Repeat("A", reqid.MaxLen+1),
		"spaces":        "id with spaces",
		"json":          `","level":"info","msg":"all good`,
	} {
		t.Run(name, func(t *testing.T) {
			core, logs := observer.New(zap.ErrorLevel)

			store := newRecordingStore()
			store.failPut = errors.New("disk d0 is read-only")

			srv := httptest.NewUnstartedServer(transport.NewServer(store, secret))
			srv.Config.BaseContext = func(net.Listener) context.Context {
				return zctx.Base(context.Background(), zap.New(core))
			}
			srv.Start()

			t.Cleanup(srv.Close)

			// Send the hostile value on the wire directly: a well-behaved
			// Client would have refused to put it there.
			resp := putWithRequestID(t, srv, id)
			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
			assert.Empty(t, <-store.seen, "hostile ID must not reach the store context")

			// The failure is still logged (that is the point of the feature) —
			// just never carrying the attacker's bytes.
			require.Len(t, logs.All(), 1)
			assert.NotContains(t, logs.All()[0].ContextMap(), reqid.Field)
		})
	}
}

// putWithRequestID performs a correctly signed fragment PUT carrying a raw
// request-ID header, bypassing Client's own validation.
func putWithRequestID(t *testing.T, srv *httptest.Server, id string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut,
		srv.URL+"/v1/fragments/d0/bucket/obj/0", bytes.NewReader([]byte("abc")))
	require.NoError(t, err)

	transport.SignForTest(secret, req, "node-a")
	req.Header.Set("X-Cluster-Request-Id", id)

	resp, err := srv.Client().Do(req)
	require.NoError(t, err)

	return resp
}

// TestPeerFailureIsLoggedUnderRequestID covers what made the original failure
// undiagnosable: the coordinator only learns "status 500", so the cause has to
// be in the peer's own log, findable by the request ID.
func TestPeerFailureIsLoggedUnderRequestID(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)

	store := newRecordingStore()
	store.failPut = errors.New("disk d0 is read-only")

	client := newPeerWithStore(t, store, zap.New(core))

	ctx := reqid.NewContext(context.Background(), "55690797AEB458C9")

	err := client.Put(ctx, "d0", "bucket/obj/0", 3, bytes.NewReader([]byte("abc")))
	require.ErrorContains(t, err, "peer returned status 500")

	entries := logs.All()
	require.Len(t, entries, 1)

	fields := entries[0].ContextMap()
	assert.Equal(t, "55690797AEB458C9", fields[reqid.Field])
	assert.Equal(t, "node-a", fields["peer_node"])
	assert.Equal(t, int64(http.StatusInternalServerError), fields["status"])
	assert.Contains(t, fields["error"], "disk d0 is read-only")
}
