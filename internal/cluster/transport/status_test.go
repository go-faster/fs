package transport_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/transport"
)

// reportedStatus is the live state the test node serves.
func reportedStatus() transport.NodeStatus {
	return transport.NodeStatus{
		NodeID:           "node-b",
		Version:          "v1.2.3",
		SchemaVersion:    1,
		UptimeSeconds:    42.5,
		RepairQueueDepth: 7,
		Rebalance: transport.NodeRebalance{
			State:     "running",
			Objects:   12,
			Relocated: 3,
			Failed:    1,
			Err:       "one object failed",
		},
		Scrub: transport.NodeScrub{
			Passes:           2,
			Objects:          100,
			Repaired:         4,
			RebuiltFragments: 5,
			CorruptReplicas:  1,
			ECUnverified:     true,
		},
	}
}

// statusPeer serves a node with live state wired in.
func statusPeer(t *testing.T) *transport.Client {
	t.Helper()

	srv := httptest.NewServer(transport.NewServer(transport.NewMemStore(), secret,
		transport.WithStatus(func(context.Context) (transport.NodeStatus, error) {
			return reportedStatus(), nil
		}),
	))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, secret, "node-a", srv.Client())
	require.NoError(t, err)

	return client
}

func TestStatusRoundTrip(t *testing.T) {
	got, err := statusPeer(t).Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, reportedStatus(), got)
}

// TestStatusUnsupported: a node wired without a status source reports
// ErrUnsupported rather than a generic failure, so a mixed-version cluster
// reads as "not reporting".
func TestStatusUnsupported(t *testing.T) {
	_, err := newPeer(t).Status(context.Background())
	require.ErrorIs(t, err, transport.ErrUnsupported)
}

// TestStatusOldPeerRouteMissing: a pre-status binary has no route at all and
// answers 404; that must also read as unsupported, not as a missing fragment.
func TestStatusOldPeerRouteMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, secret, "node-a", srv.Client())
	require.NoError(t, err)

	_, err = client.Status(context.Background())
	require.ErrorIs(t, err, transport.ErrUnsupported)
}

func TestStatusRequiresAuth(t *testing.T) {
	srv := httptest.NewServer(transport.NewServer(transport.NewMemStore(), secret,
		transport.WithStatus(func(context.Context) (transport.NodeStatus, error) {
			return reportedStatus(), nil
		}),
	))
	t.Cleanup(srv.Close)

	bad, err := transport.NewClient(srv.URL, transport.Secret("wrong"), "node-x", srv.Client())
	require.NoError(t, err)

	_, err = bad.Status(context.Background())
	require.ErrorIs(t, err, transport.ErrUnauthorized)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1/status", http.NoBody)
	require.NoError(t, err)

	resp, err := srv.Client().Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestStatusDetectsTamperedBody: a status document rewritten in transit fails
// the digest/signature check — an operator view must not be forgeable by
// whatever sits between the nodes.
func TestStatusDetectsTamperedBody(t *testing.T) {
	origin := httptest.NewServer(transport.NewServer(transport.NewMemStore(), secret,
		transport.WithStatus(func(context.Context) (transport.NodeStatus, error) {
			return reportedStatus(), nil
		}),
	))
	t.Cleanup(origin.Close)

	target, err := url.Parse(origin.URL)
	require.NoError(t, err)

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Body = &tamperReader{rc: resp.Body, flip: true}
		return nil
	}

	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)

	client, err := transport.NewClient(front.URL, secret, "node-a", front.Client())
	require.NoError(t, err)

	_, err = client.Status(context.Background())
	require.ErrorIs(t, err, transport.ErrChecksumMismatch)
}

// TestStatusSourceError surfaces a failing status source as an error, not as
// an empty document that would read as a healthy, idle node.
func TestStatusSourceError(t *testing.T) {
	srv := httptest.NewServer(transport.NewServer(transport.NewMemStore(), secret,
		transport.WithStatus(func(context.Context) (transport.NodeStatus, error) {
			return transport.NodeStatus{}, assert.AnError
		}),
	))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, secret, "node-a", srv.Client())
	require.NoError(t, err)

	_, err = client.Status(context.Background())
	require.Error(t, err)
	assert.NotErrorIs(t, err, transport.ErrUnsupported)
}
