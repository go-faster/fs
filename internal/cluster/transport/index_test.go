package transport_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/transport"
)

// indexPage is what the peer under test serves.
func indexPage() transport.IndexPage {
	return transport.IndexPage{
		Ready: true,
		Entries: []transport.IndexEntry{
			{Key: "a.txt", Size: 10, ETag: "etag-a", Modified: time.Unix(1, 0).UTC(), Seq: 3},
			{Key: "b.txt", Size: 20, OwnerID: "owner", OwnerName: "Owner"},
		},
	}
}

func indexPeer(t *testing.T, fn transport.IndexFunc) *transport.Client {
	t.Helper()

	srv := httptest.NewServer(transport.NewServer(transport.NewMemStore(), secret, transport.WithIndex(fn)))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, secret, "node-a", srv.Client())
	require.NoError(t, err)

	return client
}

func TestIndexRoundTrip(t *testing.T) {
	var got transport.IndexQuery

	client := indexPeer(t, func(_ context.Context, q transport.IndexQuery) (transport.IndexPage, error) {
		got = q

		return indexPage(), nil
	})

	page, err := client.IndexPage(context.Background(), transport.IndexQuery{
		Bucket: "photos", Prefix: "a/", After: "a/x", Limit: 50,
	})
	require.NoError(t, err)

	assert.Equal(t, "photos", got.Bucket)
	assert.Equal(t, "a/", got.Prefix)
	assert.Equal(t, "a/x", got.After)
	assert.Equal(t, 50, got.Limit)

	assert.Equal(t, indexPage(), page, "entries survive the hop intact")
}

// TestIndexCarriesKeysThatNeedEncoding: a query travels in the URL, so keys
// with spaces, unicode or the 0xFF byte the group skip uses must arrive as
// sent.
func TestIndexCarriesKeysThatNeedEncoding(t *testing.T) {
	var got transport.IndexQuery

	client := indexPeer(t, func(_ context.Context, q transport.IndexQuery) (transport.IndexPage, error) {
		got = q

		return transport.IndexPage{Ready: true}, nil
	})

	after := "видео/clip 01.mp4\xff"

	_, err := client.IndexPage(context.Background(), transport.IndexQuery{
		Bucket: "b", Prefix: "видео/", After: after, Limit: 10,
	})
	require.NoError(t, err)

	assert.Equal(t, after, got.After)
	assert.Equal(t, "видео/", got.Prefix)
}

// TestIndexUnsupported: a node wired without an index, and an older binary with
// no route at all, both read as unsupported — which is what makes a rolling
// upgrade degrade to the sidecar walk instead of to an error.
func TestIndexUnsupported(t *testing.T) {
	_, err := newPeer(t).IndexPage(context.Background(), transport.IndexQuery{Bucket: "b", Limit: 1})
	require.ErrorIs(t, err, transport.ErrUnsupported)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, secret, "node-a", srv.Client())
	require.NoError(t, err)

	_, err = client.IndexPage(context.Background(), transport.IndexQuery{Bucket: "b", Limit: 1})
	require.ErrorIs(t, err, transport.ErrUnsupported)
}

func TestIndexRequiresAuth(t *testing.T) {
	srv := httptest.NewServer(transport.NewServer(transport.NewMemStore(), secret,
		transport.WithIndex(func(context.Context, transport.IndexQuery) (transport.IndexPage, error) {
			return indexPage(), nil
		}),
	))
	t.Cleanup(srv.Close)

	bad, err := transport.NewClient(srv.URL, transport.Secret("wrong"), "node-x", srv.Client())
	require.NoError(t, err)

	_, err = bad.IndexPage(context.Background(), transport.IndexQuery{Bucket: "b", Limit: 1})
	require.ErrorIs(t, err, transport.ErrUnauthorized)
}

// TestIndexRejectsUnboundedLimit: a page must be bounded, or a peer can be made
// to assemble an unbounded one.
func TestIndexRejectsUnboundedLimit(t *testing.T) {
	client := indexPeer(t, func(context.Context, transport.IndexQuery) (transport.IndexPage, error) {
		return indexPage(), nil
	})

	_, err := client.IndexPage(context.Background(), transport.IndexQuery{Bucket: "b", Limit: 0})
	require.Error(t, err)
}

// TestIndexNotReady is the answer that makes a listing fall back rather than
// silently lose the objects only this node holds.
func TestIndexNotReady(t *testing.T) {
	client := indexPeer(t, func(context.Context, transport.IndexQuery) (transport.IndexPage, error) {
		return transport.IndexPage{}, nil
	})

	page, err := client.IndexPage(context.Background(), transport.IndexQuery{Bucket: "b", Limit: 10})
	require.NoError(t, err)
	assert.False(t, page.Ready)
	assert.Empty(t, page.Entries)
}
