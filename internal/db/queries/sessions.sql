-- name: StartSession :one
INSERT INTO sessions (host, root_path, started_at)
VALUES ($1, $2, $3)
RETURNING id;

-- name: EndSession :exec
UPDATE sessions
SET ended_at = @ended_at::timestamptz,
    kind = @kind::session_kind
WHERE id = @id;

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
