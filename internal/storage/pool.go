// Package storage persists watcher sessions and events to PostgreSQL.
//
// Two roles are involved: fm_owner owns the filemaster schema and runs
// migrations (Migrate, MigrateUp), and fm_app is the least-privilege role
// every long-lived connection uses (NewPool). Connect ties them together:
// it applies pending migrations as the owner only when needed. See
// db/bootstrap.sql.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/lcvoelker/SystemActivity/internal/db/migrations"
)

// Connect opens the application pool. If the schema is behind this build
// and ownerDSN is set, pending migrations are applied first, so upgrading
// needs no separate step. The owner connection is opened only in that case
// and closed before returning; the pool always runs as the app role.
// applied lists the migration versions this call applied.
func Connect(ctx context.Context, appDSN, ownerDSN string) (pool *pgxpool.Pool, applied []int64, err error) {
	pool, err = NewPool(ctx, appDSN)
	var sv *SchemaVersionError
	if !errors.As(err, &sv) || !sv.Behind() || ownerDSN == "" {
		return pool, nil, err
	}

	applied, err = MigrateUp(ctx, ownerDSN)
	if err != nil {
		return nil, nil, fmt.Errorf("apply migrations (schema version %d, need %d): %w", sv.Have, sv.Want, err)
	}
	pool, err = NewPool(ctx, appDSN)
	return pool, applied, err
}

// Schema holds every application object; nothing lives in public.
const Schema = "filemaster"

// NewPool connects as the application role and checks that the database
// schema matches this build. The schema's enums are registered on each
// connection so COPY can encode them in binary format.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = Schema
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// Types that don't exist are skipped, not reported; a missing schema
		// is caught by checkSchemaVersion instead.
		types, err := conn.LoadTypes(ctx, []string{"file_op", "_file_op", "session_kind", "_session_kind", "session_end", "_session_end"})
		if err != nil {
			return fmt.Errorf("load %s types: %w", Schema, err)
		}
		conn.TypeMap().RegisterTypes(types)
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := checkSchemaVersion(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Migrate runs a goose command ("up", "down", "status", "version", "redo",
// "reset") against the embedded migrations. dsn should connect as the
// schema owner.
func Migrate(ctx context.Context, dsn, command string, args ...string) error {
	sqlDB, err := openOwnerDB(ctx, dsn)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.RunContext(ctx, command, sqlDB, ".", args...)
}

// MigrateUp applies every pending migration as the schema owner and
// returns the versions applied. A Postgres advisory lock serializes
// watchers that start at the same time, and each migration runs in its own
// transaction, so a failure rolls that migration back and leaves the
// schema at the last version that applied cleanly. Never migrates down.
func MigrateUp(ctx context.Context, dsn string) ([]int64, error) {
	sqlDB, err := openOwnerDB(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer sqlDB.Close()

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, err
	}
	results, err := provider.Up(ctx)

	var applied []int64
	for _, r := range results {
		if r.Error == nil {
			applied = append(applied, r.Source.Version)
		}
	}
	return applied, err
}

// openOwnerDB connects with the migration DSN and verifies it really is
// the schema owner.
func openOwnerDB(ctx context.Context, dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse MIGRATE_DATABASE_URL: %w", err)
	}
	cfg.RuntimeParams["search_path"] = Schema

	sqlDB := stdlib.OpenDB(*cfg)
	if err := checkSchemaOwner(ctx, sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return sqlDB, nil
}

// checkSchemaOwner stops migrations from running as the wrong role, which
// would leave objects owned by someone other than fm_owner and outside the
// default privileges fm_app relies on.
func checkSchemaOwner(ctx context.Context, sqlDB *sql.DB) error {
	var user, owner string
	err := sqlDB.QueryRowContext(ctx, `
		SELECT current_user, pg_get_userbyid(nspowner)
		FROM pg_namespace WHERE nspname = $1`, Schema).Scan(&user, &owner)
	if err == sql.ErrNoRows {
		return fmt.Errorf("schema %q does not exist; run db/bootstrap.sql as a superuser first", Schema)
	}
	if err != nil {
		return err
	}
	if user != owner {
		return fmt.Errorf("migrations must run as %s (schema owner), not %s", owner, user)
	}
	return nil
}
