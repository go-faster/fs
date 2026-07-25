package adminhandler

import (
	"context"
	"net/http"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/adminapi"
)

// ErrMigrationConflict marks a migration request that cannot run now — one is
// already in flight, or the cluster's schema is newer than this binary
// implements. It maps to 409.
var ErrMigrationConflict = errors.New("migration conflict")

// Migration is one pending schema migration.
type Migration struct {
	Version     int
	Description string
}

// MigrationStatus is the cluster's schema state as this process sees it.
type MigrationStatus struct {
	// ClusterVersion is the version recorded in etcd; 0 when none is recorded
	// yet (no node has joined).
	ClusterVersion int
	// BinaryVersion is the schema version this binary implements.
	BinaryVersion int
	// Pending are the migrations between the two, in order.
	Pending []Migration
	// Running reports an apply in flight on this process.
	Running bool
	// LastApplied and LastErr describe the most recent apply on this process.
	LastApplied []int
	LastErr     string
}

// MigrationControl reports and applies cluster schema migrations. Implemented
// by the cluster runtime and the headless admin; nil outside cluster mode (the
// endpoints then report "disabled" / refuse to apply).
type MigrationControl interface {
	// Status reads the cluster's schema version and computes what is pending.
	Status(ctx context.Context) (MigrationStatus, error)
	// Apply runs every pending migration under the cluster-wide election and
	// returns the resulting status. Returns ErrMigrationConflict when an apply
	// is already running or the cluster schema is newer than this binary.
	Apply(ctx context.Context) (MigrationStatus, error)
}

// GetMigrationStatus reports the cluster schema state and pending migrations.
func (a *AdminAPI) GetMigrationStatus(ctx context.Context) (*adminapi.MigrationStatus, error) {
	if a.opts.Migrations == nil {
		return &adminapi.MigrationStatus{State: adminapi.ClusterStateDisabled, Pending: []adminapi.Migration{}}, nil
	}

	st, err := a.opts.Migrations.Status(ctx)
	if err != nil {
		return nil, apiErr(http.StatusInternalServerError, err)
	}

	return migrationStatusToAPI(st), nil
}

// ApplyMigrations applies every pending migration.
func (a *AdminAPI) ApplyMigrations(ctx context.Context) (*adminapi.MigrationStatus, error) {
	ctl := a.opts.Migrations
	if ctl == nil {
		return nil, apiErr(http.StatusConflict, errors.New("not in cluster mode"))
	}

	st, err := ctl.Apply(ctx)
	if err != nil {
		if errors.Is(err, ErrMigrationConflict) {
			return nil, apiErr(http.StatusConflict, err)
		}

		return nil, apiErr(http.StatusInternalServerError, err)
	}

	return migrationStatusToAPI(st), nil
}

// migrationStatusToAPI maps the domain status to the wire schema.
func migrationStatusToAPI(st MigrationStatus) *adminapi.MigrationStatus {
	out := &adminapi.MigrationStatus{
		State:                adminapi.ClusterStateOk,
		ClusterSchemaVersion: st.ClusterVersion,
		BinarySchemaVersion:  st.BinaryVersion,
		// A cluster with no recorded version is not "up to date": no node has
		// joined yet, so there is nothing to migrate but also nothing agreed.
		UpToDate: st.ClusterVersion > 0 && len(st.Pending) == 0,
		Running:  st.Running,
		Pending:  make([]adminapi.Migration, 0, len(st.Pending)),
	}

	for _, m := range st.Pending {
		out.Pending = append(out.Pending, adminapi.Migration{Version: m.Version, Description: m.Description})
	}

	if len(st.LastApplied) > 0 {
		out.LastApplied = append([]int(nil), st.LastApplied...)
	}

	if st.LastErr != "" {
		out.LastError = adminapi.NewOptString(st.LastErr)
	}

	return out
}
