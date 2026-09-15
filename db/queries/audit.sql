-- Audit and lookup reads (ticket #6). These queries never mutate state;
-- they back the AuditService list/lookup endpoints. Wallet transaction
-- listing is not status-scoped: it returns every status (posted, pending,
-- voided) unless the caller passes an exact status filter.

-- name: ListAccountEntries :many
-- One account's complete movement history, chronological. The caller
-- passes limit = page_size + 1 so it can detect a further page without a
-- COUNT query, then trims the extra row.
SELECT id, transaction_id, account_id, amount, created_at
FROM entries
WHERE account_id = $1
ORDER BY created_at, id
LIMIT $2 OFFSET $3;

-- name: ListWalletTransactions :many
-- Every transaction that touches any account of the wallet — of any
-- status by default, or exactly the requested status when one is passed.
-- A transaction debiting one wallet account and crediting another of the
-- same wallet still appears exactly once (EXISTS, not a join).
SELECT t.id, t.idempotency_key, t.reference, t.status, t.metadata, t.created_at, t.posted_at, t.voided_at
FROM transactions t
WHERE (sqlc.narg('status')::text IS NULL OR t.status = sqlc.narg('status'))
  AND EXISTS (
      SELECT 1
      FROM entries e
      JOIN accounts a ON a.id = e.account_id
      WHERE e.transaction_id = t.id
        AND a.wallet_id = sqlc.arg('wallet_id')
  )
ORDER BY t.created_at, t.id
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: GetTransactionByReference :one
-- Provider references (e.g. a Stripe charge ID) are indexed but not
-- unique per the schema: a reference may legitimately repeat across a
-- charge and its refund. The lookup returns the most recent match —
-- reconciliation callers that need the full set get it via ListWallet
-- or a future filter.
SELECT id, idempotency_key, reference, status, metadata, created_at, posted_at, voided_at
FROM transactions
WHERE reference = $1
ORDER BY created_at DESC, id DESC
LIMIT 1;
