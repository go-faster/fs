package adminhandler

import (
	"context"

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

		out.DiskCount += len(n.Disks)

		for _, d := range n.Disks {
			apiDisk := adminapi.ClusterDisk{ID: d.ID, Weight: d.Weight}

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

// clampInt64 caps a byte count at the int64 range for the wire schema.
func clampInt64(v uint64) int64 {
	const maxInt64 = 1<<63 - 1
	if v > maxInt64 {
		return maxInt64
	}

	return int64(v)
}
