package main

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// nodeStatus reports this node's live runtime state to peers: the queue and
// runner state that exists only inside the running process. It touches no
// control plane, so a peer poll never waits on etcd — and keeps answering
// during an etcd outage, which is exactly when an operator looks.
func (rt *clusterRuntime) nodeStatus(context.Context) (transport.NodeStatus, error) {
	reb := rt.rebalance.localStatus()

	return transport.NodeStatus{
		NodeID:           string(rt.nodeID),
		Version:          rt.version,
		SchemaVersion:    etcd.SchemaVersion,
		UptimeSeconds:    time.Since(rt.started).Seconds(),
		RepairQueueDepth: rt.coord.QueueDepth(),
		Rebalance: transport.NodeRebalance{
			State:     string(reb.State),
			Objects:   reb.Objects,
			Relocated: reb.Relocated,
			Failed:    reb.Failed,
			Err:       reb.Err,
		},
		Scrub: transport.NodeScrub{
			Passes:           rt.scrub.passes.Load(),
			Objects:          rt.scrub.objects.Load(),
			Repaired:         rt.scrub.repaired.Load(),
			Failed:           rt.scrub.failed.Load(),
			RebuiltFragments: rt.scrub.rebuilt.Load(),
			SweptStale:       rt.scrub.sweptStale.Load(),
			CorruptReplicas:  rt.scrub.corrupt.Load(),
			Converted:        rt.scrub.converted.Load(),
			ECUnverified:     rt.scrub.ecUnverifiedLastScrub.Load() != 0,
		},
	}, nil
}

// peerStatusTimeout bounds one node's live-state fetch. A status view is a
// read-only convenience: a wedged node must slow it down, never hang it.
const peerStatusTimeout = 3 * time.Second

// peerScheme is how a node's advertised address is dialed: the cluster
// listener speaks plain HTTP, authenticated by the cluster secret (TLS, where
// wanted, terminates at an ingress — see docs/DEPLOYMENT.md).
const peerScheme = "http"

// peerStatus collects live runtime state from the cluster's nodes over the
// authenticated peer transport. It is the active half of the admin's
// cluster-wide view — the passive half (topology, capacity, schema version,
// rebalance election) comes from etcd, which no node has to be reachable for.
type peerStatus struct {
	self    cluster.NodeID
	secret  transport.Secret
	http    *http.Client
	timeout time.Duration

	mu      sync.Mutex
	clients map[string]*transport.Client
}

// newPeerStatus builds the fetcher; self identifies this process to the peers.
func newPeerStatus(self cluster.NodeID, secret transport.Secret) *peerStatus {
	return &peerStatus{
		self:    self,
		secret:  secret,
		http:    &http.Client{Timeout: peerStatusTimeout},
		timeout: peerStatusTimeout,
		clients: make(map[string]*transport.Client),
	}
}

// client returns the cached transport client for an address.
func (p *peerStatus) client(addr string) (*transport.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if c, ok := p.clients[addr]; ok {
		return c, nil
	}

	base := url.URL{Scheme: peerScheme, Host: addr}

	c, err := transport.NewClient(base.String(), p.secret, p.self, p.http)
	if err != nil {
		return nil, errors.Wrapf(err, "dial node %q", addr)
	}

	p.clients[addr] = c

	return c, nil
}

// nodeLiveResult is one node's live state, or the reason it is missing.
type nodeLiveResult struct {
	Live *adminhandler.NodeLive
	Err  string
}

// Fetch asks every node for its live state, concurrently and with a per-node
// timeout. Nodes are few (1–16) and the payload is a handful of counters, so
// the fan-out is a goroutine per node. A node that fails to answer yields the
// reason, never a zero-valued status that would read as a healthy, idle node.
func (p *peerStatus) Fetch(ctx context.Context, nodes []cluster.Node) map[cluster.NodeID]nodeLiveResult {
	out := make(map[cluster.NodeID]nodeLiveResult, len(nodes))

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	for i := range nodes {
		node := nodes[i]

		wg.Add(1)

		go func() {
			defer wg.Done()

			live, err := p.fetchOne(ctx, node)

			res := nodeLiveResult{Live: live}
			if err != nil {
				res.Err = err.Error()
			}

			mu.Lock()
			defer mu.Unlock()

			out[node.ID] = res
		}()
	}

	wg.Wait()

	return out
}

// fetchOne gets one node's live state and maps it to the admin view.
func (p *peerStatus) fetchOne(ctx context.Context, node cluster.Node) (*adminhandler.NodeLive, error) {
	if node.Addr == "" {
		return nil, errors.New("node advertises no address")
	}

	c, err := p.client(node.Addr)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	st, err := c.Status(ctx)
	if err != nil {
		if errors.Is(err, transport.ErrUnsupported) {
			return nil, errors.New("node runs a binary that does not report live state")
		}

		return nil, err
	}

	return &adminhandler.NodeLive{
		Version:            st.Version,
		SchemaVersion:      st.SchemaVersion,
		UptimeSeconds:      st.UptimeSeconds,
		RepairQueueDepth:   st.RepairQueueDepth,
		RebalanceState:     adminhandler.RebalanceState(st.Rebalance.State),
		RebalanceObjects:   st.Rebalance.Objects,
		RebalanceRelocated: st.Rebalance.Relocated,
		RebalanceFailed:    st.Rebalance.Failed,
		RebalanceErr:       st.Rebalance.Err,
		ScrubPasses:        st.Scrub.Passes,
		ScrubObjects:       st.Scrub.Objects,
		ScrubRepaired:      st.Scrub.Repaired,
		ScrubFailed:        st.Scrub.Failed,
		RebuiltFragments:   st.Scrub.RebuiltFragments,
		SweptStale:         st.Scrub.SweptStale,
		CorruptReplicas:    st.Scrub.CorruptReplicas,
		Converted:          st.Scrub.Converted,
		ECUnverified:       st.Scrub.ECUnverified,
	}, nil
}
