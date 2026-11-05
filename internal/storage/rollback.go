package storage

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/lcvoelker/SystemActivity/internal/db/migrations"
)

// Undo is one migration a rollback command would reverse, with the DROP
// statements from its Down section so the user sees what will be deleted.
type Undo struct {
	Version int64
	File    string
	Drops   []string
}

// RollbackPlan lists, newest first, the migrations that a goose rollback
// command (down, down-to N, redo, reset) would undo on the database at dsn.
// dsn must be the schema owner, as for Migrate.
func RollbackPlan(ctx context.Context, dsn, command string, args []string) ([]Undo, error) {
	sqlDB, err := openOwnerDB(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer sqlDB.Close()

	rows, err := sqlDB.QueryContext(ctx,
		`SELECT version_id FROM goose_db_version WHERE version_id > 0 ORDER BY version_id DESC`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()
	var applied []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied = append(applied, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var undo []int64
	switch command {
	case "down", "redo":
		if len(applied) > 0 {
			undo = applied[:1]
		}
	case "reset":
		undo = applied
	case "down-to":
		if len(args) != 1 {
			return nil, fmt.Errorf("usage: filemaster migrate down-to <version> --yes")
		}
		target, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("down-to: %q is not a version number", args[0])
		}
		for _, v := range applied {
			if v > target {
				undo = append(undo, v)
			}
		}
	default:
		return nil, fmt.Errorf("%q is not a rollback command", command)
	}

	files, err := migrationFiles()
	if err != nil {
		return nil, err
	}
	plan := make([]Undo, 0, len(undo))
	for _, v := range undo {
		u := Undo{Version: v, File: files[v]}
		if u.File != "" {
			if u.Drops, err = downDrops(u.File); err != nil {
				return nil, err
			}
		}
		plan = append(plan, u)
	}
	return plan, nil
}

// migrationFiles maps each embedded migration's version to its file name.
func migrationFiles() (map[int64]string, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, err
	}
	files := make(map[int64]string)
	for _, e := range entries {
		prefix, _, _ := strings.Cut(e.Name(), "_")
		if v, err := strconv.ParseInt(prefix, 10, 64); err == nil && strings.HasSuffix(e.Name(), ".sql") {
			files[v] = e.Name()
		}
	}
	return files, nil
}

// downDrops returns the statements in a migration's Down section that
// delete something: DROP TABLE/VIEW/TYPE/FUNCTION and ALTER ... DROP COLUMN.
func downDrops(file string) ([]string, error) {
	f, err := migrations.FS.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var drops []string
	inDown := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "-- +goose Down") {
			inDown = true
			continue
		}
		upper := strings.ToUpper(line)
		if inDown && (strings.HasPrefix(upper, "DROP ") || strings.Contains(upper, " DROP COLUMN ")) {
			drops = append(drops, strings.TrimSuffix(line, ";"))
		}
	}
	return drops, sc.Err()
}
