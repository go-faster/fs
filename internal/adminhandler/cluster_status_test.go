package adminhandler

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/adminapi"
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
