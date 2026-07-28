// Package pgstore is the PostgreSQL-backed, cluster-scope implementation of
// metastore.Store.
//
// It is **scaffolding, and saying so is the point.** The destination for
// cluster-scope metadata is the embedded sharded pebble plane, which keeps the
// project's single-external-dependency guarantee. This backend exists because
// it makes the cluster-scope *contract* — the interface, the listing path, the
// rebuild machinery, the availability semantics — real and testable in CI with
// a container and zero distributed-systems work. That is worth building even if
// no deployment ever runs it, because it means the sharded plane implements a
// settled interface instead of co-designing one while also building range
// splits and failover.
//
// It is also a legitimate thing to run, for a deployment that already operates
// PostgreSQL, to roughly 10⁹–10¹⁰ objects.
//
// # Scope
//
// This store holds one row per object for the *whole cluster*, so it reports
// [metastore.ScopeCluster] and a listing page is one range scan rather than a
// k-way merge of one scan per node. The consequence a caller must be ready for
// is the other direction: the store is now on the listing path's availability
// envelope, where before an unreachable node cost only the objects it held.
//
// # Still derived, never authoritative
//
// Nothing here is a commit point. Sidecars on disk remain the truth; this is a
// cache of them that happens to be queryable. That is what makes it safe for
// the store to be unavailable — listing and usage degrade, writes and reads do
// not — and it is why none of the operations below need to be more careful than
// a transaction.
package pgstore

import (
	"context"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/go-faster/errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
)

// psql builds statements with PostgreSQL's $n placeholders.
var psql = sq.StatementBuilder.PlaceholderFormat(sq.Dollar)

// Column names used in more than one statement, so a rename is one edit.
const (
	colBucket = "bucket"
	colKey    = "key"
	colState  = "state"
)

// entryColumns is the projection every read of an object uses, in the order
// scanEntry expects them.
var entryColumns = []string{
	colBucket, colKey, "size", "etag", "modified", "seq",
	"generation", "disk", "owner_id", "owner_name", "verified_at",
}

// Store is a PostgreSQL-backed metadata store.
type Store struct {
	pool *pgxpool.Pool
	// owned says whether Close should shut the pool down. A pool handed in by a
	// caller is the caller's to close.
	owned bool
}

var _ metastore.Store = (*Store)(nil)

// Open connects to dsn, migrates the schema, and returns a store.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if err := Migrate(ctx, dsn); err != nil {
		return nil, err
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, errors.Wrap(err, "connect to postgres")
	}

	return &Store{pool: pool, owned: true}, nil
}

// New wraps an existing pool, assuming the schema is already migrated. Close
// does not shut the pool down; the caller keeps that.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Pool exposes the underlying connection pool, for tests that need to inspect
// the database rather than go through the store.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Scope implements metastore.Store: one row per object for the whole cluster,
// so a caller scans once instead of merging across nodes.
func (s *Store) Scope() metastore.Scope { return metastore.ScopeCluster }

// Close implements metastore.Store.
func (s *Store) Close() error {
	if s.owned {
		s.pool.Close()
	}

	return nil
}

// querier is satisfied by both the pool and a transaction, so a read can run
// inside a write's transaction or on its own.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Put implements metastore.Store.
//
// The supersedes check runs in Go rather than as a WHERE clause, deliberately.
// The ordering rule already exists twice — on [metastore.Entry] and on the
// sidecar it is derived from — and those two are pinned to each other by a
// differential test. Expressing it a third time in SQL would put a copy where
// that test cannot reach it, and an ordering rule that disagrees with the
// sidecars is how a derived store corrupts rather than merely lags.
//
// So: lock the row, decide in Go, write the entry and the counter it moves in
// one transaction.
func (s *Store) Put(ctx context.Context, e metastore.Entry) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "put index entry")
	}

	return s.tx(ctx, "commit index entry", func(tx pgx.Tx) error {
		prev, found, err := get(ctx, tx, e.Bucket, e.Key, forUpdate)
		if err != nil {
			return err
		}

		if found && !e.Supersedes(prev) {
			return nil
		}

		// A re-index of the same object keeps whatever the scrub last recorded;
		// only a scrub sets it, and it does not know about writes.
		if found && e.VerifiedAt.IsZero() {
			e.VerifiedAt = prev.VerifiedAt
		}

		delta := metastore.Usage{Objects: 1, Bytes: e.Size}
		if found {
			delta = metastore.Usage{Bytes: e.Size - prev.Size}
		}

		query, args, err := psql.Insert("objects").
			Columns(entryColumns...).
			Values(e.Bucket, e.Key, e.Size, e.ETag, e.Modified, e.Seq,
				e.Generation, string(e.Disk), e.OwnerID, e.OwnerName,
				nullTime(e.VerifiedAt)).
			Suffix(`ON CONFLICT (bucket, key) DO UPDATE SET
				size = EXCLUDED.size, etag = EXCLUDED.etag,
				modified = EXCLUDED.modified, seq = EXCLUDED.seq,
				generation = EXCLUDED.generation, disk = EXCLUDED.disk,
				owner_id = EXCLUDED.owner_id, owner_name = EXCLUDED.owner_name,
				verified_at = EXCLUDED.verified_at`).
			ToSql()
		if err != nil {
			return errors.Wrap(err, "build insert query")
		}

		if _, err := tx.Exec(ctx, query, args...); err != nil {
			return errors.Wrap(err, "write index entry")
		}

		return addUsage(ctx, tx, e.Bucket, delta)
	})
}

// Delete implements metastore.Store. Removing what is not there is not an
// error.
func (s *Store) Delete(ctx context.Context, bucket, key string) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "delete index entry")
	}

	return s.tx(ctx, "commit index delete", func(tx pgx.Tx) error {
		prev, found, err := get(ctx, tx, bucket, key, forUpdate)
		if err != nil {
			return err
		}

		if !found {
			return nil
		}

		query, args, err := psql.Delete("objects").
			Where(sq.Eq{colBucket: bucket, colKey: key}).
			ToSql()
		if err != nil {
			return errors.Wrap(err, "build delete query")
		}

		if _, err := tx.Exec(ctx, query, args...); err != nil {
			return errors.Wrap(err, "delete index entry")
		}

		return addUsage(ctx, tx, bucket,
			metastore.Usage{Objects: -1, Bytes: -prev.Size})
	})
}

// addUsage folds a delta into the bucket's counters inside tx.
//
// The clamp at zero matches the pebble backend: counters are derived, and one
// that has gone negative should report nothing rather than nonsense while the
// next rebuild fixes it.
//
// Written out rather than built, because squirrel cannot express a conflict
// target that references both EXCLUDED and the existing row, and pretending
// otherwise would be more obscure than the statement itself.
func addUsage(ctx context.Context, tx pgx.Tx, bucket string, delta metastore.Usage) error {
	const query = `
		INSERT INTO bucket_usage (bucket, objects, bytes)
		VALUES ($1, GREATEST($2::bigint, 0), GREATEST($3::bigint, 0))
		ON CONFLICT (bucket) DO UPDATE SET
			objects = GREATEST(bucket_usage.objects + $2::bigint, 0),
			bytes   = GREATEST(bucket_usage.bytes + $3::bigint, 0)`

	if _, err := tx.Exec(ctx, query, bucket, delta.Objects, delta.Bytes); err != nil {
		return errors.Wrap(err, "write usage")
	}

	return nil
}

// locking says whether a read takes the row for update. It exists so the two
// call sites read as what they are rather than as a bare true/false.
type locking bool

const (
	forRead   locking = false
	forUpdate locking = true
)

// Get implements metastore.Store.
func (s *Store) Get(ctx context.Context, bucket, key string) (metastore.Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return metastore.Entry{}, false, errors.Wrap(err, "read index entry")
	}

	return get(ctx, s.pool, bucket, key, forRead)
}

// get reads one entry. Taken FOR UPDATE, it is what serializes two writers
// racing on the same object — the analog of the pebble backend's per-bucket
// stripe, at row granularity instead.
func get(ctx context.Context, q querier, bucket, key string, lock locking) (metastore.Entry, bool, error) {
	builder := psql.Select(entryColumns...).
		From("objects").
		Where(sq.Eq{colBucket: bucket, colKey: key})
	if lock {
		builder = builder.Suffix("FOR UPDATE")
	}

	query, args, err := builder.ToSql()
	if err != nil {
		return metastore.Entry{}, false, errors.Wrap(err, "build select query")
	}

	e, err := scanEntry(q.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return metastore.Entry{}, false, nil
	}

	if err != nil {
		return metastore.Entry{}, false, errors.Wrap(err, "read index entry")
	}

	return e, true, nil
}

// scanEntry reads one row of entryColumns.
func scanEntry(row pgx.Row) (metastore.Entry, error) {
	var (
		e          metastore.Entry
		disk       string
		verifiedAt *time.Time
	)

	if err := row.Scan(
		&e.Bucket, &e.Key, &e.Size, &e.ETag, &e.Modified, &e.Seq,
		&e.Generation, &disk, &e.OwnerID, &e.OwnerName, &verifiedAt,
	); err != nil {
		return metastore.Entry{}, err
	}

	e.Disk = cluster.DiskID(disk)
	if verifiedAt != nil {
		e.VerifiedAt = *verifiedAt
	}

	return e, nil
}

// Usage implements metastore.Store, reading the maintained aggregate rather
// than recounting.
func (s *Store) Usage(ctx context.Context, bucket string) (metastore.Usage, error) {
	if err := ctx.Err(); err != nil {
		return metastore.Usage{}, errors.Wrap(err, "read usage")
	}

	query, args, err := psql.Select("objects", "bytes").
		From("bucket_usage").
		Where(sq.Eq{colBucket: bucket}).
		ToSql()
	if err != nil {
		return metastore.Usage{}, errors.Wrap(err, "build select query")
	}

	var u metastore.Usage

	err = s.pool.QueryRow(ctx, query, args...).Scan(&u.Objects, &u.Bytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return metastore.Usage{}, nil
	}

	if err != nil {
		return metastore.Usage{}, errors.Wrap(err, "read usage")
	}

	return u, nil
}

// Buckets implements metastore.Store.
func (s *Store) Buckets(ctx context.Context, fn func(bucket string) error) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "scan buckets")
	}

	query, args, err := psql.Select(colBucket).From("bucket_usage").OrderBy(colBucket).ToSql()
	if err != nil {
		return errors.Wrap(err, "build select query")
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return errors.Wrap(err, "scan buckets")
	}

	defer rows.Close()

	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "scan buckets")
		}

		var bucket string
		if err := rows.Scan(&bucket); err != nil {
			return errors.Wrap(err, "scan buckets")
		}

		if err := fn(bucket); err != nil {
			return err
		}
	}

	if err := rows.Err(); err != nil {
		return errors.Wrap(err, "scan buckets")
	}

	return nil
}

// Scan implements metastore.Store as **one query**.
//
// That is the whole justification for cluster scope, so it is worth being
// explicit about what makes it one: the prefix becomes a half-open range rather
// than a LIKE, so the primary key's btree is positioned directly; the cursor is
// an exclusive lower bound on that same index; and the limit is pushed into the
// statement rather than applied while draining. Nothing here degrades into a
// filter over the bucket.
func (s *Store) Scan(
	ctx context.Context,
	bucket, prefix, after string,
	limit int,
	fn func(metastore.Entry) error,
) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "scan index")
	}

	builder := psql.Select(entryColumns...).
		From("objects").
		Where(sq.Eq{colBucket: bucket}).
		OrderBy(colKey)

	// Lower bound: the later of the prefix and the cursor, exclusive only when
	// it is the cursor that won. Either way it is a comparison on the indexed
	// column, so the btree is positioned rather than filtered.
	if after != "" && after >= prefix {
		builder = builder.Where(sq.Gt{colKey: after})
	} else if prefix != "" {
		builder = builder.Where(sq.GtOrEq{colKey: prefix})
	}

	// The prefix's exclusive upper bound, so the scan stops at the end of the
	// prefix instead of reading to the end of the bucket and discarding.
	if end, ok := prefixEnd(prefix); ok {
		builder = builder.Where(sq.Lt{colKey: end})
	}

	if limit > 0 {
		builder = builder.Limit(uint64(limit))
	}

	query, args, err := builder.ToSql()
	if err != nil {
		return errors.Wrap(err, "build select query")
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return errors.Wrap(err, "scan index")
	}

	defer rows.Close()

	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "scan index")
		}

		e, err := scanEntry(rows)
		if err != nil {
			return errors.Wrap(err, "scan index")
		}

		if err := fn(e); err != nil {
			return err
		}
	}

	if err := rows.Err(); err != nil {
		return errors.Wrap(err, "scan index")
	}

	return nil
}

// SetVerified implements metastore.Store.
//
// Objects the store does not hold are skipped rather than created, which the
// WHERE gives for free: a stamp with no object behind it would be an entry that
// then gets listed.
func (s *Store) SetVerified(ctx context.Context, records []metastore.Verification) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "record verification")
	}

	if len(records) == 0 {
		return nil
	}

	return s.tx(ctx, "commit verification", func(tx pgx.Tx) error {
		batch := &pgx.Batch{}

		for _, rec := range records {
			query, args, err := psql.Update("objects").
				Set("verified_at", rec.At).
				Where(sq.Eq{colBucket: rec.Bucket, colKey: rec.Key}).
				ToSql()
			if err != nil {
				return errors.Wrap(err, "build update query")
			}

			batch.Queue(query, args...)
		}

		results := tx.SendBatch(ctx, batch)
		defer func() { _ = results.Close() }()

		for range records {
			if _, err := results.Exec(); err != nil {
				return errors.Wrap(err, "stage verification")
			}
		}

		return nil
	})
}

// Coverage implements metastore.Store as one aggregate query.
//
// This is the clearest demonstration of what cluster scope buys. The pebble
// backend answers it by walking every entry it holds, per node, and the caller
// adds the results up. Here it is a single pass the database plans itself, over
// the whole cluster's objects, and it does not get slower as nodes are added.
func (s *Store) Coverage(ctx context.Context) (metastore.Coverage, error) {
	if err := ctx.Err(); err != nil {
		return metastore.Coverage{}, errors.Wrap(err, "read coverage")
	}

	query, args, err := psql.Select(
		"count(*)",
		"count(*) FILTER (WHERE verified_at IS NULL)",
		"min(verified_at)",
	).From("objects").ToSql()
	if err != nil {
		return metastore.Coverage{}, errors.Wrap(err, "build select query")
	}

	var (
		cov    metastore.Coverage
		oldest *time.Time
	)

	if err := s.pool.QueryRow(ctx, query, args...).
		Scan(&cov.Objects, &cov.Never, &oldest); err != nil {
		return metastore.Coverage{}, errors.Wrap(err, "read coverage")
	}

	if oldest != nil {
		cov.Oldest = *oldest
	}

	return cov, nil
}

// State implements metastore.Store.
func (s *Store) State(ctx context.Context) (metastore.State, error) {
	if err := ctx.Err(); err != nil {
		return metastore.StateBuilding, errors.Wrap(err, "read index state")
	}

	query, args, err := psql.Select(colState).From("store_meta").Where(sq.Eq{"id": true}).ToSql()
	if err != nil {
		return metastore.StateBuilding, errors.Wrap(err, "build select query")
	}

	var state int16

	err = s.pool.QueryRow(ctx, query, args...).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return metastore.StateBuilding, nil
	}

	if err != nil {
		return metastore.StateBuilding, errors.Wrap(err, "read index state")
	}

	if state == int16(metastore.StateReady) {
		return metastore.StateReady, nil
	}

	return metastore.StateBuilding, nil
}

// MarkReady implements metastore.Store.
func (s *Store) MarkReady(ctx context.Context) error {
	return s.setState(ctx, metastore.StateReady)
}

// MarkBuilding implements metastore.Store.
func (s *Store) MarkBuilding(ctx context.Context) error {
	return s.setState(ctx, metastore.StateBuilding)
}

func (s *Store) setState(ctx context.Context, state metastore.State) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "write index state")
	}

	query, args, err := psql.Insert("store_meta").
		Columns("id", colState).
		Values(true, int16(state)).
		Suffix("ON CONFLICT (id) DO UPDATE SET state = EXCLUDED.state").
		ToSql()
	if err != nil {
		return errors.Wrap(err, "build insert query")
	}

	if _, err := s.pool.Exec(ctx, query, args...); err != nil {
		return errors.Wrap(err, "write index state")
	}

	return nil
}

// Reset implements metastore.Store.
//
// TRUNCATE rather than DELETE: this runs before a full rebuild, so there is
// nothing to preserve, and a DELETE over 10⁹ rows would leave that many dead
// tuples for autovacuum to chase afterwards.
func (s *Store) Reset(ctx context.Context) error {
	if err := s.MarkBuilding(ctx); err != nil {
		return err
	}

	if _, err := s.pool.Exec(ctx, `TRUNCATE objects, bucket_usage`); err != nil {
		return errors.Wrap(err, "reset index")
	}

	return nil
}

// tx runs fn in a transaction, rolling back on error.
func (s *Store) tx(ctx context.Context, what string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return errors.Wrap(err, what)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return errors.Wrap(err, what)
	}

	return nil
}

// nullTime maps the zero time to NULL, so "never verified" is absent rather
// than "verified at the beginning of time" — which would sort first in a
// staleness sweep and so be picked up last by the sweep that should reach it
// first.
func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}

	return &t
}

// prefixEnd is the exclusive upper bound of a prefix scan: the prefix with its
// last byte incremented. A prefix that is empty, or all 0xFF, has no successor,
// and then there is no upper bound to add — which is correct, because nothing
// sorts above it.
func prefixEnd(prefix string) (string, bool) {
	out := []byte(prefix)

	for i := len(out) - 1; i >= 0; i-- {
		if out[i] < 0xFF {
			out[i]++

			return string(out[:i+1]), true
		}
	}

	return "", false
}
