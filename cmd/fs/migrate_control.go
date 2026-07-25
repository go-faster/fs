package main

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// migrateApplyTimeout bounds one apply through the admin API, including the
// wait for the cluster-wide migrate election. Migrations are resumable, so a
// run cut short here is continued by the next apply — but a migration expected
// to outlast this belongs on the CLI (`fs cluster migrate`), which waits as
// long as the operator does.
const migrateApplyTimeout = 10 * time.Minute

// migrateController is the admin API's schema-migration control: the same
// elected, resumable walk as `fs cluster migrate`, driven from a node's admin
// listener or the headless `fs admin`. At most one apply runs per process
// (and, through the etcd election, one cluster-wide).
type migrateController struct {
	client     *clientv3.Client
	etcdCfg    etcd.Config
	candidate  string
	migrations []etcd.Migration
	// binaryVersion is the schema version this binary implements; a field so
	// tests can exercise a pending migration against the founding version.
	binaryVersion int

	mu          sync.Mutex
	running     bool
	lastApplied []int
	lastErr     string
}

// newMigrateController builds the controller; candidate labels this process in
// the migrate election.
func newMigrateController(client *clientv3.Client, etcdCfg etcd.Config, candidate string, migrations []etcd.Migration) *migrateController {
	return &migrateController{
		client:        client,
		etcdCfg:       etcdCfg,
		candidate:     candidate,
		migrations:    migrations,
		binaryVersion: etcd.SchemaVersion,
	}
}

var _ adminhandler.MigrationControl = (*migrateController)(nil)

// Status implements adminhandler.MigrationControl.
func (c *migrateController) Status(ctx context.Context) (adminhandler.MigrationStatus, error) {
	clusterVersion, ok, err := etcd.LoadSchemaVersion(ctx, c.client, c.etcdCfg)
	if err != nil {
		return adminhandler.MigrationStatus{}, errors.Wrap(err, "load schema version")
	}

	if !ok {
		// No node has joined yet: nothing is recorded, so nothing is pending.
		clusterVersion = 0
	}

	st := adminhandler.MigrationStatus{
		ClusterVersion: clusterVersion,
		BinaryVersion:  c.binaryVersion,
	}

	if ok {
		st.Pending = c.pending(clusterVersion)
	}

	c.mu.Lock()
	st.Running = c.running
	st.LastApplied = append([]int(nil), c.lastApplied...)
	st.LastErr = c.lastErr
	c.mu.Unlock()

	return st, nil
}

// pending lists the migrations between the cluster's version and this binary's,
// in order.
func (c *migrateController) pending(clusterVersion int) []adminhandler.Migration {
	var out []adminhandler.Migration

	for _, m := range c.migrations {
		if m.Version() > clusterVersion && m.Version() <= c.binaryVersion {
			out = append(out, adminhandler.Migration{Version: m.Version(), Description: m.Description()})
		}
	}

	// RunMigrations sorts its own copy; report them in the order it applies.
	slices.SortFunc(out, func(a, b adminhandler.Migration) int { return a.Version - b.Version })

	return out
}

// Apply implements adminhandler.MigrationControl.
func (c *migrateController) Apply(ctx context.Context) (adminhandler.MigrationStatus, error) {
	st, err := c.Status(ctx)
	if err != nil {
		return adminhandler.MigrationStatus{}, err
	}

	switch {
	case st.Running:
		return adminhandler.MigrationStatus{}, errors.Wrap(adminhandler.ErrMigrationConflict, "a migration is already running on this process")
	case st.ClusterVersion == 0:
		return adminhandler.MigrationStatus{}, errors.Wrap(adminhandler.ErrMigrationConflict, "no schema version recorded yet; a node must join the cluster first")
	case st.ClusterVersion > st.BinaryVersion:
		return adminhandler.MigrationStatus{}, errors.Wrapf(adminhandler.ErrMigrationConflict,
			"cluster schema v%d is newer than this binary implements (v%d)", st.ClusterVersion, st.BinaryVersion)
	case len(st.Pending) == 0:
		return st, nil // Up to date: don't campaign for nothing.
	}

	if !c.begin() {
		return adminhandler.MigrationStatus{}, errors.Wrap(adminhandler.ErrMigrationConflict, "a migration is already running on this process")
	}

	// Detached from the request: a client that hangs up mid-migration must not
	// cut the run short, and the election needs a context that outlives the
	// HTTP handler's cancellation.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migrateApplyTimeout)
	defer cancel()

	applied, err := etcd.RunMigrations(runCtx, c.client, c.etcdCfg, c.binaryVersion, c.candidate, c.migrations)

	c.finish(applied, err)

	if err != nil {
		return adminhandler.MigrationStatus{}, errors.Wrap(err, "run migrations")
	}

	return c.Status(ctx)
}

// begin claims the single-flight apply slot.
func (c *migrateController) begin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.running {
		return false
	}

	c.running = true
	c.lastApplied = nil
	c.lastErr = ""

	return true
}

// finish releases the slot and records the run's outcome.
func (c *migrateController) finish(applied []int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.running = false
	c.lastApplied = applied

	if err != nil {
		c.lastErr = err.Error()
	}
}
