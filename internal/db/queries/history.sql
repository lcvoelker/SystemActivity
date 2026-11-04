-- name: FileHistory :many
SELECT v.sha256, b.size_bytes, v.valid_from, v.valid_to
FROM file_versions v
JOIN files f ON f.id = v.file_id
JOIN blobs b ON b.sha256 = v.sha256
WHERE f.path = $1
ORDER BY v.valid_from;

-- Other paths whose current content is identical to the given hash.
-- name: PathsWithContent :many
SELECT f.path
FROM file_versions v
JOIN files f ON f.id = v.file_id
WHERE v.sha256 = $1
  AND v.valid_to IS NULL
  AND f.path <> sqlc.arg(exclude_path)::text
ORDER BY f.path;
