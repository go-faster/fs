package clusterstore

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// Peer moves named fragment payloads to and from one node, local or remote.
// *transport.Client satisfies it for remote nodes.
type Peer interface {
	Put(ctx context.Context, disk cluster.DiskID, name string, size int64, body io.Reader) error
	Get(ctx context.Context, disk cluster.DiskID, name string) (io.ReadCloser, int64, error)
	Stat(ctx context.Context, disk cluster.DiskID, name string) (int64, error)
	Delete(ctx context.Context, disk cluster.DiskID, name string) error
	List(ctx context.Context, disk cluster.DiskID, prefix string) ([]string, error)
}

var _ Peer = (*transport.Client)(nil)

// PeerDialer resolves a topology node to the Peer that reaches it. Dialing is
// on the write/read path, so implementations must cache.
type PeerDialer interface {
	Peer(node cluster.Node) (Peer, error)
}

// LocalPeer adapts a node-local transport.Store to the Peer interface,
// bypassing HTTP for the node's own fragments.
type LocalPeer struct {
	Store transport.Store
	// Index answers index queries for this node without a round trip to
	// itself. Nil reports the index as unusable, which a listing reads as
	// "fall back to the sidecars" — correct, just slow, and what a store
	// wired without an index does.
	Index transport.IndexFunc
}

// IndexPage implements the optional index half of a peer.
func (p LocalPeer) IndexPage(ctx context.Context, q transport.IndexQuery) (transport.IndexPage, error) {
	if p.Index == nil {
		return transport.IndexPage{}, transport.ErrUnsupported
	}

	return p.Index(ctx, q)
}

// Put implements Peer. Exactly size bytes are copied; the fragment becomes
// visible only when the store's writer closes cleanly.
func (p LocalPeer) Put(ctx context.Context, disk cluster.DiskID, name string, size int64, body io.Reader) error {
	if !transport.ValidName(name) {
		return errors.Errorf("invalid fragment name %q", name)
	}

	w, err := p.Store.Create(ctx, disk, name)
	if err != nil {
		return errors.Wrap(err, "create fragment")
	}

	if _, err := io.CopyN(w, body, size); err != nil {
		_ = w.Close()
		return errors.Wrap(err, "copy fragment")
	}

	if err := w.Close(); err != nil {
		return errors.Wrap(err, "commit fragment")
	}

	return nil
}

// Get implements Peer.
func (p LocalPeer) Get(ctx context.Context, disk cluster.DiskID, name string) (io.ReadCloser, int64, error) {
	return p.Store.Open(ctx, disk, name)
}

// Stat implements Peer.
func (p LocalPeer) Stat(ctx context.Context, disk cluster.DiskID, name string) (int64, error) {
	return p.Store.Stat(ctx, disk, name)
}

// Delete implements Peer.
func (p LocalPeer) Delete(ctx context.Context, disk cluster.DiskID, name string) error {
	return p.Store.Delete(ctx, disk, name)
}

// List implements Peer.
func (p LocalPeer) List(ctx context.Context, disk cluster.DiskID, prefix string) ([]string, error) {
	return p.Store.List(ctx, disk, prefix)
}

// HTTPPeers is the production PeerDialer: the node's own targets go straight
// to its local store, every other node through an authenticated
// transport.Client for its Addr. Clients are cached per address.
type HTTPPeers struct {
	self   cluster.NodeID
	local  Peer
	secret transport.Secret
	http   *http.Client

	mu      sync.Mutex
	clients map[string]*transport.Client
}

// NewHTTPPeers builds the production dialer. httpClient may be nil for
// http.DefaultClient.
func NewHTTPPeers(self cluster.NodeID, local transport.Store, secret transport.Secret, httpClient *http.Client) *HTTPPeers {
	return &HTTPPeers{
		self:    self,
		local:   LocalPeer{Store: local},
		secret:  secret,
		http:    httpClient,
		clients: make(map[string]*transport.Client),
	}
}

// WithLocalIndex makes the node's own index reachable without a round trip to
// itself, so a listing does not pay HTTP to read what is in this process.
func (h *HTTPPeers) WithLocalIndex(fn transport.IndexFunc) *HTTPPeers {
	if local, ok := h.local.(LocalPeer); ok {
		local.Index = fn
		h.local = local
	}

	return h
}

// Peer implements PeerDialer.
func (h *HTTPPeers) Peer(node cluster.Node) (Peer, error) {
	if node.ID == h.self {
		return h.local, nil
	}

	return h.Client(node)
}

// Client returns the transport client for another node, from the same cache
// Peer uses.
//
// Exported because the metadata plane reaches peers through the same transport
// but needs the client itself rather than the Peer view of it, and dialing it
// separately would open a second connection per node for no reason. Unlike
// Peer, this has no local shortcut: a caller asking for a client to itself
// wants one, and it is not this type's place to decide otherwise.
func (h *HTTPPeers) Client(node cluster.Node) (*transport.Client, error) {
	if node.Addr == "" {
		return nil, errors.Errorf("node %q has no address", node.ID)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if c, ok := h.clients[node.Addr]; ok {
		return c, nil
	}

	base := url.URL{Scheme: "http", Host: node.Addr}

	c, err := transport.NewClient(base.String(), h.secret, h.self, h.http)
	if err != nil {
		return nil, errors.Wrapf(err, "dial peer %q", node.ID)
	}

	h.clients[node.Addr] = c

	return c, nil
}

// dial resolves the node carrying a placement target and dials its peer.
func (c *Coordinator) dial(topo *cluster.Topology, node cluster.NodeID) (Peer, error) {
	for i := range topo.Nodes {
		if topo.Nodes[i].ID == node {
			return c.peers.Peer(topo.Nodes[i])
		}
	}

	return nil, errors.Errorf("node %q not in topology epoch %d", node, topo.Epoch)
}
