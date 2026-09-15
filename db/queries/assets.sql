-- name: CreateAsset :one
INSERT INTO assets (id, code, precision, metadata)
VALUES ($1, $2, $3, $4)
RETURNING id, code, precision, metadata, created_at;

-- name: GetAsset :one
SELECT id, code, precision, metadata, created_at
FROM assets
WHERE id = $1;

-- name: ListAssets :many
SELECT id, code, precision, metadata, created_at
FROM assets
ORDER BY created_at, id;
