package clusterstore

import (
	"context"
	"io"

	"golang.org/x/time/rate"

	"github.com/go-faster/fs/internal/cluster"
)

// ThrottledPeers wraps a PeerDialer with a shared bandwidth limit on fragment
// reads. Every byte a rebalance or repair pass moves is first read from a
// source peer, so limiting Get streams bounds the total data-movement rate of
// the process; metadata calls (Stat, List, Delete) stay unthrottled.
type ThrottledPeers struct {
	// Dialer is the wrapped dialer. Required.
	Dialer PeerDialer
	// Limiter is the shared byte-rate limit; its burst caps the per-Read chunk.
	// Required.
	Limiter *rate.Limiter
}

// Peer implements PeerDialer.
func (t *ThrottledPeers) Peer(node cluster.Node) (Peer, error) {
	p, err := t.Dialer.Peer(node)
	if err != nil {
		return nil, err
	}

	return &throttledPeer{Peer: p, limiter: t.Limiter}, nil
}

type throttledPeer struct {
	Peer
	limiter *rate.Limiter
}

// Get wraps the fragment stream so reading it consumes limiter tokens.
func (p *throttledPeer) Get(ctx context.Context, disk cluster.DiskID, name string) (io.ReadCloser, int64, error) {
	rc, size, err := p.Peer.Get(ctx, disk, name)
	if err != nil {
		return nil, 0, err
	}

	return &throttledReader{rc: rc, ctx: ctx, limiter: p.limiter}, size, nil
}

type throttledReader struct {
	rc      io.ReadCloser
	ctx     context.Context
	limiter *rate.Limiter
}

// Read reads at most one burst worth of bytes, then waits the tokens out.
func (r *throttledReader) Read(p []byte) (int, error) {
	if burst := r.limiter.Burst(); len(p) > burst {
		p = p[:burst]
	}

	n, err := r.rc.Read(p)

	if n > 0 {
		if werr := r.limiter.WaitN(r.ctx, n); werr != nil && err == nil {
			err = werr
		}
	}

	return n, err
}

func (r *throttledReader) Close() error { return r.rc.Close() }
