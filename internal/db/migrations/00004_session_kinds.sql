-- New session kinds: "mixed" (shown as "work + organization": both
-- behaviors are a substantial part of the session) and "brief" (too few
-- events to tell). "other" is no longer produced but stays valid for old
-- rows. Additive only: safe to apply unattended at startup.

-- +goose Up
ALTER TYPE session_kind ADD VALUE IF NOT EXISTS 'mixed';
ALTER TYPE session_kind ADD VALUE IF NOT EXISTS 'brief';

-- +goose Down
-- Postgres can't remove enum values, so the type is rebuilt; sessions with
-- the new kinds fall back to "other".
DROP VIEW session_summary;
ALTER TABLE sessions ALTER COLUMN kind TYPE text;
UPDATE sessions SET kind = 'other' WHERE kind IN ('mixed', 'brief');
DROP TYPE session_kind;
CREATE TYPE session_kind AS ENUM ('work', 'organization', 'other');
ALTER TABLE sessions ALTER COLUMN kind TYPE session_kind USING kind::session_kind;
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
       coalesce(sum(a.removes), 0)::bigint                        AS removes,
       s.end_reason
FROM sessions s
LEFT JOIN file_activity a ON a.session_id = s.id
GROUP BY s.id;
