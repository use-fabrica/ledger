-- name: CreateBalance :exec
INSERT INTO balances (account_id)
VALUES ($1);

-- name: GetWalletBalances :many
SELECT a.id, a.wallet_id, a.asset_id, a.allow_negative, a.created_at, b.posted, b.pending
FROM accounts a
JOIN balances b ON b.account_id = a.id
WHERE a.wallet_id = $1
ORDER BY a.created_at, a.id;

-- name: LockBalance :one
SELECT account_id, posted, pending, updated_at
FROM balances
WHERE account_id = $1
FOR UPDATE;

-- name: UpdateBalancePosted :exec
UPDATE balances
SET posted = posted + $2, updated_at = now()
WHERE account_id = $1;

-- name: UpdateBalancePending :exec
UPDATE balances
SET pending = pending + $2, updated_at = now()
WHERE account_id = $1;

-- name: MoveBalancePendingToPosted :exec
-- Settles an earmark: the pending movement is moved onto the posted
-- column (pending holds net earmarked amounts, so settling subtracts the
-- same delta that created the earmark).
UPDATE balances
SET posted = posted + $2, pending = pending - $2, updated_at = now()
WHERE account_id = $1;
