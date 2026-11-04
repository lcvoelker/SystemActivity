# AGENTS.md

Guidance for coding agents working in this repository. Read this first;
`agentNotes.txt` has the running log of decisions and open items.

## What this is

`filemaster` (module `github.com/lcvoelker/SystemActivity`) is a Go daemon
that watches one large folder tree during 4–7 hour work sessions, records
only files that change while it runs, and stores per-file activity in
PostgreSQL. Each session is classified as **work** (mostly saves),
**organization** (creating, renaming, moving, deleting files/folders) or
**other**. Data older than 3 days is deleted. It's a resume project: prefer
designs that are clean and defensible in an interview.

## Rules

- **Never edit `notes.txt`.** It's the owner's personal notes. Agent notes go
  in `agentNotes.txt`; lasting guidance goes here.
- **Ask before changing any of these** (values were agreed explicitly):

  | Setting | Value | Where |
  |---|---|---|
  | Save interval | 5 minutes (for testing) | `saveInterval`, internal/storage/batcher.go |
  | In-memory cap | 1,000 file entries; full → save early; while saves fail, drop oldest | `maxEntries`, batcher.go |
  | Retry after failed save | at the next scheduled save | batcher.go |
  | Retention | 3 days | `retention`, internal/storage/store.go |
  | Retention check | at startup, then hourly | `pruneInterval`, store.go |
  | Watch buffer | 16 KB per folder | `watchBufferSize`, internal/watcher/watcher.go |
  | Granularity | one row per file per save window (merged counts) | batcher.go, migration 00002 |
  | What is recorded | only paths named by events during the run; WRITEs to folders and to files whose modified time predates the watcher are dropped; a moved-in folder is one event, its files are not listed | watcher.go |

  Anything else that changes database size or row volume (partitioning,
  new per-event tables, filters) also needs a question first.
- **Applied migrations are immutable.** `00001_init.sql` is applied on the
  owner's real database. Add a new numbered goose migration for schema
  changes, with a working `-- +goose Down`.
- **Tests never touch real data.** Integration tests read
  `FILEMASTER_TEST_DATABASE_URL` only (not `DATABASE_URL`, not `.env`);
  `TestPruneExpired` deletes everything older than 3 days.
- Don't commit `.env` (git-ignored) or print its contents.

## Layout

```
cmd/filemaster/main.go        CLI: migrate | watch | sessions | history | prune
internal/watcher/             fsnotify wrapper, rename pairing, WRITE filtering
internal/filter/              ignored dirs/extensions
internal/session/             Manager (inactivity split, counts), Classify, Store interface
internal/storage/             pgx pool, goose Migrate, Store, batcher (merge + COPY), retention
internal/db/                  sqlc output (DO NOT EDIT), plus:
internal/db/queries/*.sql     hand-written queries → `sqlc generate`
internal/db/migrations/*.sql  goose migrations, embedded into the binary
db/bootstrap.sql              one-time superuser setup (schema, roles, default privileges)
```

## Database

- Database `filemaster`, schema `filemaster` (never `public`).
- `fm_owner` owns the schema and runs `filemaster migrate` (`MIGRATE_DATABASE_URL`).
  `Migrate` refuses to run as any other role.
- `fm_app` is DML-only via default privileges (`DATABASE_URL`). DDL it needs
  (creating daily partitions, retention) goes through `SECURITY DEFINER`
  functions owned by `fm_owner`: `ensure_file_activity_partition`, `prune_before`.
- Tables: `sessions` (with `kind`), `files`, `blobs` (content-addressed by
  SHA-256), `file_activity` (daily partitions, UTC), `file_versions`
  (no-overlap `EXCLUDE USING gist`); view `session_summary`.

## Commands

```
go build ./... && go vet ./...
go test ./...                                   # DB tests skip without FILEMASTER_TEST_DATABASE_URL
~/go/bin/sqlc generate                          # after editing queries or migrations; then build
go run ./cmd/filemaster migrate status
go run ./cmd/filemaster watch <dir>             # "end" or Ctrl+C to stop
```

sqlc v1.29.0 is installed at `%USERPROFILE%\go\bin\sqlc.exe` (built with
`CGO_ENABLED=0`; works on Windows). When adding a view column, check the
generated type; add a `column:` override in `sqlc.yaml` if it comes out as
`interface{}`.

## Testing against a throwaway Postgres (Windows)

The local Postgres 18 install can run a disposable instance with trust auth,
so no real passwords are needed:

```
initdb -D <scratch>\data -U postgres -A trust -E UTF8
pg_ctl -D <scratch>\data -o "-p 54329" -l <scratch>\pg.log -w start
createdb -h localhost -p 54329 -U postgres filemaster
psql -h localhost -p 54329 -U postgres -d filemaster -f db/bootstrap.sql
```

Then point `MIGRATE_DATABASE_URL` / `FILEMASTER_TEST_DATABASE_URL` at
`postgres://fm_owner@localhost:54329/filemaster` / `postgres://fm_app@...`.
Gotcha: in PowerShell, don't pipe or redirect `pg_ctl start` output; the
call then waits on the server process forever. Use Git Bash or no redirect.
In psql, write a literal backslash as `chr(92)`.

## Conventions

- Match the surrounding code: short doc comments explaining *why*,
  `fmt.Errorf("context: %w", err)` wrapping, tabwriter for CLI tables.
- Windows is the primary platform; paths in the DB are absolute Windows paths.
