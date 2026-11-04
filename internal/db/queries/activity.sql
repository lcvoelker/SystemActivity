-- name: EnsureFileActivityPartition :one
SELECT ensure_file_activity_partition(@at::timestamptz)::text AS partition;

-- Returns ids for every input path, inserting the ones not seen before.
-- The final SELECT reads the pre-insert snapshot, so new rows come from
-- ins and existing rows from files, with no overlap.
-- name: UpsertFiles :many
WITH input AS (
    SELECT DISTINCT unnest(@paths::text[]) AS path
), ins AS (
    INSERT INTO files (path)
    SELECT path FROM input
    ON CONFLICT (path) DO NOTHING
    RETURNING id, path
)
SELECT id, path FROM ins
UNION ALL
SELECT f.id, f.path FROM files f JOIN input i ON i.path = f.path;

-- name: InsertBlobs :exec
INSERT INTO blobs (sha256, size_bytes)
SELECT unnest(@hashes::bytea[]), unnest(@sizes::bigint[])
ON CONFLICT (sha256) DO NOTHING;

-- name: InsertFileActivity :copyfrom
INSERT INTO file_activity (session_id, file_id, first_at, last_at, creates, writes, renames, removes,
                           last_op, old_path, sha256, size_bytes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- Moves a file to new content (or to no content when sha256 is NULL, i.e.
-- removed). Closes the current version only if the content differs, then
-- opens a new one unless the file already holds this content.
-- name: RecordVersion :batchexec
WITH closed AS (
    UPDATE file_versions
    SET valid_to = GREATEST(@at::timestamptz, valid_from)
    WHERE file_id = @file_id::bigint
      AND valid_to IS NULL
      AND sha256 IS DISTINCT FROM sqlc.narg(sha256)::bytea
    RETURNING valid_to
)
INSERT INTO file_versions (file_id, sha256, valid_from)
SELECT @file_id::bigint,
       sqlc.narg(sha256)::bytea,
       GREATEST(@at::timestamptz, (SELECT max(valid_to) FROM closed))
WHERE sqlc.narg(sha256)::bytea IS NOT NULL
  AND NOT EXISTS (
      SELECT 1
      FROM file_versions v
      WHERE v.file_id = @file_id::bigint
        AND v.valid_to IS NULL
        AND v.sha256 = sqlc.narg(sha256)::bytea
  );

-- name: PruneBefore :one
SELECT p.dropped_partitions::integer AS dropped_partitions,
       p.deleted_sessions::bigint AS deleted_sessions
FROM prune_before(@cutoff::timestamptz) AS p;
