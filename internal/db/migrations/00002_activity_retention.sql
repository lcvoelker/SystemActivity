-- Replaces the one-row-per-event log with one row per file per save window
-- (the watcher saves every 5 minutes), partitions it by day so data older
-- than the 3-day retention can be dropped cheaply, and records what kind of
-- session each one was.

-- +goose Up
CREATE TYPE session_kind AS ENUM ('work', 'organization', 'other');
ALTER TABLE sessions ADD COLUMN kind session_kind;

-- Everything that happened to one path in one save window: how many times
-- each operation occurred, when, and the content at save time.
CREATE TABLE file_activity (
    id         bigint      GENERATED ALWAYS AS IDENTITY,
    session_id bigint      NOT NULL REFERENCES sessions (id),
    file_id    bigint      NOT NULL REFERENCES files (id),
    first_at   timestamptz NOT NULL,
    last_at    timestamptz NOT NULL,
    creates    integer     NOT NULL DEFAULT 0 CHECK (creates >= 0),
    writes     integer     NOT NULL DEFAULT 0 CHECK (writes >= 0),
    renames    integer     NOT NULL DEFAULT 0 CHECK (renames >= 0),
    removes    integer     NOT NULL DEFAULT 0 CHECK (removes >= 0),
    last_op    file_op     NOT NULL,
    old_path   text,       -- set when the path was renamed or moved here
    sha256     bytea       REFERENCES blobs (sha256),
    size_bytes bigint,
    PRIMARY KEY (id, last_at),
    CHECK (last_at >= first_at),
    CHECK (creates + writes + renames + removes > 0)
) PARTITION BY RANGE (last_at);

CREATE INDEX file_activity_file_time_idx ON file_activity (file_id, last_at DESC);
CREATE INDEX file_activity_session_idx ON file_activity (session_id);

-- Daily partitions, UTC boundaries. SECURITY DEFINER so fm_app can create
-- them without DDL rights.
-- +goose StatementBegin
CREATE FUNCTION ensure_file_activity_partition(p_at timestamptz)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = filemaster, pg_temp
AS $$
DECLARE
    day_utc timestamp := date_trunc('day', p_at AT TIME ZONE 'UTC');
    lo      timestamptz := day_utc AT TIME ZONE 'UTC';
    hi      timestamptz := (day_utc + interval '1 day') AT TIME ZONE 'UTC';
    part    text := 'file_activity_' || to_char(day_utc, 'YYYYMMDD');
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF file_activity FOR VALUES FROM (%L) TO (%L)',
        part, lo, hi);
    RETURN part;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION ensure_file_activity_partition(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ensure_file_activity_partition(timestamptz) TO fm_app;

-- Carry existing events over as single-event activity rows.
-- +goose StatementBegin
DO $$
DECLARE
    d timestamptz;
BEGIN
    FOR d IN SELECT DISTINCT date_trunc('day', occurred_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' FROM file_events LOOP
        PERFORM ensure_file_activity_partition(d);
    END LOOP;
END;
$$;
-- +goose StatementEnd

INSERT INTO file_activity (session_id, file_id, first_at, last_at, creates, writes, renames, removes,
                           last_op, old_path, sha256, size_bytes)
SELECT session_id, file_id, occurred_at, occurred_at,
       (op = 'CREATE')::int, (op = 'WRITE')::int, (op = 'RENAME')::int, (op = 'REMOVE')::int,
       op, old_path, sha256, size_bytes
FROM file_events;

DROP VIEW session_summary;
DROP TABLE file_events;
DROP FUNCTION ensure_file_event_partition(timestamptz);

CREATE VIEW session_summary AS
SELECT s.id                                                       AS session_id,
       s.host,
       s.root_path,
       s.started_at,
       s.ended_at,
       s.kind,
       max(a.last_at)                                             AS last_activity_at,
       coalesce(sum(a.creates + a.writes + a.renames + a.removes), 0)::bigint AS total_events,
       count(DISTINCT a.file_id)                                  AS unique_files,
       coalesce(sum(a.creates), 0)::bigint                        AS creates,
       coalesce(sum(a.writes), 0)::bigint                         AS writes,
       coalesce(sum(a.renames), 0)::bigint                        AS renames,
       coalesce(sum(a.removes), 0)::bigint                        AS removes
FROM sessions s
LEFT JOIN file_activity a ON a.session_id = s.id
GROUP BY s.id;

-- Retention: deletes everything recorded before p_cutoff. Whole daily
-- partitions are dropped; the partition straddling the cutoff is trimmed
-- row by row. Then sessions, versions, paths and blobs nothing references.
-- +goose StatementBegin
CREATE FUNCTION prune_before(p_cutoff timestamptz)
RETURNS TABLE (dropped_partitions integer, deleted_sessions bigint)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = filemaster, pg_temp
AS $$
DECLARE
    part record;
BEGIN
    dropped_partitions := 0;
    FOR part IN
        SELECT c.relname
        FROM pg_inherits i
        JOIN pg_class c ON c.oid = i.inhrelid
        WHERE i.inhparent = 'file_activity'::regclass
          AND c.relname ~ '^file_activity_\d{8}$'
          -- upper bound of the partition's day, in UTC
          AND (to_date(right(c.relname, 8), 'YYYYMMDD') + 1)::timestamp AT TIME ZONE 'UTC' <= p_cutoff
    LOOP
        EXECUTE format('DROP TABLE %I', part.relname);
        dropped_partitions := dropped_partitions + 1;
    END LOOP;

    DELETE FROM file_activity WHERE last_at < p_cutoff;
    DELETE FROM file_versions WHERE coalesce(valid_to, valid_from) < p_cutoff;

    DELETE FROM sessions s
    WHERE coalesce(s.ended_at, s.started_at) < p_cutoff
      AND NOT EXISTS (SELECT 1 FROM file_activity a WHERE a.session_id = s.id);
    GET DIAGNOSTICS deleted_sessions = ROW_COUNT;

    DELETE FROM files f
    WHERE NOT EXISTS (SELECT 1 FROM file_activity a WHERE a.file_id = f.id)
      AND NOT EXISTS (SELECT 1 FROM file_versions v WHERE v.file_id = f.id);

    DELETE FROM blobs b
    WHERE NOT EXISTS (SELECT 1 FROM file_activity a WHERE a.sha256 = b.sha256)
      AND NOT EXISTS (SELECT 1 FROM file_versions v WHERE v.sha256 = b.sha256);

    RETURN NEXT;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION prune_before(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION prune_before(timestamptz) TO fm_app;

SELECT ensure_file_activity_partition(now());
SELECT ensure_file_activity_partition(now() + interval '1 day');

-- +goose Down
-- Restores the 00001 structure. Activity rows are not converted back.
DROP VIEW IF EXISTS session_summary;
DROP FUNCTION IF EXISTS prune_before(timestamptz);
DROP TABLE IF EXISTS file_activity;
DROP FUNCTION IF EXISTS ensure_file_activity_partition(timestamptz);
ALTER TABLE sessions DROP COLUMN IF EXISTS kind;
DROP TYPE IF EXISTS session_kind;

CREATE TABLE file_events (
    id          bigint      GENERATED ALWAYS AS IDENTITY,
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
