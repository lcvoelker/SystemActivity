package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lcvoelker/SystemActivity/internal/db/migrations"
)

// LatestMigration is the newest migration version embedded in this binary,
// taken from the numeric prefix of the goose file names (00002_... → 2).
func LatestMigration() (int64, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(name, "_")
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migration %s: version prefix is not a number", name)
		}
		latest = max(latest, v)
	}
	return latest, nil
}

// checkSchemaVersion refuses to run against a database whose applied
// migrations don't match this binary. Without it, a pending migration only
// shows up later as "function/type/column does not exist" errors, and
// buffered data is lost when the first save fails.
func checkSchemaVersion(ctx context.Context, pool *pgxpool.Pool) error {
	want, err := LatestMigration()
	if err != nil {
		return err
	}

	// Same query goose uses for the current version; down migrations
	// delete their row.
	var have int64
	err = pool.QueryRow(ctx, `SELECT coalesce(max(version_id), 0) FROM goose_db_version`).Scan(&have)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table: never migrated
		have, err = 0, nil
	}
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	return compareSchemaVersion(have, want)
}

// SchemaVersionError reports a database whose schema doesn't match this
// build. Behind() errors can be fixed by applying migrations (MigrateUp).
type SchemaVersionError struct {
	Have, Want int64
}

func (e *SchemaVersionError) Behind() bool { return e.Have < e.Want }

func (e *SchemaVersionError) Error() string {
	if e.Behind() {
		return fmt.Sprintf("database schema is at version %d but this build needs %d; set MIGRATE_DATABASE_URL to apply migrations automatically, or run `filemaster migrate up`", e.Have, e.Want)
	}
	return fmt.Sprintf("database schema is at version %d, newer than this build (%d); update filemaster", e.Have, e.Want)
}

func compareSchemaVersion(have, want int64) error {
	if have != want {
		return &SchemaVersionError{Have: have, Want: want}
	}
	return nil
}
