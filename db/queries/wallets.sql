-- name: CreateWallet :one
INSERT INTO wallets (id, holder_id, name, metadata)
VALUES ($1, $2, $3, $4)
RETURNING id, holder_id, name, metadata, created_at;

-- name: GetWallet :one
SELECT id, holder_id, name, metadata, created_at
FROM wallets
WHERE id = $1;
