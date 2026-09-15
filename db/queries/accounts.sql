-- name: CreateAccount :one
INSERT INTO accounts (id, wallet_id, asset_id, allow_negative)
VALUES ($1, $2, $3, $4)
RETURNING id, wallet_id, asset_id, allow_negative, created_at;

-- name: GetAccount :one
SELECT id, wallet_id, asset_id, allow_negative, created_at
FROM accounts
WHERE id = $1;

-- name: GetAccountByWalletAsset :one
SELECT id, wallet_id, asset_id, allow_negative, created_at
FROM accounts
WHERE wallet_id = $1 AND asset_id = $2;

-- name: ListAccountsByWallet :many
SELECT id, wallet_id, asset_id, allow_negative, created_at
FROM accounts
WHERE wallet_id = $1
ORDER BY created_at, id;
