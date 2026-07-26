package adminhandler

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/adminapi"
)

type stubClusterStatus struct {
	st  ClusterStatus
	err error
}

func (s stubClusterStatus) ClusterStatus(context.Context) (ClusterStatus, error) {
	return s.st, s.err
}

func TestClusterStatusDisabledWithoutSource(t *testing.T) {
	a := NewAdminAPI(Options{})

	st, err := a.GetClusterStatus(t.Context())
	require.NoError(t, err)
	assert.Equal(t, adminapi.ClusterStateDisabled, st.State)
}

func TestClusterStatusMapsAndAggregates(t *testing.T) {
	src := stubClusterStatus{st: ClusterStatus{
		SchemaVersion:         1,
		BinarySchemaVersion:   1,
		RebalanceRunning:      true,
		RebalanceCursorBucket: "b",
		Cursor:                "k9",
		Nodes: []ClusterNode{
			{ID: "n0", Addr: "10.0.0.1:7080", Rack: "r0", Disks: []ClusterDisk{
				{ID: "d0", Weight: 1, TotalBytes: 1000, FreeBytes: 900}, // 10% full
			}},
			{ID: "n1", Rack: "r1", Disks: []ClusterDisk{
				{ID: "d0", Weight: 2, TotalBytes: 1000, FreeBytes: 100}, // 90% full
				{ID: "d1", Weight: 1}, // capacity unknown
			}},
		},
	}}

	a := NewAdminAPI(Options{ClusterStatus: src})

	st, err := a.GetClusterStatus(t.Context())
	require.NoError(t, err)

	assert.Equal(t, adminapi.ClusterStateOk, st.State)
	assert.Equal(t, 1, st.SchemaVersion)
	assert.Equal(t, 2, st.NodeCount)
	assert.Equal(t, 3, st.DiskCount)
	assert.Equal(t, int64(2000), st.TotalBytes)
	assert.Equal(t, int64(1000), st.FreeBytes)
	assert.InDelta(t, 0.8, st.PlacementSkew, 1e-9) // 0.9 - 0.1
	assert.True(t, st.RebalanceRunning)
	assert.Equal(t, "b", st.RebalanceCursorBucket.Or(""))
	assert.Equal(t, "k9", st.RebalanceCursorKey.Or(""))

	// The capacity-unknown disk carries no bytes/fullness.
	require.Len(t, st.Nodes, 2)
	assert.Equal(t, "10.0.0.1:7080", st.Nodes[0].Addr.Or(""))
	require.Len(t, st.Nodes[1].Disks, 2)
	assert.False(t, st.Nodes[1].Disks[1].TotalBytes.Set)
	assert.False(t, st.Nodes[1].Disks[1].Fullness.Set)
}

// TestClusterStatusCarriesLiveState: live state reported by the nodes lands on
// each node and in the cluster aggregates, and a node that did not report is
// counted as such instead of silently reading as an idle, empty node.
func TestClusterStatusCarriesLiveState(t *testing.T) {
	src := stubClusterStatus{st: ClusterStatus{
		Nodes: []ClusterNode{
			{ID: "n0", Live: &NodeLive{
				Version:          "v1.2.3",
				SchemaVersion:    1,
				UptimeSeconds:    30,
				RepairQueueDepth: 4,
				RebalanceState:   RebalanceRunning,
				RebalanceObjects: 10,
				RebuiltFragments: 2,
				CorruptReplicas:  1,
				ECUnverified:     true,
			}},
			{ID: "n1", Live: &NodeLive{RepairQueueDepth: 3}},
			{ID: "n2", LiveError: "dial tcp: connection refused"},
		},
	}}

	a := NewAdminAPI(Options{ClusterStatus: src})

	st, err := a.GetClusterStatus(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 7, st.RepairQueueDepth)
	assert.Equal(t, 2, st.NodesReporting)
	assert.Equal(t, 1, st.NodesNotReporting)

	require.Len(t, st.Nodes, 3)
	require.True(t, st.Nodes[0].Live.Set)
	assert.Equal(t, "v1.2.3", st.Nodes[0].Live.Value.Version.Or(""))
	assert.Equal(t, adminapi.RebalanceStateRunning, st.Nodes[0].Live.Value.RebalanceState)
	assert.Equal(t, int64(2), st.Nodes[0].Live.Value.RebuiltFragments)
	assert.True(t, st.Nodes[0].Live.Value.EcUnverified)

	// A node with no runner reports idle, not the empty string.
	assert.Equal(t, adminapi.RebalanceStateIdle, st.Nodes[1].Live.Value.RebalanceState)

	assert.False(t, st.Nodes[2].Live.Set)
	assert.Equal(t, "dial tcp: connection refused", st.Nodes[2].LiveError.Or(""))
}

// TestClusterStatusMergesDiskDrainState covers the drain signal an
// orchestrator gates a decommission on. Capacity comes from the control plane
// and what a disk holds only from the node, and the two are merged onto one
// disk — so the rule that matters is which states leave has_data absent:
// absent is unknown, and unknown must never be read as drained.
func TestClusterStatusMergesDiskDrainState(t *testing.T) {
	src := stubClusterStatus{st: ClusterStatus{
		Nodes: []ClusterNode{
			{
				ID: "n0",
				Disks: []ClusterDisk{
					{ID: "d0", Weight: 1, TotalBytes: 100, FreeBytes: 40},
					{ID: "d1", Weight: -1, TotalBytes: 100, FreeBytes: 99},
					{ID: "d2", Weight: 1},
				},
				Live: &NodeLive{Disks: []NodeDisk{
					{ID: "d0", HasData: true},
					// Drained: weight is negative and the data has moved off.
					{ID: "d1", HasData: false},
					// Probed and failed: unknown, not drained.
					{ID: "d2", Err: "open /mnt/d2: input/output error"},
				}},
			},
			// A node that did not report at all: every disk is unknown.
			{
				ID:        "n1",
				Disks:     []ClusterDisk{{ID: "d0", Weight: 1}},
				LiveError: "dial tcp: connection refused",
			},
		},
	}}

	a := NewAdminAPI(Options{ClusterStatus: src})

	st, err := a.GetClusterStatus(t.Context())
	require.NoError(t, err)

	require.Len(t, st.Nodes, 2)
	disks := st.Nodes[0].Disks
	require.Len(t, disks, 3)

	// Still holding fragments.
	hasData, ok := disks[0].HasData.Get()
	require.True(t, ok, "a reported disk must carry a verdict")
	assert.True(t, hasData)
	// Capacity is merged onto the same disk, not replaced by the probe.
	assert.Equal(t, int64(100), disks[0].TotalBytes.Or(0))

	// Drained: the volume can be deleted.
	hasData, ok = disks[1].HasData.Get()
	require.True(t, ok)
	assert.False(t, hasData)

	// A disk the node could not probe says why, and stays absent — reading it
	// as drained is how a volume still holding the only copy gets deleted.
	_, ok = disks[2].HasData.Get()
	assert.False(t, ok, "an unprobeable disk must not carry a verdict")
	assert.Equal(t, "open /mnt/d2: input/output error", disks[2].DataError.Or(""))

	// A silent node leaves its disks unknown too.
	_, ok = st.Nodes[1].Disks[0].HasData.Get()
	assert.False(t, ok, "a node that did not report leaves has_data absent")
}

func TestClusterStatusPropagatesError(t *testing.T) {
	a := NewAdminAPI(Options{ClusterStatus: stubClusterStatus{err: assert.AnError}})

	_, err := a.GetClusterStatus(t.Context())
	requireStatusCode(t, err, http.StatusInternalServerError)
}

func TestAccessKeyEndpointsUnavailableWithoutManager(t *testing.T) {
	a := NewAdminAPI(Options{}) // no Manager (headless)

	_, err := a.ListAccessKeys(t.Context())
	requireStatusCode(t, err, http.StatusNotImplemented)

	_, err = a.CreateAccessKey(t.Context(), &adminapi.CreateAccessKeyRequest{})
	requireStatusCode(t, err, http.StatusNotImplemented)

	err = a.DeleteAccessKey(t.Context(), adminapi.DeleteAccessKeyParams{AccessKey: "x"})
	requireStatusCode(t, err, http.StatusNotImplemented)
}

// TestClusterStatusReportsDiskOccupancy covers the progress half of a drain:
// how much is left to move. The rule mirrors has_data — an index that has not
// been anchored yet reports nothing rather than a confident zero, because a
// zero here reads as "done".
func TestClusterStatusReportsDiskOccupancy(t *testing.T) {
	src := stubClusterStatus{st: ClusterStatus{
		Nodes: []ClusterNode{
			{
				ID: "n0",
				Disks: []ClusterDisk{
					{ID: "d0", Weight: -1, TotalBytes: 1000, FreeBytes: 400},
					{ID: "d1", Weight: -1},
					{ID: "d2", Weight: 1},
				},
				Live: &NodeLive{Disks: []NodeDisk{
					// Draining, with the remaining work attached.
					{ID: "d0", HasData: true, Fragments: 12, Bytes: 4096, Counted: true},
					// Drained, and counted: the two agree.
					{ID: "d1", HasData: false, Fragments: 0, Bytes: 0, Counted: true},
					// Holding data, but the index has not been anchored yet.
					{ID: "d2", HasData: true},
				}},
			},
		},
	}}

	a := NewAdminAPI(Options{ClusterStatus: src})

	st, err := a.GetClusterStatus(t.Context())
	require.NoError(t, err)

	disks := st.Nodes[0].Disks
	require.Len(t, disks, 3)

	assert.Equal(t, int64(12), disks[0].Fragments.Or(0))
	assert.Equal(t, int64(4096), disks[0].Bytes.Or(0))
	// Occupancy is payload only; capacity still comes from statfs.
	assert.Equal(t, int64(1000), disks[0].TotalBytes.Or(0))

	fragments, ok := disks[1].Fragments.Get()
	require.True(t, ok, "a counted, empty disk reports zero rather than nothing")
	assert.Zero(t, fragments)

	_, ok = disks[2].Fragments.Get()
	assert.False(t, ok, "an unanchored index must not report a count")
	_, ok = disks[2].Bytes.Get()
	assert.False(t, ok)

	hasData, ok := disks[2].HasData.Get()
	require.True(t, ok, "the drain verdict is independent of the index")
	assert.True(t, hasData)
}
