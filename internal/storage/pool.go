// Package storage persists watcher sessions and events to PostgreSQL.
//
// Two roles are involved: fm_owner owns the filemaster schema and runs
// migrations (Migrate), and fm_app is the least-privilege role the watcher
// connects as (NewPool). See db/bootstrap.sql.
package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/lcvoelker/SystemActivity/internal/db/migrations"
)

// Schema holds every application object; nothing lives in public.
const Schema = "filemaster"

// NewPool connects as the application role. The schema's enums are
// registered on each connection so COPY can encode them in binary format.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = Schema
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		types, err := conn.LoadTypes(ctx, []string{"file_op", "_file_op", "session_kind", "_session_kind"})
		if err != nil {
			return fmt.Errorf("load %s types (has `filemaster migrate up` been run?): %w", Schema, err)
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
	return pool, nil
}

// Migrate runs a goose command ("up", "down", "status", "version", "redo",
// "reset") against the embedded migrations. dsn should connect as the
// schema owner.
func Migrate(ctx context.Context, dsn, command string, args ...string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse MIGRATE_DATABASE_URL: %w", err)
	}
	cfg.RuntimeParams["search_path"] = Schema

	sqlDB := stdlib.OpenDB(*cfg)
	defer sqlDB.Close()
	if err := checkSchemaOwner(ctx, sqlDB); err != nil {
		return err
	}

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.RunContext(ctx, command, sqlDB, ".", args...)
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
