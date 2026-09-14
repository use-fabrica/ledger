-- name: CreateTransaction :one
INSERT INTO transactions (id, idempotency_key, reference, status, posted_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, idempotency_key, reference, status, metadata, created_at, posted_at, voided_at;

-- name: GetTransactionByID :one
SELECT id, idempotency_key, reference, status, metadata, created_at, posted_at, voided_at
FROM transactions
WHERE id = $1;

-- name: GetTransactionByIdempotencyKey :one
SELECT id, idempotency_key, reference, status, metadata, created_at, posted_at, voided_at
FROM transactions
WHERE idempotency_key = $1;
