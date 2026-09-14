-- name: GetAsset :one
SELECT id, code, precision, metadata, created_at
FROM assets
WHERE id = $1;
