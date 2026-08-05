package adminhandler

import (
	"context"
	"net/http"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/adminapi"
)

// ErrPlaneRebuildConflict marks a rebuild asked for while this node is already
// running one; it maps to 409.
var ErrPlaneRebuildConflict = errors.New("a metadata plane rebuild is already running on this node")

// PlaneStatus is a snapshot of the sharded metadata plane as one node sees it.
type PlaneStatus struct {
	// Ready reports that listings are served from the plane rather than from
	// walking sidecars.
	Ready bool
	// Cause is why it is not, matching the admin API's enum. Empty when ready.
	Cause string
	// Rebuilding reports that this node is running a rebuild right now.
	Rebuilding bool
	// Policy is the configured automatic-rebuild policy.
	Policy string
	// StartedAt/FinishedAt frame this node's current or last rebuild.
	StartedAt, FinishedAt time.Time
	// Err is why this node's last rebuild failed.
	Err string
}

// PlaneControl is the sharded metadata plane behind the admin API. Implemented
// by the cluster runtime; absent (nil) when the node does not run the plane.
type PlaneControl interface {
	// Status snapshots the plane and this node's rebuild runner.
	Status(ctx context.Context) (PlaneStatus, error)
	// Rebuild starts the cluster-wide rebuild now, whatever the configured
	// policy. Returns ErrPlaneRebuildConflict when this node is already
	// running one.
	//
	// Returns as soon as the rebuild is launched. The walk is hours on a
	// cluster of any size, so a request that waited for it would time out long
	// before it finished, and an operator would have no way to tell a rebuild
	// that was running from one that never started.
	Rebuild(ctx context.Context) error
}

// GetMetadataPlaneStatus implements getMetadataPlaneStatus.
func (a *AdminAPI) GetMetadataPlaneStatus(ctx context.Context) (*adminapi.MetadataPlaneStatus, error) {
	if a.opts.Plane == nil {
		return &adminapi.MetadataPlaneStatus{State: adminapi.MetadataPlaneStateDisabled}, nil
	}

	got, err := a.opts.Plane.Status(ctx)
	if err != nil {
		return nil, apiErr(http.StatusInternalServerError, err)
	}

	return planeStatus(got), nil
}

// RebuildMetadataPlane implements rebuildMetadataPlane.
func (a *AdminAPI) RebuildMetadataPlane(ctx context.Context) (*adminapi.MetadataPlaneStatus, error) {
	if a.opts.Plane == nil {
		return nil, apiErr(http.StatusNotImplemented,
			errors.New("this server does not run the sharded metadata plane"))
	}

	if err := a.opts.Plane.Rebuild(ctx); err != nil {
		if errors.Is(err, ErrPlaneRebuildConflict) {
			// A conflict rather than a failure: the thing the caller asked for
			// is already happening, and the right response is to say so.
			return nil, apiErr(http.StatusConflict, err)
		}

		return nil, apiErr(http.StatusInternalServerError, err)
	}

	// Read back rather than assumed. What an operator wants to see is that a
	// rebuild is now running on this node, and the only thing that can say so
	// is the runner.
	got, err := a.opts.Plane.Status(ctx)
	if err != nil {
		return nil, apiErr(http.StatusInternalServerError, err)
	}

	return planeStatus(got), nil
}

// planeStatus renders a snapshot for the wire.
func planeStatus(s PlaneStatus) *adminapi.MetadataPlaneStatus {
	out := &adminapi.MetadataPlaneStatus{
		State:      adminapi.MetadataPlaneStateBuilding,
		Rebuilding: s.Rebuilding,
	}

	if s.Ready {
		out.State = adminapi.MetadataPlaneStateReady
	}

	if !s.Ready && s.Cause != "" {
		out.Cause = adminapi.NewOptMetadataPlaneCause(adminapi.MetadataPlaneCause(s.Cause))
	}

	if s.Policy != "" {
		out.Policy = adminapi.NewOptString(s.Policy)
	}

	if !s.StartedAt.IsZero() {
		out.StartedAt = adminapi.NewOptDateTime(s.StartedAt)
	}

	if !s.FinishedAt.IsZero() {
		out.FinishedAt = adminapi.NewOptDateTime(s.FinishedAt)
	}

	if s.Err != "" {
		out.ErrorMessage = adminapi.NewOptString(s.Err)
	}

	return out
}
