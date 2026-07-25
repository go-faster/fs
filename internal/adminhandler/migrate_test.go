package adminhandler

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/adminapi"
)

type stubMigrations struct {
	st        MigrationStatus
	statusErr error
	applyErr  error
	applied   int
}

func (s *stubMigrations) Status(context.Context) (MigrationStatus, error) {
	return s.st, s.statusErr
}

func (s *stubMigrations) Apply(context.Context) (MigrationStatus, error) {
	if s.applyErr != nil {
		return MigrationStatus{}, s.applyErr
	}

	s.applied++
	s.st.Pending = nil
	s.st.ClusterVersion = s.st.BinaryVersion
	s.st.LastApplied = []int{s.st.BinaryVersion}

	return s.st, nil
}

func TestMigrationStatusDisabledWithoutControl(t *testing.T) {
	a := NewAdminAPI(Options{})

	st, err := a.GetMigrationStatus(t.Context())
	require.NoError(t, err)
	assert.Equal(t, adminapi.ClusterStateDisabled, st.State)
	assert.Empty(t, st.Pending)

	_, err = a.ApplyMigrations(t.Context())
	requireStatusCode(t, err, http.StatusConflict)
}

func TestMigrationStatusReportsPending(t *testing.T) {
	ctl := &stubMigrations{st: MigrationStatus{
		ClusterVersion: 1,
		BinaryVersion:  3,
		Pending: []Migration{
			{Version: 2, Description: "split sidecars"},
			{Version: 3, Description: "hash bucket namespaces"},
		},
	}}

	a := NewAdminAPI(Options{Migrations: ctl})

	st, err := a.GetMigrationStatus(t.Context())
	require.NoError(t, err)

	assert.Equal(t, adminapi.ClusterStateOk, st.State)
	assert.Equal(t, 1, st.ClusterSchemaVersion)
	assert.Equal(t, 3, st.BinarySchemaVersion)
	assert.False(t, st.UpToDate)
	require.Len(t, st.Pending, 2)
	assert.Equal(t, 2, st.Pending[0].Version)
	assert.Equal(t, "hash bucket namespaces", st.Pending[1].Description)
}

// TestMigrationStatusUpToDate: nothing pending on a joined cluster reads as
// up to date; a cluster with no recorded version does not (no node has joined,
// so there is nothing to be up to date with).
func TestMigrationStatusUpToDate(t *testing.T) {
	a := NewAdminAPI(Options{Migrations: &stubMigrations{st: MigrationStatus{ClusterVersion: 1, BinaryVersion: 1}}})

	st, err := a.GetMigrationStatus(t.Context())
	require.NoError(t, err)
	assert.True(t, st.UpToDate)

	a = NewAdminAPI(Options{Migrations: &stubMigrations{st: MigrationStatus{BinaryVersion: 1}}})

	st, err = a.GetMigrationStatus(t.Context())
	require.NoError(t, err)
	assert.False(t, st.UpToDate)
	assert.Equal(t, 0, st.ClusterSchemaVersion)
}

func TestApplyMigrations(t *testing.T) {
	ctl := &stubMigrations{st: MigrationStatus{
		ClusterVersion: 1,
		BinaryVersion:  2,
		Pending:        []Migration{{Version: 2, Description: "split sidecars"}},
	}}

	a := NewAdminAPI(Options{Migrations: ctl})

	st, err := a.ApplyMigrations(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, ctl.applied)
	assert.Equal(t, 2, st.ClusterSchemaVersion)
	assert.True(t, st.UpToDate)
	assert.Empty(t, st.Pending)
	assert.Equal(t, []int{2}, st.LastApplied)
}

// TestApplyMigrationsConflict: an apply that cannot run now (one in flight, or
// a cluster newer than this binary) is a 409, not a 500 — an orchestrator
// retries it rather than treating the cluster as broken.
func TestApplyMigrationsConflict(t *testing.T) {
	a := NewAdminAPI(Options{Migrations: &stubMigrations{
		applyErr: errors.Wrap(ErrMigrationConflict, "migration already running"),
	}})

	_, err := a.ApplyMigrations(t.Context())
	requireStatusCode(t, err, http.StatusConflict)
}

func TestApplyMigrationsPropagatesError(t *testing.T) {
	a := NewAdminAPI(Options{Migrations: &stubMigrations{applyErr: assert.AnError}})

	_, err := a.ApplyMigrations(t.Context())
	requireStatusCode(t, err, http.StatusInternalServerError)

	a = NewAdminAPI(Options{Migrations: &stubMigrations{statusErr: assert.AnError}})

	_, err = a.GetMigrationStatus(t.Context())
	requireStatusCode(t, err, http.StatusInternalServerError)
}
