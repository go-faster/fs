package adminhandler

import (
	"context"
	"time"

	"github.com/go-faster/fs/adminapi"
)

// ClusterDisk is one disk's placement weight and reported capacity.
type ClusterDisk struct {
	ID         string
	Weight     float64
	TotalBytes uint64
	FreeBytes  uint64
}

// ClusterNode is a cluster member and its disks.
type ClusterNode struct {
	ID    string
	Addr  string
	Rack  string
	Disks []ClusterDisk
	// Live is the node's live runtime state, fetched from the node itself; nil
	// when it did not report.
	Live *NodeLive
	// LiveError says why Live is nil (unreachable, or a binary that does not
	// serve live state).
	LiveError string
}

// NodeLive is what only a running node knows: its queue depths, runner
// progress and scrub totals. The control plane (etcd) carries none of this, so
// it is collected from the nodes over the peer transport.
type NodeLive struct {
	Version       string
	SchemaVersion int
	UptimeSeconds float64
	// RepairQueueDepth is the node's pending async remainder/repair backlog.
	RepairQueueDepth int
	// Rebalance* is the node's rebalance runner and its current/last run.
	RebalanceState                                        RebalanceState
	RebalanceObjects, RebalanceRelocated, RebalanceFailed int
	RebalanceErr                                          string
	// Scrub* and the repair totals are cumulative since the node started.
	ScrubPasses, ScrubObjects, ScrubRepaired, ScrubFailed int64
	RebuiltFragments, SweptStale                          int64
	CorruptReplicas, Converted                            int64
	// ECUnverified reports that the node's last scrub pass saw an EC set
	// failing parity verification.
	ECUnverified bool
	// OldestVerified, NeverVerified and Held are the node's scrub coverage:
	// how long ago the least recently checked object was checked, how many
	// have never been, and how many the node holds. Counts of work done say
	// how busy a scrubber was; these say whether it is keeping up.
	OldestVerified time.Time
	NeverVerified  int64
	Held           int64
	// Disks is what each of the node's disks holds, as only the node can know.
	Disks []NodeDisk
}

// NodeDisk is what one of a node's disks holds right now, reported by the node
// itself. It is the drain signal: an orchestrator decommissioning a node waits
// for HasData to go false before it deletes the volume.
type NodeDisk struct {
	ID      string
	HasData bool
	// Err is why the disk could not be probed. A disk that failed to answer is
	// unknown, never drained.
	Err string
	// Fragments and Bytes are what the node's occupancy index says the disk
	// holds, so a drain in progress has a number attached. Meaningful only
	// when Counted; HasData remains what decides "empty".
	Fragments int64
	Bytes     int64
	Counted   bool
}

// ClusterStatus is the cluster-wide view assembled from the control plane.
type ClusterStatus struct {
	SchemaVersion       int
	BinarySchemaVersion int
	Nodes               []ClusterNode
	// RebalanceRunning reports whether a runner holds the cluster-wide
	// election; the cursor is the in-progress/last resume point.
	RebalanceRunning              bool
	RebalanceCursorBucket, Cursor string
}

// ClusterStatusSource assembles the cluster-wide status from etcd (topology,
// schema version, rebalance election/cursor). Implemented by the cluster
// runtime and the headless admin; nil outside cluster mode.
type ClusterStatusSource interface {
	ClusterStatus(ctx context.Context) (ClusterStatus, error)
}

// GetClusterStatus reports the cluster-wide view.
func (a *AdminAPI) GetClusterStatus(ctx context.Context) (*adminapi.ClusterStatus, error) {
	if a.opts.ClusterStatus == nil {
		return &adminapi.ClusterStatus{State: adminapi.ClusterStateDisabled}, nil
	}

	st, err := a.opts.ClusterStatus.ClusterStatus(ctx)
	if err != nil {
		return nil, apiErr(500, err)
	}

	return clusterStatusToAPI(st), nil
}

// clusterStatusToAPI maps the domain status to the wire schema, computing the
// aggregate capacity and placement skew.
func clusterStatusToAPI(st ClusterStatus) *adminapi.ClusterStatus {
	out := &adminapi.ClusterStatus{
		State:               adminapi.ClusterStateOk,
		SchemaVersion:       st.SchemaVersion,
		BinarySchemaVersion: st.BinarySchemaVersion,
		NodeCount:           len(st.Nodes),
		RebalanceRunning:    st.RebalanceRunning,
		Nodes:               make([]adminapi.ClusterNode, 0, len(st.Nodes)),
	}

	if st.RebalanceCursorBucket != "" || st.Cursor != "" {
		out.RebalanceCursorBucket = adminapi.NewOptString(st.RebalanceCursorBucket)
		out.RebalanceCursorKey = adminapi.NewOptString(st.Cursor)
	}

	var (
		minFull = 2.0
		maxFull = -1.0
	)

	for _, n := range st.Nodes {
		apiNode := adminapi.ClusterNode{ID: n.ID, Disks: make([]adminapi.ClusterDisk, 0, len(n.Disks))}

		if n.Addr != "" {
			apiNode.Addr = adminapi.NewOptString(n.Addr)
		}

		if n.Rack != "" {
			apiNode.Rack = adminapi.NewOptString(n.Rack)
		}

		switch {
		case n.Live != nil:
			apiNode.Live = adminapi.NewOptClusterNodeLive(liveToAPI(*n.Live))
			out.NodesReporting++
			out.RepairQueueDepth += n.Live.RepairQueueDepth
		default:
			out.NodesNotReporting++

			if n.LiveError != "" {
				apiNode.LiveError = adminapi.NewOptString(n.LiveError)
			}
		}

		out.DiskCount += len(n.Disks)

		// Capacity comes from the control plane, what a disk holds only from
		// the node. Merge the two so one disk reads as one thing; a node that
		// did not report leaves has_data absent, which is "unknown" and not
		// "drained".
		held := make(map[string]NodeDisk)

		if n.Live != nil {
			for _, d := range n.Live.Disks {
				held[d.ID] = d
			}
		}

		for _, d := range n.Disks {
			apiDisk := adminapi.ClusterDisk{ID: d.ID, Weight: d.Weight}

			if probe, ok := held[d.ID]; ok {
				if probe.Err != "" {
					apiDisk.DataError = adminapi.NewOptString(probe.Err)
				} else {
					apiDisk.HasData = adminapi.NewOptBool(probe.HasData)
				}

				// Occupancy is reported only once the node's index has been
				// anchored. Zeroes from an unanchored index would read as a
				// drained disk, which is the one reading that must never be
				// wrong.
				if probe.Counted {
					apiDisk.Fragments = adminapi.NewOptInt64(probe.Fragments)
					apiDisk.Bytes = adminapi.NewOptInt64(probe.Bytes)
				}
			}

			if d.TotalBytes > 0 {
				apiDisk.TotalBytes = adminapi.NewOptInt64(clampInt64(d.TotalBytes))
				apiDisk.FreeBytes = adminapi.NewOptInt64(clampInt64(d.FreeBytes))

				full := 1 - float64(d.FreeBytes)/float64(d.TotalBytes)
				apiDisk.Fullness = adminapi.NewOptFloat64(full)

				out.TotalBytes += clampInt64(d.TotalBytes)
				out.FreeBytes += clampInt64(d.FreeBytes)
				minFull = min(minFull, full)
				maxFull = max(maxFull, full)
			}

			apiNode.Disks = append(apiNode.Disks, apiDisk)
		}

		out.Nodes = append(out.Nodes, apiNode)
	}

	if maxFull >= minFull {
		out.PlacementSkew = maxFull - minFull
	}

	return out
}

// liveToAPI maps a node's live state to the wire schema.
func liveToAPI(l NodeLive) adminapi.ClusterNodeLive {
	state := l.RebalanceState
	if state == "" {
		state = RebalanceIdle
	}

	out := adminapi.ClusterNodeLive{
		RepairQueueDepth:   l.RepairQueueDepth,
		RebalanceState:     adminapi.RebalanceState(state),
		RebalanceObjects:   l.RebalanceObjects,
		RebalanceRelocated: l.RebalanceRelocated,
		RebalanceFailed:    l.RebalanceFailed,
		ScrubPasses:        l.ScrubPasses,
		ScrubObjects:       l.ScrubObjects,
		ScrubRepaired:      l.ScrubRepaired,
		ScrubFailed:        l.ScrubFailed,
		RebuiltFragments:   l.RebuiltFragments,
		SweptStale:         l.SweptStale,
		CorruptReplicas:    l.CorruptReplicas,
		ConvertedObjects:   l.Converted,
		EcUnverified:       l.ECUnverified,
	}

	// Coverage is reported only when the node could derive it. Zeroes would
	// read as "nothing held, everything just verified", which is the reading
	// these numbers exist to prevent.
	if l.Held > 0 {
		out.ObjectsHeld = adminapi.NewOptInt64(l.Held)
		out.NeverVerified = adminapi.NewOptInt64(l.NeverVerified)

		if !l.OldestVerified.IsZero() {
			out.OldestVerified = adminapi.NewOptDateTime(l.OldestVerified)
		}
	}

	if l.Version != "" {
		out.Version = adminapi.NewOptString(l.Version)
	}

	if l.SchemaVersion > 0 {
		out.SchemaVersion = adminapi.NewOptInt(l.SchemaVersion)
	}

	if l.UptimeSeconds > 0 {
		out.UptimeSeconds = adminapi.NewOptFloat64(l.UptimeSeconds)
	}

	if l.RebalanceErr != "" {
		out.RebalanceError = adminapi.NewOptString(l.RebalanceErr)
	}

	return out
}

// clampInt64 caps a byte count at the int64 range for the wire schema.
func clampInt64(v uint64) int64 {
	const maxInt64 = 1<<63 - 1
	if v > maxInt64 {
		return maxInt64
	}

	return int64(v)
}
