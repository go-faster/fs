package adminhandler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/adminapi"
)

// fakePlane answers whatever a test dictates and records what was asked of it.
type fakePlane struct {
	status   PlaneStatus
	err      error
	rebuilds int
}

func (p *fakePlane) Status(context.Context) (PlaneStatus, error) { return p.status, nil }

func (p *fakePlane) Rebuild(context.Context) error {
	p.rebuilds++

	if p.err != nil {
		return p.err
	}

	p.status.Rebuilding = true

	return nil
}

// TestPlaneStatusDisabledWithoutAPlane: a node that does not run the sharded
// plane must say so rather than report a plane that is not there.
func TestPlaneStatusDisabledWithoutAPlane(t *testing.T) {
	a := NewAdminAPI(Options{})

	got, err := a.GetMetadataPlaneStatus(t.Context())
	require.NoError(t, err)

	assert.Equal(t, adminapi.MetadataPlaneStateDisabled, got.State)
}

// TestRebuildRefusedWithoutAPlane: 501 rather than a silent success, because a
// rebuild an operator believes started and did not is the worst answer here.
func TestRebuildRefusedWithoutAPlane(t *testing.T) {
	a := NewAdminAPI(Options{})

	_, err := a.RebuildMetadataPlane(t.Context())

	var status *adminapi.ErrorStatusCode
	require.ErrorAs(t, err, &status)
	assert.Equal(t, http.StatusNotImplemented, status.StatusCode)
}

// TestPlaneStatusCarriesTheCause: the reason is what tells an operator whether
// to expect the cluster to fix itself, and it has to reach the wire.
func TestPlaneStatusCarriesTheCause(t *testing.T) {
	plane := &fakePlane{status: PlaneStatus{
		Cause:      "orphaned",
		Policy:     "on_failure",
		StartedAt:  time.Unix(1700000000, 0).UTC(),
		FinishedAt: time.Unix(1700000600, 0).UTC(),
		Err:        "etcd went away",
	}}

	a := NewAdminAPI(Options{Plane: plane})

	got, err := a.GetMetadataPlaneStatus(t.Context())
	require.NoError(t, err)

	assert.Equal(t, adminapi.MetadataPlaneStateBuilding, got.State)
	assert.Equal(t, adminapi.MetadataPlaneCauseOrphaned, got.Cause.Value)
	assert.Equal(t, "on_failure", got.Policy.Value)
	assert.Equal(t, "etcd went away", got.ErrorMessage.Value)
	assert.True(t, got.StartedAt.Set)
	assert.True(t, got.FinishedAt.Set)
}

// TestAReadyPlaneCarriesNoCause: a usable plane has no reason to be unusable,
// and reporting the last one would explain a state the cluster has left.
func TestAReadyPlaneCarriesNoCause(t *testing.T) {
	a := NewAdminAPI(Options{Plane: &fakePlane{status: PlaneStatus{Ready: true, Cause: "orphaned"}}})

	got, err := a.GetMetadataPlaneStatus(t.Context())
	require.NoError(t, err)

	assert.Equal(t, adminapi.MetadataPlaneStateReady, got.State)
	assert.False(t, got.Cause.Set, "a ready plane explained why it was not")
}

// TestRebuildReportsTheStatusItProduced: what an operator wants back is that a
// rebuild is now running, and the only thing that can say so is the runner —
// not the handler assuming its own request worked.
func TestRebuildReportsTheStatusItProduced(t *testing.T) {
	plane := &fakePlane{status: PlaneStatus{Cause: "never-built"}}
	a := NewAdminAPI(Options{Plane: plane})

	got, err := a.RebuildMetadataPlane(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, plane.rebuilds)
	assert.True(t, got.Rebuilding)
}

// TestRebuildConflictIsA409: the thing the caller asked for is already
// happening. That is an answer, not a failure, and 500 would have them retry.
func TestRebuildConflictIsA409(t *testing.T) {
	a := NewAdminAPI(Options{Plane: &fakePlane{err: ErrPlaneRebuildConflict}})

	_, err := a.RebuildMetadataPlane(t.Context())

	var status *adminapi.ErrorStatusCode
	require.ErrorAs(t, err, &status)
	assert.Equal(t, http.StatusConflict, status.StatusCode)
}

// TestRebuildFailureIsA500: anything else is this server's problem to report as
// one, so a caller can tell "already running" from "could not start".
func TestRebuildFailureIsA500(t *testing.T) {
	a := NewAdminAPI(Options{Plane: &fakePlane{err: errors.New("etcd unreachable")}})

	_, err := a.RebuildMetadataPlane(t.Context())

	var status *adminapi.ErrorStatusCode
	require.ErrorAs(t, err, &status)
	assert.Equal(t, http.StatusInternalServerError, status.StatusCode)
}
