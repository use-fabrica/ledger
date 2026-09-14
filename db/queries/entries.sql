-- name: CreateEntry :exec
INSERT INTO entries (id, transaction_id, account_id, amount)
VALUES ($1, $2, $3, $4);

-- name: GetEntriesByTransaction :many
SELECT id, transaction_id, account_id, amount, created_at
FROM entries
WHERE transaction_id = $1
ORDER BY created_at, id;
