# AGENTS.md

Guidance for coding agents working in this repository. Read this first;
`agentNotes.txt` has the running log of decisions and open items.

## What this is

`filemaster` (module `github.com/lcvoelker/SystemActivity`) is a Go daemon
that watches one large folder tree during 4–7 hour work sessions, records
only files that change while it runs, and stores per-file activity in
PostgreSQL. Each session is classified as **work** (saves),
**organization** (creating, renaming, moving, deleting files/folders),
**work + organization** (both, stored as `mixed`) or **brief** (under 3
events); a behavior counts if it's at least 20% of the session's events,
and creating then saving (or renaming) a new file counts as work.
Summaries list the files per operation. Data older than 3 days is deleted. It's a resume project: prefer
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
  | Classification | writes = work; creates/renames/removes = organization, except a file created in the session and then saved: its create and renames count as work (live tracker follows renames; from saved data, per file); a behavior is in the label at ≥ 20% of events; both → `mixed` ("work + organization"); < 3 events → `brief`; `other` no longer produced | `behaviorShare`, `minClassifiedEvents`, internal/session/classify.go, behavior.go |
  | Summary file lists | at most 10 names per operation, then "+N more"; distinct files tracked as 8-byte hashes | `maxListedFiles`, internal/session/files.go |
  | Unsaved on exit | if the final save fails, entries go to `%LOCALAPPDATA%\filemaster\unsaved\` and the next watch loads them into its buffer (saved with its next regular save); expired entries and entries for deleted sessions are dropped | internal/storage/spill.go, batcher.go |

  Anything else that changes database size or row volume (partitioning,
  new per-event tables, filters) also needs a question first.
- **Applied migrations are immutable.** `00001` and `00002` are applied on
  the owner's real database, and `00003` (session end reason) too (seen at
  version 3 on 2026-11-05); `00004` (kinds `mixed`, `brief`) applies itself
  on the next start. Add a new numbered goose migration for schema
  changes, with a working `-- +goose Down`, and bump the expected version in
  `TestLatestMigrationMatchesFiles`. Every migration must be safe to run
  unattended: `watch` (and `sessions`, `history`, `prune`) apply pending
  migrations automatically at startup when `MIGRATE_DATABASE_URL` is set, so
  never write one that loses data (copy before dropping, as 00002 does).
  Auto-migration only goes up, holds a goose advisory lock, and uses the
  owner connection only while migrating; the running pool is always
  fm_app (`storage.Connect`, internal/storage/pool.go). With
  `MIGRATE_DATABASE_URL` unset, app commands refuse to start unless the
  database is at exactly the newest embedded version (`checkSchemaVersion`,
  internal/storage/schema.go).
- **Tests never touch real data.** Integration tests read
  `FILEMASTER_TEST_DATABASE_URL` and `FILEMASTER_TEST_MIGRATE_DATABASE_URL`
  only (not `DATABASE_URL`, not `.env`); `TestPruneExpired` deletes
  everything older than 3 days, and `TestConnectAppliesPendingMigrationsOnce`
  migrates the test database down and back up. Tests build stores
  with `newStore(pool, root, "")` or a temp spill dir, never `NewStore`,
  which would load the real `%LOCALAPPDATA%` spill files. End-to-end tests
  go through `tests/harness.py`, which always runs filemaster with a temp
  working directory (so the repo's `.env` is never loaded), a temp
  `LOCALAPPDATA`, and URLs for a throwaway cluster from `tests/pgtemp.py`.
- **Smart App Control is on (enforcing) on the owner's PC.** It can block
  any freshly built executable: `go test`, `go run`, `go build` output,
  coverage's `covdata.exe`. Never suggest turning it off or working around
  it. Check compilation locally with `go vet ./...` (it doesn't run
  anything) and run tests on GitHub Actions. Blocks show in the
  `Microsoft-Windows-CodeIntegrity/Operational` event log (event 3077).
- **Rollbacks need `--yes`.** `filemaster migrate down|down-to|redo|reset`
  print what they would drop and refuse without `--yes`.
- Don't commit `.env` (git-ignored) or print its contents.

## Layout

```
cmd/filemaster/main.go        CLI: migrate | watch | sessions | session | history | prune
internal/watcher/             fsnotify wrapper, rename pairing, WRITE filtering
internal/filter/              ignored dirs/extensions
internal/session/             Manager (inactivity split, counts, file lists), Classify, Summary.Print, Store interface
internal/storage/             pgx pool, goose Migrate, Store, batcher (merge + COPY), retention
internal/db/                  sqlc output (DO NOT EDIT), plus:
internal/db/queries/*.sql     hand-written queries → `sqlc generate`
internal/db/migrations/*.sql  goose migrations, embedded into the binary
db/bootstrap.sql              one-time superuser setup (schema, roles, default privileges)
rules.sql                     the owner's own setup notes; read-only for agents, never edit
tests/pgtemp.py               throwaway Postgres clusters (library + CLI)
tests/harness.py              runs filemaster in isolation; Watcher, DB helpers
tests/e2e/                    pytest end-to-end suite (conftest.py has the fixtures)
.github/workflows/tests.yml   CI: vet, Go tests, e2e, coverage summary
```

## Database

- Database `filemaster`, schema `filemaster` (never `public`).
- `fm_owner` owns the schema and runs migrations (`MIGRATE_DATABASE_URL`),
  either via `filemaster migrate` or automatically at startup.
  `Migrate`/`MigrateUp` refuse to run as any other role.
- `fm_app` is DML-only via default privileges (`DATABASE_URL`). DDL it needs
  (creating daily partitions, retention) goes through `SECURITY DEFINER`
  functions owned by `fm_owner`: `ensure_file_activity_partition`, `prune_before`.
- Tables: `sessions` (with `kind`, and `end_reason`: stopped / idle /
  recovered; recovered sessions are reclassified when their spilled
  activity is saved), `files`, `blobs` (content-addressed by
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

## Testing

Three layers, all run by `.github/workflows/tests.yml` on a GitHub Windows
runner (Actions tab > tests > Run workflow; also on every push/PR):

| Layer | Where | What it covers |
|---|---|---|
| Go unit tests | `internal/*/*_test.go` | pure logic: classify, merge, spill files, schema version math, rollback plans |
| Go integration tests | `internal/storage/*_test.go` | SQL against a real cluster; skip without `FILEMASTER_TEST_DATABASE_URL` |
| End-to-end (pytest) | `tests/e2e/test_*.py` | the real `filemaster` binary: file actions in a temp folder, then checks in the database and CLI output |

CI reports coverage for both Go tests and the e2e runs (the binary is built
with `-cover`) in the job summary, and uploads the profiles.

**Adding an end-to-end test:** add `test_*` functions to the matching
`tests/e2e/test_*.py` file (or a new one). Fixtures (`tests/e2e/conftest.py`):
`fm` (CLI bound to the shared migrated cluster), `pg` (query it with
`pg.query` / `pg.scalar`, as superuser, `search_path=filemaster`),
`fresh_pg` (new cluster at version 0), `watched` (empty folder). Drive a
watcher with `with fm.watch(watched) as w:`, sync on output with
`w.expect_event("WRITE", path)` / `w.wait_for(regex)` instead of sleeping,
and finish with `w.end()` (saves) or `w.kill()`. Helpers in
`tests/harness.py`: `session_rows`, `activity_rows`, `stored_paths`. Mark
tests that restart Postgres or wait on timeouts `@pytest.mark.slow`.

**Running locally** (may be blocked by Smart App Control; the suite then
stops with an explanation):

```
python -m pip install -r tests/requirements.txt
python -m pytest tests                 # e2e; builds filemaster itself
python tests/pgtemp.py start --dir <scratch>\pg --port 54329   # cluster for Go tests
```

`tests/pgtemp.py` creates clusters with the local PostgreSQL install
(`FILEMASTER_PG_BIN` or `C:\Program Files\PostgreSQL\<v>\bin`), trust auth,
a free port, `db/bootstrap.sql` applied; `stop --dir` deletes them.
Gotchas: in PowerShell, don't pipe or redirect `pg_ctl start` output (the
call waits on the server forever; pgtemp sends it to DEVNULL). In psql
files, write a literal backslash as `chr(92)`; pgtemp uses `-c`, where
backslashes are fine.

## Conventions

- Match the surrounding code: short doc comments explaining *why*,
  `fmt.Errorf("context: %w", err)` wrapping, tabwriter for CLI tables.
- Windows is the primary platform; paths in the DB are absolute Windows paths.
