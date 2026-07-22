package transport_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/transport"
)

var secret = transport.Secret("test-cluster-secret")

func newPeer(t *testing.T) *transport.Client {
	t.Helper()

	store := transport.NewMemStore()
	srv := httptest.NewServer(transport.NewServer(store, secret))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, secret, "node-a", srv.Client())
	require.NoError(t, err)

	return client
}

func randBytes(n int) []byte {
	r := rand.New(rand.NewSource(int64(n)*31 + 11)) //nolint:gosec // deterministic test data
	b := make([]byte, n)
	_, _ = r.Read(b)

	return b
}

func TestPutGetRoundTrip(t *testing.T) {
	client := newPeer(t)
	ctx := context.Background()

	for _, n := range []int{0, 1, 4096, 1 << 20} {
		data := randBytes(n)
		name := fmt.Sprintf("bucket/obj-%d/0", n)

		require.NoError(t, client.Put(ctx, "d0", name, int64(n), bytes.NewReader(data)))

		rc, size, err := client.Get(ctx, "d0", name)
		require.NoError(t, err)
		assert.Equal(t, int64(n), size)

		got, err := io.ReadAll(rc)
		require.NoError(t, err, "n=%d (checksum verified at EOF)", n)
		require.NoError(t, rc.Close())
		assert.True(t, bytes.Equal(data, got), "n=%d", n)
	}
}

func TestStatAndDelete(t *testing.T) {
	client := newPeer(t)
	ctx := context.Background()

	data := randBytes(500)
	require.NoError(t, client.Put(ctx, "d0", "frag", 500, bytes.NewReader(data)))

	size, err := client.Stat(ctx, "d0", "frag")
	require.NoError(t, err)
	assert.Equal(t, int64(500), size)

	require.NoError(t, client.Delete(ctx, "d0", "frag"))

	_, err = client.Stat(ctx, "d0", "frag")
	require.ErrorIs(t, err, transport.ErrNotFound)

	err = client.Delete(ctx, "d0", "frag")
	require.ErrorIs(t, err, transport.ErrNotFound)

	_, _, err = client.Get(ctx, "d0", "frag")
	require.ErrorIs(t, err, transport.ErrNotFound)
}

func TestAuthWrongSecret(t *testing.T) {
	store := transport.NewMemStore()
	srv := httptest.NewServer(transport.NewServer(store, secret))
	t.Cleanup(srv.Close)

	bad, err := transport.NewClient(srv.URL, transport.Secret("wrong"), "node-x", srv.Client())
	require.NoError(t, err)

	err = bad.Put(context.Background(), "d0", "frag", 3, bytes.NewReader([]byte("abc")))
	require.ErrorIs(t, err, transport.ErrUnauthorized)

	_, _, err = bad.Get(context.Background(), "d0", "frag")
	require.ErrorIs(t, err, transport.ErrUnauthorized)
}

func TestAuthUnsignedRequest(t *testing.T) {
	store := transport.NewMemStore()
	srv := httptest.NewServer(transport.NewServer(store, secret))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/v1/fragments/d0/frag", http.NoBody)
	require.NoError(t, err)

	resp, err := srv.Client().Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// tamperReader mutates the first non-empty read, simulating in-transit
// corruption between peers.
type tamperReader struct {
	rc       io.ReadCloser
	limit    int64 // when > 0, stop (truncate) after this many bytes
	served   int64
	tampered bool
	flip     bool
}

func (r *tamperReader) Read(p []byte) (int, error) {
	if r.limit > 0 && r.served >= r.limit {
		return 0, io.EOF
	}

	n, err := r.rc.Read(p)
	if n > 0 {
		if r.flip && !r.tampered {
			p[0] ^= 0xff
			r.tampered = true
		}

		r.served += int64(n)
	}

	return n, err
}

func (r *tamperReader) Close() error { return r.rc.Close() }

// tamperingPeer stands a real fragment server behind a proxy that corrupts or
// truncates response bodies in transit.
func tamperingPeer(t *testing.T, flip bool, truncateAt int64) *transport.Client {
	t.Helper()

	origin := httptest.NewServer(transport.NewServer(transport.NewMemStore(), secret))
	t.Cleanup(origin.Close)

	target, err := url.Parse(origin.URL)
	require.NoError(t, err)

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.Request.Method == http.MethodGet {
			resp.Body = &tamperReader{rc: resp.Body, flip: flip, limit: truncateAt}
		}

		return nil
	}

	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)

	client, err := transport.NewClient(front.URL, secret, "node-a", front.Client())
	require.NoError(t, err)

	return client
}

// TestGetDetectsTamperedStream: a byte flipped in transit must fail the digest
// check at EOF. (At-rest bit-rot on the peer is the storage layer's job —
// sidecar checksums and the scrubber — not the transport's.)
func TestGetDetectsTamperedStream(t *testing.T) {
	client := tamperingPeer(t, true, 0)
	ctx := context.Background()

	data := randBytes(10000)
	require.NoError(t, client.Put(ctx, "d0", "frag", int64(len(data)), bytes.NewReader(data)))

	rc, _, err := client.Get(ctx, "d0", "frag")
	require.NoError(t, err)

	defer func() { _ = rc.Close() }()

	_, err = io.ReadAll(rc)
	require.ErrorIs(t, err, transport.ErrChecksumMismatch,
		"tampered payload must fail digest verification at EOF")
}

// TestGetDetectsTruncatedStream: a stream cut short in transit never delivers a
// valid digest trailer, so the read must fail rather than silently return a
// prefix.
func TestGetDetectsTruncatedStream(t *testing.T) {
	client := tamperingPeer(t, false, 1000)
	ctx := context.Background()

	data := randBytes(10000)
	require.NoError(t, client.Put(ctx, "d0", "frag", int64(len(data)), bytes.NewReader(data)))

	rc, _, err := client.Get(ctx, "d0", "frag")
	require.NoError(t, err)

	defer func() { _ = rc.Close() }()

	_, err = io.ReadAll(rc)
	require.Error(t, err, "truncated payload must not read cleanly to EOF")
}

func TestPathTraversalRejected(t *testing.T) {
	client := newPeer(t)
	ctx := context.Background()

	for _, name := range []string{"", "..", "../x", "a/../../b", "/abs"} {
		err := client.Put(ctx, "d0", name, 1, bytes.NewReader([]byte{1}))
		require.Error(t, err, "name %q must be rejected", name)
	}
}

func TestNamesWithSpecialCharacters(t *testing.T) {
	client := newPeer(t)
	ctx := context.Background()

	// S3 keys can carry spaces, unicode, percent and plus; fragment names
	// derived from them must round-trip.
	for _, name := range []string{
		"bucket/my file.txt/0",
		"bucket/päth/ünïcode/2",
		"bucket/100%+sure/1",
	} {
		data := randBytes(64)
		require.NoError(t, client.Put(ctx, "d0", name, 64, bytes.NewReader(data)), "name %q", name)

		rc, _, err := client.Get(ctx, "d0", name)
		require.NoError(t, err, "name %q", name)

		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		assert.True(t, bytes.Equal(data, got), "name %q", name)
	}
}

func TestConcurrentPuts(t *testing.T) {
	client := newPeer(t)
	ctx := context.Background()

	const workers = 16

	var wg sync.WaitGroup

	for i := range workers {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			data := randBytes(2048 + i)
			name := fmt.Sprintf("bucket/obj/%d", i)

			if err := client.Put(ctx, "d0", name, int64(len(data)), bytes.NewReader(data)); err != nil {
				t.Errorf("put %d: %v", i, err)
				return
			}

			rc, _, err := client.Get(ctx, "d0", name)
			if err != nil {
				t.Errorf("get %d: %v", i, err)
				return
			}

			got, err := io.ReadAll(rc)
			_ = rc.Close()

			if err != nil || !bytes.Equal(data, got) {
				t.Errorf("round-trip %d failed: %v", i, err)
			}
		}(i)
	}

	wg.Wait()
}

func TestDisksAreIsolated(t *testing.T) {
	client := newPeer(t)
	ctx := context.Background()

	require.NoError(t, client.Put(ctx, "d0", "frag", 3, bytes.NewReader([]byte("abc"))))

	_, err := client.Stat(ctx, "d1", "frag")
	require.ErrorIs(t, err, transport.ErrNotFound, "fragments are per-disk")
}
