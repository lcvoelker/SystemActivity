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

Writes are work; creates, renames (including moves) and removes are
organization, with one exception: a file created during the session and
then saved is a new document being worked on, so its create and any
renames of it (draft → final) count as work. The live tracker
(`behaviorTracker`) follows the new file across renames; sessions
classified from saved data (recovered ones) apply the rule per path
(`BehaviorFromFiles`). A behavior is in the label if it's at least 20% of
the session's events, so a stray rename in a long editing session doesn't
count.

| Kind (stored) | Shown as | Rule |
|---|---|---|
| brief | brief | fewer than 3 events |
| work | work | only work reaches 20% |
| organization | organization | only organization reaches 20% |
| mixed | work + organization | both reach 20% |
| other | other | from before 00004; no longer produced |

The end-of-session summary and `filemaster session <id>` list the files per
operation, relative to the watched folder (renames as `old -> new`), at most
10 per line plus "+N more".

Each closed session also records `end_reason`:

| End reason | Meaning |
|---|---|
| stopped | the user typed `end` or pressed Ctrl+C |
| idle | the inactivity timeout closed it |
| recovered | a later watcher closed it after a kill or failed exit; end time is the last saved activity. When its spilled activity is saved later, the same transaction reclassifies it from the full counts. |

## Lifecycle and retention

- `end` / Ctrl+C: the session is classified and closed, then buffered activity is saved.
- Startup (`storage.Connect`): open the `fm_app` pool and compare the schema
  version with the newest embedded migration. If it's behind and
  `MIGRATE_DATABASE_URL` is set, apply the pending migrations as `fm_owner`
  (goose advisory lock, up only), close that connection, and reopen as
  `fm_app`. Otherwise refuse to start with a clear message.
- Failed final save (e.g. database down at `end`): unsaved entries are
  written to `%LOCALAPPDATA%\filemaster\unsaved\` and loaded into the buffer
  by the next `watch`, which saves them with its next regular save.
- Killed watcher: its session stays open. The next `watch` on the same host
  and root closes it at its last saved activity, classified from saved counts.
  Up to 5 minutes of unsaved activity from the killed process is lost.
- Retention: `prune_before(now - 3 days)` at startup and hourly drops whole
  daily partitions, trims the partition straddling the cutoff, then deletes
  sessions, versions, paths and blobs nothing references.

## Roles

| Role | Used by | Rights |
|---|---|---|
| `fm_owner` | `filemaster migrate`, and briefly at startup when migrations are pending | Owns schema `filemaster`, runs DDL |
| `fm_app` | `watch`, `sessions`, `history`, `prune` | DML only, via default privileges; EXECUTE on `ensure_file_activity_partition` and `prune_before` |

See `db/bootstrap.sql` for the one-time superuser setup.
