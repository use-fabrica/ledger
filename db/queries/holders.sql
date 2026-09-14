-- name: CreateHolder :one
INSERT INTO holders (id, type, external_ref, metadata)
VALUES ($1, $2, $3, $4)
RETURNING id, type, external_ref, metadata, created_at;

-- name: GetHolder :one
SELECT id, type, external_ref, metadata, created_at
FROM holders
WHERE id = $1;
