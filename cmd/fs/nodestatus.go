package main

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"go.uber.org/zap"

	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/cluster/objindex"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// nodeStatus reports this node's live runtime state to peers: the queue and
// runner state that exists only inside the running process. It touches no
// control plane, so a peer poll never waits on etcd — and keeps answering
// during an etcd outage, which is exactly when an operator looks.
func (rt *clusterRuntime) nodeStatus(ctx context.Context) (transport.NodeStatus, error) {
	reb := rt.rebalance.localStatus()
	coverage := rt.scrubCoverage()

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
			OldestVerified:   coverage.Oldest,
			NeverVerified:    coverage.Never,
			Held:             coverage.Objects,
		},
		Disks: rt.diskStatus(ctx),
	}, nil
}

// liveDisks maps a peer's reported disks into the admin domain type.
func liveDisks(disks []transport.NodeDisk) []adminhandler.NodeDisk {
	if len(disks) == 0 {
		return nil
	}

	out := make([]adminhandler.NodeDisk, 0, len(disks))
	for _, d := range disks {
		out = append(out, adminhandler.NodeDisk{
			ID:        d.ID,
			HasData:   d.HasData,
			Err:       d.Err,
			Fragments: d.Fragments,
			Bytes:     d.Bytes,
			Counted:   d.Counted,
		})
	}

	return out
}

// diskStatus probes each of the node's disks for whether it still holds data —
// the drain signal an orchestrator gates a decommission on.
//
// A probe that fails carries its error instead of a verdict: reporting a disk
// the node could not read as drained would invite deleting a volume still
// holding the only copy of something.
func (rt *clusterRuntime) diskStatus(ctx context.Context) []transport.NodeDisk {
	if len(rt.node.Disks) == 0 {
		return nil
	}

	disks := make([]transport.NodeDisk, 0, len(rt.node.Disks))

	for _, disk := range rt.node.Disks {
		status := transport.NodeDisk{ID: string(disk.ID)}

		hasData, err := rt.store.HasData(ctx, disk.ID)
		if err != nil {
			status.Err = err.Error()
		} else {
			status.HasData = hasData
		}

		// Occupancy rides along as progress, not as a verdict: the probe above
		// decides whether the disk is empty, and the index only says how much
		// is left to move. An index error is not worth reporting — the disk
		// still answered the question that matters.
		if u, err := rt.store.Occupancy(disk.ID); err == nil && u.Anchored {
			status.Fragments = u.Fragments
			status.Bytes = u.Bytes
			status.Counted = true
		}

		disks = append(disks, status)
	}

	return disks
}

// scrubCoverage returns the last computed coverage.
//
// It is read here rather than computed here: deriving it scans every entry in
// the index, and this runs on a peer's status request, which is contracted to
// be cheap and bounded. A status path that walked the node's whole object set
// would be the same mistake the index exists to remove.
func (rt *clusterRuntime) scrubCoverage() objindex.Coverage {
	if cov := rt.coverage.Load(); cov != nil {
		return *cov
	}

	return objindex.Coverage{}
}

// coverageInterval is how often the node re-derives its verification coverage.
// The numbers move as the scrub advances and as writes add objects nothing has
// checked yet, so they go stale slowly; the scan is local but proportional to
// what the node holds, which is why it is not on a request path.
const coverageInterval = 10 * time.Minute

// RunCoverage keeps this node's verification coverage current.
//
// An index that is missing or still building leaves the coverage unset rather
// than zero: zero would read as "everything verified just now", the opposite of
// the truth and exactly the reading these numbers exist to prevent.
func (rt *clusterRuntime) RunCoverage(ctx context.Context) {
	if rt.index == nil {
		return
	}

	ticker := time.NewTicker(coverageInterval)
	defer ticker.Stop()

	for {
		if state, err := rt.index.State(); err == nil && state == objindex.StateReady {
			if cov, err := rt.index.Coverage(); err == nil {
				rt.coverage.Store(&cov)
			} else if ctx.Err() == nil {
				rt.lg.Warn("Computing scrub coverage failed", zap.Error(err))
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
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
		OldestVerified:     st.Scrub.OldestVerified,
		NeverVerified:      st.Scrub.NeverVerified,
		Held:               st.Scrub.Held,
		Disks:              liveDisks(st.Disks),
	}, nil
}
