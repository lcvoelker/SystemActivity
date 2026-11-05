-- name: StartSession :one
INSERT INTO sessions (host, root_path, started_at)
VALUES ($1, $2, $3)
RETURNING id;

-- name: EndSession :exec
UPDATE sessions
SET ended_at = @ended_at::timestamptz,
    kind = @kind::session_kind,
    end_reason = @end_reason::session_end
WHERE id = @id;

-- Saved counts of already-closed sessions, for reclassifying them after
-- activity recovered from a previous run arrives late.
-- name: ClosedSessionCounts :many
SELECT session_id,
       coalesce(last_activity_at, started_at)::timestamptz AS last_seen_at,
       creates, writes, renames, removes
FROM session_summary
WHERE session_id = ANY(@ids::bigint[])
  AND ended_at IS NOT NULL;

-- Per-file totals for classifying sessions from saved data: a file both
-- created and saved in a session counts as work (session.BehaviorFromFiles).
-- name: SessionFileCounts :many
SELECT a.session_id,
       a.file_id,
       sum(a.creates)::bigint AS creates,
       sum(a.writes)::bigint  AS writes,
       sum(a.renames)::bigint AS renames,
       sum(a.removes)::bigint AS removes
FROM file_activity a
WHERE a.session_id = ANY(@ids::bigint[])
GROUP BY a.session_id, a.file_id;

-- name: ReclassifySession :exec
UPDATE sessions
SET kind = @kind::session_kind,
    ended_at = GREATEST(ended_at, @last_seen_at::timestamptz)
WHERE id = @id;

-- name: ExistingSessions :many
SELECT id
FROM sessions
WHERE id = ANY(@ids::bigint[]);

-- name: SessionDetail :one
SELECT *
FROM session_summary
WHERE session_id = $1;

-- Every file a session touched, in the order it was first touched.
-- name: SessionFiles :many
SELECT f.path, a.old_path, a.creates, a.writes, a.renames, a.removes
FROM file_activity a
JOIN files f ON f.id = a.file_id
WHERE a.session_id = $1
ORDER BY a.first_at, f.path;

-- name: RecentSessions :many
SELECT *
FROM session_summary
ORDER BY started_at DESC
LIMIT $1;

-- Sessions left open by a watcher on this host and root that was killed
-- before it could close them.
-- name: OpenSessions :many
SELECT session_id,
       coalesce(last_activity_at, started_at)::timestamptz AS last_seen_at,
       creates, writes, renames, removes
FROM session_summary
WHERE ended_at IS NULL
  AND host = $1
  AND root_path = $2;
