package pgstore_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/metastore/metastoretest"
	"github.com/go-faster/fs/internal/cluster/pgstore"
)

const (
	dbName     = "fs_test"
	dbUser     = "fs_test"
	dbPassword = "fs_test"
)

// dsn is the connection string for the container started by TestMain, empty
// when there is none.
var dsn atomic.Pointer[string]

// TestMain starts one PostgreSQL container for the whole package.
//
// One container, many schemas: starting a container per test would dominate the
// runtime, and the isolation each test actually needs is its own tables, which
// a schema gives.
func TestMain(m *testing.M) {
	// The container runtime is not available everywhere a contributor might run
	// the suite. Skipping is handled per-test rather than here so that `go test
	// ./...` stays green without Docker while CI, which has it, runs everything.
	if runtime.GOOS == "linux" {
		ctx := context.Background()

		if endpoint, terminate, err := startPostgres(ctx); err == nil {
			dsn.Store(&endpoint)

			code := m.Run()

			terminate()
			//nolint:gocritic // The deferred terminate has already run.
			exit(code)
		}
	}

	exit(m.Run())
}

// startPostgres brings up the container and returns its DSN.
func startPostgres(ctx context.Context) (uri string, stop func(), err error) {
	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "postgres:17",
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_DB":       dbName,
				"POSTGRES_USER":     dbUser,
				"POSTGRES_PASSWORD": dbPassword,
				// Deliberately a locale whose collation is NOT byte order.
				//
				// The alpine image defaults to a C-like locale, which made the
				// byte-ordering cases pass whether or not the schema asked for
				// COLLATE "C" — green for the wrong reason, and no protection at
				// all for an operator whose database was created with a language
				// locale. Here the default collation actively disagrees with byte
				// order, so those cases fail if the collation is ever dropped.
				"POSTGRES_INITDB_ARGS": "--locale=en_US.utf8",
			},
			// Twice, because the entrypoint starts the server once to run the
			// init scripts and then again for real; waiting on the first would
			// race the restart.
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	}

	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		return "", nil, err
	}

	stop = func() { _ = container.Terminate(context.Background()) }

	endpoint, err := container.PortEndpoint(ctx, "5432", "")
	if err != nil {
		stop()

		return "", nil, err
	}

	dsn := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(dbUser, dbPassword),
		Host:     endpoint,
		Path:     dbName,
		RawQuery: "sslmode=disable",
	}

	return dsn.String(), stop, nil
}

// schemas counts test schemas within a run, so parallel subtests never share
// tables.
var schemas atomic.Int64

// open returns a migrated store on a schema of its own, dropped afterwards.
func open(t testing.TB) *pgstore.Store {
	t.Helper()

	uri := dsn.Load()
	if uri == nil {
		t.Skip("no container runtime available; skipping the PostgreSQL suite")

		return nil
	}

	ctx := context.Background()
	name := fmt.Sprintf("fs_test_%d", schemas.Add(1))

	admin, err := pgxpool.New(ctx, *uri)
	require.NoError(t, err)

	defer admin.Close()

	_, err = admin.Exec(ctx, "CREATE SCHEMA "+name)
	require.NoError(t, err)

	t.Cleanup(func() {
		cleanup, err := pgxpool.New(context.Background(), *uri)
		if err != nil {
			return
		}

		defer cleanup.Close()

		_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+name+" CASCADE")
	})

	// search_path on the DSN, so every connection the pool opens later lands in
	// this test's schema too — and so migrate, which opens its own, agrees.
	scoped := *uri + "&search_path=" + name

	require.NoError(t, pgstore.Migrate(ctx, scoped))

	cfg, err := pgxpool.ParseConfig(scoped)
	require.NoError(t, err)

	// Counting queries at the client rather than from pg_stat_user_tables:
	// the statistics collector is asynchronous, so a before/after read of it
	// can settle on a stale value and report a delta of zero for a query that
	// definitely ran. A tracer counts exactly what the store issued, when it
	// issued it.
	counter := &queryCounter{}
	cfg.ConnConfig.Tracer = counter

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)

	t.Cleanup(pool.Close)

	store := pgstore.New(pool)
	counters.Store(store, counter)

	return store
}

// counters maps a store to the tracer counting its queries.
var counters sync.Map

// queryCounter counts every statement issued on a connection.
type queryCounter struct{ n atomic.Int64 }

func (c *queryCounter) TraceQueryStart(
	ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData,
) context.Context {
	c.n.Add(1)

	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// queriesDuring reports how many statements the store issued while fn ran.
func queriesDuring(t *testing.T, store *pgstore.Store, fn func()) int64 {
	t.Helper()

	value, ok := counters.Load(store)
	require.True(t, ok, "store was not opened by this package")

	counter, ok := value.(*queryCounter)
	require.True(t, ok)

	before := counter.n.Load()

	fn()

	return counter.n.Load() - before
}

// TestConformance is the reason this backend exists: held to the same contract
// as pebble, by the same suite, with no arm of the matrix skipped.
func TestConformance(t *testing.T) {
	metastoretest.Run(t, func(t testing.TB) metastore.Store { return open(t) })
}

// TestScopeIsCluster pins what this backend answers, which is the whole
// difference from objindex: one row per object for the cluster, so a caller
// scans once instead of merging N per-node results.
func TestScopeIsCluster(t *testing.T) {
	assert.Equal(t, metastore.ScopeCluster, open(t).Scope())
}

// TestScanIssuesOneQuery is E2's headline acceptance criterion, checked against
// the database's own statistics rather than against our belief about the SQL.
//
// A listing page costing one query is the entire justification for cluster
// scope, and it is easy to lose without noticing: a helper that reads a count
// first, or a prefix expressed as LIKE, would each still return correct results.
func TestScanIssuesOneQuery(t *testing.T) {
	store := open(t)

	for _, key := range []string{"a", "b", "c", "docs/one", "docs/two"} {
		require.NoError(t, store.Put(t.Context(), metastoretest.Entry("photos", key, 1, 1)))
	}

	for _, tc := range []struct {
		name          string
		prefix, after string
		limit         int
	}{
		{name: "whole bucket"},
		{name: "prefix", prefix: "docs/"},
		{name: "cursor", after: "b"},
		{name: "limit", limit: 2},
		{name: "prefix and cursor and limit", prefix: "docs/", after: "docs/one", limit: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issued := queriesDuring(t, store, func() {
				require.NoError(t, store.Scan(t.Context(), "photos", tc.prefix, tc.after, tc.limit,
					func(metastore.Entry) error { return nil }))
			})

			assert.Equal(t, int64(1), issued, "a listing page must cost exactly one query")
		})
	}
}

// TestPrefixIsARangeNotALike is what keeps Scan one *cheap* query rather than
// merely one query: a LIKE cannot use the primary key's btree for a bound, so
// it degrades into reading the bucket and filtering — correct, and O(bucket)
// per page.
func TestPrefixIsARangeNotALike(t *testing.T) {
	store := open(t)

	for i := range 200 {
		key := fmt.Sprintf("docs/%04d", i)
		require.NoError(t, store.Put(t.Context(), metastoretest.Entry("photos", key, 1, 1)))
	}

	// Asked with sequential scans disabled, so the answer is whether the
	// predicate *can* be served by the index rather than whether the planner
	// happens to prefer it at this size. On a table this small it never would;
	// at 10⁹ rows the choice is not close, and that is the case being pinned.
	tx, err := store.Pool().Begin(t.Context())
	require.NoError(t, err)

	defer func() { _ = tx.Rollback(t.Context()) }()

	_, err = tx.Exec(t.Context(), "SET LOCAL enable_seqscan = off")
	require.NoError(t, err)

	var plan strings.Builder

	rows, err := tx.Query(t.Context(), `
		EXPLAIN SELECT key FROM objects
		WHERE bucket = $1 AND key >= $2 AND key < $3 ORDER BY key`,
		"photos", "docs/", "docs0")
	require.NoError(t, err)

	defer rows.Close()

	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan.WriteString(line + "\n")
	}

	require.NoError(t, rows.Err())

	// Asserting on the node name would be brittle — this plans as an Index Only
	// Scan today and could legitimately become an Index Scan. What must hold is
	// where the predicates end up:
	//
	//   Index Cond: bucket = ... AND key >= ... AND key < ...
	//
	// every bound in the index condition, so the btree is positioned; nothing in
	// a Filter, which would mean rows are read and discarded; and no Sort, which
	// would mean the index's order is not the order asked for — the symptom a
	// wrong collation produces.
	got := plan.String()

	assert.Contains(t, got, "Index Cond", "the bounds must position the index:\n%s", got)
	assert.Contains(t, got, `key >= 'docs/'`, "the prefix lower bound belongs in the index condition:\n%s", got)
	assert.Contains(t, got, `key < 'docs0'`, "the prefix upper bound belongs in the index condition:\n%s", got)
	assert.NotContains(t, got, "Filter",
		"a filter means rows are read and discarded, which is O(bucket) per page:\n%s", got)
	assert.NotContains(t, got, "Sort",
		"a sort means the index order is not the order asked for, which is what a "+
			"non-C collation on the key column would cause:\n%s", got)
}

// TestKeysAreByteOrderedInTheSchema guards the COLLATE "C" directly, at the
// level where it can actually be lost — a migration that adds a column without
// it, or a restore into a database with a different default.
//
// The conformance suite covers the behavior; this covers the cause, because a
// collation regression reads as a mysterious paging bug months later rather
// than as a failing listing today.
func TestKeysAreByteOrderedInTheSchema(t *testing.T) {
	store := open(t)

	for _, column := range []string{"bucket", "key"} {
		var collation string

		require.NoError(t, store.Pool().QueryRow(t.Context(), `
			SELECT coalesce(coll.collname, '')
			FROM pg_attribute a
			JOIN pg_class c ON c.oid = a.attrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			LEFT JOIN pg_collation coll ON coll.oid = a.attcollation
			WHERE c.relname = 'objects' AND n.nspname = current_schema()
			  AND a.attname = $1`, column,
		).Scan(&collation))

		assert.Equal(t, "C", collation,
			"%s must sort in byte order, which S3 listing and the paging cursor both assume", column)
	}
}

// TestMigrateIsIdempotent: Open migrates on every start, so a rolling restart
// runs this against an already-current schema on every node.
func TestMigrateIsIdempotent(t *testing.T) {
	store := open(t)

	require.NoError(t, store.Put(t.Context(), metastoretest.Entry("photos", "a.jpg", 1, 1)))

	uri := dsn.Load()
	require.NotNil(t, uri)

	var schema string
	require.NoError(t, store.Pool().QueryRow(t.Context(), "SELECT current_schema()").Scan(&schema))

	require.NoError(t, pgstore.Migrate(t.Context(), *uri+"&search_path="+schema))

	// Re-migrating must not have disturbed what was already there.
	_, found, err := store.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	assert.True(t, found)
}

// exit ends the process with code, in a function so TestMain's deferred
// cleanups are not silently skipped by an inline os.Exit.
func exit(code int) { os.Exit(code) }
