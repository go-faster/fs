package pgstore

import (
	"context"
	"embed"
	"strings"

	"github.com/go-faster/errors"
	"github.com/golang-migrate/migrate/v4"

	// The pgx/v5 database driver and the iofs source, registered by import.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate brings the schema at dsn up to date.
//
// It is exported so an operator can run it as a deliberate step against a
// database they own, rather than only as a side effect of a node starting. Open
// calls it too, because a node that cannot read its own schema is not useful
// and this backend is scaffolding rather than a system of record — but the two
// paths run the same migrations, so there is never a question of which applied.
func Migrate(ctx context.Context, dsn string) error {
	source, err := iofs.New(migrations, "migrations")
	if err != nil {
		return errors.Wrap(err, "open migrations")
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, migrateDSN(dsn))
	if err != nil {
		return errors.Wrap(err, "create migrate")
	}

	defer func() {
		// The source is in-process and the database handle is migrate's own, so
		// neither close can fail in a way the caller could act on.
		_, _ = m.Close()
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return errors.Wrap(err, "migrate up")
	}

	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "migrate up")
	}

	return nil
}

// migrateDSN rewrites a connection string into the scheme golang-migrate's
// pgx/v5 driver registers itself under. Callers configure one DSN; only the
// migration path needs the other spelling.
func migrateDSN(dsn string) string {
	for _, scheme := range []string{"postgres://", "postgresql://"} {
		if strings.HasPrefix(dsn, scheme) {
			return "pgx5://" + strings.TrimPrefix(dsn, scheme)
		}
	}

	return dsn
}
