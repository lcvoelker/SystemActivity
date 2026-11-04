# SystemActivity Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    cmd/filemaster/main.go                        │
│   Subcommands: migrate | watch | sessions | history | prune      │
└───────────────┬─────────────────────────────┬───────────────────┘
                │ watch                       │ migrate
                ▼                             ▼
┌───────────────────────┐        ┌───────────────────────────────┐
│  internal/watcher/    │        │  storage.Migrate (goose)      │
│  fsnotify, 16 KB per  │        │  embedded internal/db/        │
│  folder               │        │  migrations, runs as fm_owner │
│  - AddRecursive()     │        └───────────────────────────────┘
│    (folders only)     │
│  - normalize()        │
│  - modifiedSinceStart │  drops folder WRITEs and untouched files
└───────────┬───────────┘
            │ session.Event (path, op, time, old path)
            ▼
┌───────────────────────┐
│  internal/session/    │
│  Manager              │
│  - inactivity split   │
│  - Counts per op      │  no per-path memory
│  - Classify() on      │  work / organization / other
│    close              │
└───────────┬───────────┘
            │ StartSession / EndSession(kind) (sync), Record (buffered)
            ▼
┌───────────────────────────────────────────────┐
│  internal/storage/  (fm_app via pgxpool)       │
│  startup: CloseOrphans, RunRetention (hourly)  │
│  Store ──► batcher                             │
│    merge per (session, path): counts,          │
│    first/last time, last op  (≤ 1,000 entries) │
│    every 5 min, or early when full:            │
│    1. fingerprint each file once (SHA-256)     │
│    2. ensure daily partition (SECURITY DEFINER)│
│    3. BEGIN                                    │
│    4. UpsertFiles   (unnest text[])            │
│    5. InsertBlobs   (unnest bytea[])           │
│    6. InsertFileActivity  (COPY)               │
│    7. RecordVersion (pipelined batch)          │
│    8. COMMIT  (on error: re-merge, retry at    │
│       the next 5-min save)                     │
└───────────────────────┬───────────────────────┘
                        │ sqlc-generated, internal/db
                        ▼
┌───────────────────────────────────────────────┐
│  PostgreSQL  schema "filemaster"               │
│  sessions (kind) ─┐                            │
│  files ───────────┼─► file_activity            │
│                   │   (PARTITION BY day, UTC)  │
│  blobs ───────────┴─► file_versions            │
│                       (EXCLUDE gist)           │
│  view session_summary                          │
│  fn ensure_file_activity_partition, prune_before│
└───────────────────────────────────────────────┘

internal/filter/ShouldIgnore(path)
  - Filters: .git, .idea, .vscode, dist, build, bin
  - Filters: .tmp, .swp, .log extensions
```

## Data Flow

```
watcher: fsnotify event
    ├─ CREATE  → watch new dirs → CREATE (or RENAME if a rename is pending)
    │            a folder moved/unzipped in is one event; its files are not listed
    ├─ WRITE   → dropped for folders and files modified before the watcher started
    ├─ RENAME  → held as pending until the matching CREATE
    ├─ REMOVE  → REMOVE
    └─ CHMOD   → dropped
    → filter.ShouldIgnore → events channel

main loop → manager.AddEvent
    ├─ gap > inactivity? → Classify → EndSession(ended_at, kind) → new session row
    └─ store.Record → batcher merges into the file's entry for this window

batcher save → per entry, version changes:
    last op WRITE/CREATE with hash H → close current version if ≠ H, open H
    last op REMOVE                   → close current version
    renamed/moved here from old path → close old path's version
```

## Session kinds

`session.Classify` (internal/session/classify.go):

| Kind | Rule |
|---|---|
| other | fewer than 3 events, or nothing dominates |
| work | WRITEs ≥ 60% of events |
| organization | CREATE + RENAME + REMOVE ≥ 60% of events |

## Lifecycle and retention

- `end` / Ctrl+C: the session is classified and closed, then buffered activity is saved.
- Killed watcher: its session stays open. The next `watch` on the same host
  and root closes it at its last saved activity, classified from saved counts.
  Up to 5 minutes of unsaved activity from the killed process is lost.
- Retention: `prune_before(now - 3 days)` at startup and hourly drops whole
  daily partitions, trims the partition straddling the cutoff, then deletes
  sessions, versions, paths and blobs nothing references.

## Roles

| Role | Used by | Rights |
|---|---|---|
| `fm_owner` | `filemaster migrate` | Owns schema `filemaster`, runs DDL |
| `fm_app` | `watch`, `sessions`, `history`, `prune` | DML only, via default privileges; EXECUTE on `ensure_file_activity_partition` and `prune_before` |

See `db/bootstrap.sql` for the one-time superuser setup.
