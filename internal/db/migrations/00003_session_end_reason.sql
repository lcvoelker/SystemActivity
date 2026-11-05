-- Records how each session ended, so summaries can tell exact sessions from
-- approximate ones. Additive only: safe to apply unattended at startup.

-- +goose Up
-- stopped:   the user typed "end" or pressed Ctrl+C
-- idle:      the inactivity timeout closed it
-- recovered: closed by a later watcher after a kill or failed exit; the end
--            time is the last saved activity and may be approximate
CREATE TYPE session_end AS ENUM ('stopped', 'idle', 'recovered');
ALTER TABLE sessions ADD COLUMN end_reason session_end;

-- Sessions closed before this migration keep a NULL end_reason (unknown).
CREATE OR REPLACE VIEW session_summary AS
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

-- +goose Down
DROP VIEW session_summary;
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
ALTER TABLE sessions DROP COLUMN end_reason;
DROP TYPE session_end;
