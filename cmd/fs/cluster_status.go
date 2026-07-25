package main

import (
	"context"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// clusterStatusSource assembles the admin API's cluster-wide status: the
// control-plane view — the watched topology (node/disk/rack + reported
// capacity) plus the etcd schema version and rebalance election/cursor —
// enriched with the live runtime state each node reports over the peer
// transport (queue depths, runner progress, scrub totals), which exists
// nowhere else. It needs no data node, so it backs both the per-node admin and
// the headless `fs admin`.
type clusterStatusSource struct {
	topo    clusterstore.TopologySource
	client  *clientv3.Client
	etcdCfg etcd.Config
	// live fetches the nodes' runtime state; nil leaves the status
	// control-plane-only.
	live *peerStatus
}

func newClusterStatusSource(topo clusterstore.TopologySource, client *clientv3.Client, etcdCfg etcd.Config, live *peerStatus) *clusterStatusSource {
	return &clusterStatusSource{topo: topo, client: client, etcdCfg: etcdCfg, live: live}
}

var _ adminhandler.ClusterStatusSource = (*clusterStatusSource)(nil)

// ClusterStatus implements adminhandler.ClusterStatusSource.
func (s *clusterStatusSource) ClusterStatus(ctx context.Context) (adminhandler.ClusterStatus, error) {
	topo := s.topo.Topology()

	// Ask the nodes themselves for what etcd cannot know. Best-effort: a node
	// that does not answer is reported as not reporting, and the rest of the
	// view still renders.
	var live map[cluster.NodeID]nodeLiveResult

	if s.live != nil {
		live = s.live.Fetch(ctx, topo.Nodes)
	}

	nodes := make([]adminhandler.ClusterNode, 0, len(topo.Nodes))
	for i := range topo.Nodes {
		n := &topo.Nodes[i]

		disks := make([]adminhandler.ClusterDisk, 0, len(n.Disks))
		for _, d := range n.Disks {
			disks = append(disks, adminhandler.ClusterDisk{
				ID:         string(d.ID),
				Weight:     d.Weight,
				TotalBytes: d.TotalBytes,
				FreeBytes:  d.FreeBytes,
			})
		}

		node := adminhandler.ClusterNode{
			ID:    string(n.ID),
			Addr:  n.Addr,
			Rack:  n.Rack,
			Disks: disks,
		}

		// Fetch answers for every node it was given, so a missing entry only
		// happens when live state is not collected at all.
		if res, ok := live[n.ID]; ok {
			node.Live, node.LiveError = res.Live, res.Err
		}

		nodes = append(nodes, node)
	}

	st := adminhandler.ClusterStatus{
		BinarySchemaVersion: etcd.SchemaVersion,
		Nodes:               nodes,
	}

	// Control-plane reads are best-effort: a status view must not fail just
	// because the schema key or an election lookup momentarily errored.
	if v, ok, err := etcd.LoadSchemaVersion(ctx, s.client, s.etcdCfg); err == nil && ok {
		st.SchemaVersion = v
	}

	if running, err := etcd.RebalanceLeaderExists(ctx, s.client, s.etcdCfg); err == nil {
		st.RebalanceRunning = running
	}

	if raw, ok, err := etcd.LoadRebalanceCursor(ctx, s.client, s.etcdCfg); err == nil && ok {
		if cur, err := clusterstore.DecodeRebalanceCursor(raw); err == nil {
			st.RebalanceCursorBucket, st.Cursor = cur.Bucket, cur.Key
		}
	}

	return st, nil
}
