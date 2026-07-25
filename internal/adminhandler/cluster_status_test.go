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
