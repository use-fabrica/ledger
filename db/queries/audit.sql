-- Audit and lookup reads (ticket #6). These queries never mutate state;
-- they back the AuditService list/lookup endpoints. Wallet transaction
-- listing is scoped to status 'posted' for now — pending-state filtering
-- arrives with the pending-lifecycle ticket (#5).

-- name: ListAccountEntries :many
-- One account's complete movement history, chronological. The caller
-- passes limit = page_size + 1 so it can detect a further page without a
-- COUNT query, then trims the extra row.
SELECT id, transaction_id, account_id, amount, created_at
FROM entries
WHERE account_id = $1
ORDER BY created_at, id
LIMIT $2 OFFSET $3;

-- name: ListWalletPostedTransactions :many
-- Every posted transaction that touches any account of the wallet. A
-- transaction debiting one wallet account and crediting another of the
-- same wallet still appears exactly once (EXISTS, not a join).
SELECT t.id, t.idempotency_key, t.reference, t.status, t.metadata, t.created_at, t.posted_at, t.voided_at
FROM transactions t
WHERE t.status = 'posted'
  AND EXISTS (
      SELECT 1
      FROM entries e
      JOIN accounts a ON a.id = e.account_id
      WHERE e.transaction_id = t.id
        AND a.wallet_id = $1
  )
ORDER BY t.created_at, t.id
LIMIT $2 OFFSET $3;

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
