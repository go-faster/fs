package etcd

import (
	"context"
	"sort"
	"strconv"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// Migration upgrades the cluster from schema version Version()-1 to Version().
// Apply must be idempotent: the migrator records progress only at whole-version
// granularity (the schema-version key bumps after Apply returns), so a crash
// mid-Apply re-runs the whole migration on resume. A migration that mutates a
// lot may keep its own finer resume cursor in etcd, but must tolerate being
// restarted from the beginning.
type Migration interface {
	// Version is the schema version this migration produces. Migrations must
	// form a contiguous run (2, 3, 4, …) up to SchemaVersion.
	Version() int
	// Description is a short human-readable summary for logs and the CLI.
	Description() string
	// Apply performs the migration. It runs under the cluster-wide migrate
	// election, so it is the only migrator active in the cluster.
	Apply(ctx context.Context) error
}

// migrateElectionPrefix is the election namespace for the single migrator.
func (c Config) migrateElectionPrefix() string { return c.Prefix + "/migrate/leader" }

// RunMigrations applies every pending migration in order under a cluster-wide
// election, bumping the recorded schema version after each. It is resumable:
// on restart it re-reads the recorded version and continues from the next
// unapplied migration. Returns the versions applied.
//
// migrations may be unsorted; the set actually applied is those with
// Version() in (clusterVersion, binaryVersion], and they must be contiguous.
// A binary is never allowed to apply a migration beyond the version it
// implements, and never runs against a cluster already newer than itself
// (ErrSchemaTooNew).
func RunMigrations(ctx context.Context, client *clientv3.Client, cfg Config, binaryVersion int, candidate string, migrations []Migration) (applied []int, err error) {
	cfg = cfg.withDefaults()

	pending := append([]Migration(nil), migrations...)
	sort.Slice(pending, func(i, j int) bool { return pending[i].Version() < pending[j].Version() })

	session, err := concurrency.NewSession(client, concurrency.WithTTL(int(cfg.TTL)))
	if err != nil {
		return nil, errors.Wrap(err, "migrate election session")
	}

	defer func() { _ = session.Close() }()

	election := concurrency.NewElection(session, cfg.migrateElectionPrefix())
	if err := election.Campaign(ctx, candidate); err != nil {
		return nil, errors.Wrap(err, "campaign migrate leadership")
	}

	defer func() {
		resignCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()

		_ = election.Resign(resignCtx)
	}()

	cur, ok, err := loadSchemaVersion(ctx, client, cfg.schemaVersionKey())
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, errors.New("no schema version recorded; a node must join first (EnsureCompatible)")
	}

	if cur > binaryVersion {
		return nil, errors.Wrapf(ErrSchemaTooNew, "cluster at v%d, binary implements v%d", cur, binaryVersion)
	}

	for _, m := range pending {
		if m.Version() <= cur {
			continue // Already applied.
		}

		if m.Version() > binaryVersion {
			break // Beyond what this binary implements.
		}

		if m.Version() != cur+1 {
			return applied, errors.Errorf("migration to v%d is not contiguous with cluster v%d", m.Version(), cur)
		}

		if err := m.Apply(ctx); err != nil {
			return applied, errors.Wrapf(err, "apply migration to v%d", m.Version())
		}

		if err := setSchemaVersionFenced(ctx, session, election, cfg, m.Version()); err != nil {
			return applied, err
		}

		cur = m.Version()
		applied = append(applied, m.Version())
	}

	return applied, nil
}

// setSchemaVersionFenced writes the schema version, fenced on still holding the
// migrate election — a deposed migrator's late bump cannot clobber the new
// leader's progress.
func setSchemaVersionFenced(ctx context.Context, session *concurrency.Session, election *concurrency.Election, cfg Config, version int) error {
	resp, err := session.Client().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(election.Key()), "=", election.Rev())).
		Then(clientv3.OpPut(cfg.schemaVersionKey(), strconv.Itoa(version))).
		Commit()
	if err != nil {
		return errors.Wrapf(err, "record schema version %d", version)
	}

	if !resp.Succeeded {
		return errors.New("migrate leadership lost")
	}

	return nil
}
