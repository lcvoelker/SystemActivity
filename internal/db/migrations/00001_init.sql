-- Runs as fm_owner with search_path = filemaster (see db/bootstrap.sql).
-- fm_app gets SELECT/INSERT/UPDATE/DELETE on everything here through the
-- default privileges set up in the bootstrap script.

-- +goose Up
CREATE TYPE file_op AS ENUM ('CREATE', 'WRITE', 'RENAME', 'REMOVE');

-- One row per burst of activity, split by the watcher's inactivity timeout.
CREATE TABLE sessions (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    host       text        NOT NULL,
    root_path  text        NOT NULL,
    started_at timestamptz NOT NULL,
    ended_at   timestamptz,
    CHECK (ended_at IS NULL OR ended_at >= started_at)
);

-- Path dimension: every path ever observed, stored once.
CREATE TABLE files (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    path          text        NOT NULL UNIQUE,
    first_seen_at timestamptz NOT NULL DEFAULT now()
);

-- Content-addressed store: one row per distinct file content (SHA-256),
-- no matter how many paths or versions share it.
CREATE TABLE blobs (
    sha256        bytea       PRIMARY KEY CHECK (octet_length(sha256) = 32),
    size_bytes    bigint      NOT NULL CHECK (size_bytes >= 0),
    first_seen_at timestamptz NOT NULL DEFAULT now()
);

-- Append-only event log, range-partitioned by month so old months can be
-- detached or dropped cheaply. Partitions are created on demand through
-- ensure_file_event_partition below.
CREATE TABLE file_events (
    id          bigint GENERATED ALWAYS AS IDENTITY,
    session_id  bigint      NOT NULL REFERENCES sessions (id),
    file_id     bigint      NOT NULL REFERENCES files (id),
    op          file_op     NOT NULL,
    old_path    text,
    sha256      bytea       REFERENCES blobs (sha256),
    size_bytes  bigint,
    occurred_at timestamptz NOT NULL,
    PRIMARY KEY (id, occurred_at),
    CHECK ((op = 'RENAME') = (old_path IS NOT NULL))
) PARTITION BY RANGE (occurred_at);

CREATE INDEX file_events_file_time_idx ON file_events (file_id, occurred_at DESC);
CREATE INDEX file_events_session_idx ON file_events (session_id);

-- Which content each path held, and when. valid_to IS NULL marks the
-- current version. The exclusion constraint guarantees that a file's
-- versions never overlap in time (needs btree_gist for the = on file_id).
CREATE TABLE file_versions (
    file_id    bigint      NOT NULL REFERENCES files (id),
    sha256     bytea       NOT NULL REFERENCES blobs (sha256),
    valid_from timestamptz NOT NULL,
    valid_to   timestamptz,
    PRIMARY KEY (file_id, valid_from),
    CHECK (valid_to IS NULL OR valid_to >= valid_from),
    EXCLUDE USING gist (
        file_id WITH =,
        tstzrange(valid_from, valid_to, '[)') WITH &&
    )
);

CREATE UNIQUE INDEX file_versions_current_idx ON file_versions (file_id) WHERE valid_to IS NULL;
CREATE INDEX file_versions_sha256_idx ON file_versions (sha256);

CREATE VIEW session_summary AS
SELECT s.id                                       AS session_id,
       s.host,
       s.root_path,
       s.started_at,
       s.ended_at,
       count(e.id)                                AS total_events,
       count(DISTINCT e.file_id)                  AS unique_files,
       count(*) FILTER (WHERE e.op = 'CREATE')    AS creates,
       count(*) FILTER (WHERE e.op = 'WRITE')     AS writes,
       count(*) FILTER (WHERE e.op = 'RENAME')    AS renames,
       count(*) FILTER (WHERE e.op = 'REMOVE')    AS removes
FROM sessions s
LEFT JOIN file_events e ON e.session_id = s.id
GROUP BY s.id;

-- fm_app cannot create tables, so partition creation goes through this
-- SECURITY DEFINER function owned by fm_owner. Month boundaries are UTC so
-- clients in different time zones agree on them.
-- +goose StatementBegin
CREATE FUNCTION ensure_file_event_partition(p_at timestamptz)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = filemaster, pg_temp
AS $$
DECLARE
    month_utc timestamp := date_trunc('month', p_at AT TIME ZONE 'UTC');
    lo        timestamptz := month_utc AT TIME ZONE 'UTC';
    hi        timestamptz := (month_utc + interval '1 month') AT TIME ZONE 'UTC';
    part      text := 'file_events_' || to_char(month_utc, 'YYYY_MM');
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF file_events FOR VALUES FROM (%L) TO (%L)',
        part, lo, hi);
    RETURN part;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION ensure_file_event_partition(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ensure_file_event_partition(timestamptz) TO fm_app;

SELECT ensure_file_event_partition(now());
SELECT ensure_file_event_partition(now() + interval '1 month');

-- +goose Down
DROP VIEW IF EXISTS session_summary;
DROP TABLE IF EXISTS file_versions, file_events, blobs, files, sessions;
DROP FUNCTION IF EXISTS ensure_file_event_partition(timestamptz);
DROP TYPE IF EXISTS file_op;
