// Package ledger holds the posting engine: the only package permitted to
// run balance-mutating queries. The RPC layer goes through the engine for
// anything that touches the balances table; provisioning reads and the
// asset/holder/wallet tables may be accessed directly via internal/store.
//
// Ticket #3 lands account creation (account row + zeroed balance row in one
// database transaction) and wallet balance reads. The posting engine
// itself (zero-sum validation, precision math, pending/posted/voided
// transitions) arrives with the posting-engine ticket.
package ledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/use-fabrica/ledger/internal/store"
)

// Engine owns every balance-mutating code path. It wraps the sqlc query
// set plus the connection pool so multi-statement invariants (account +
// balance row) run inside a single database transaction.
type Engine struct {
	pool *pgxpool.Pool
	q    *store.Queries
}

func New(pool *pgxpool.Pool) *Engine {
	return &Engine{pool: pool, q: store.New(pool)}
}

// CreateAccount creates the wallet × asset account together with its
// zeroed balance row (posted = 0, pending = 0) in one transaction, so an
// account can never exist without a balance. The unique (wallet_id,
// asset_id) pair and the wallet/asset foreign keys are enforced by the
// schema; callers map the resulting Postgres errors.
func (e *Engine) CreateAccount(
	ctx context.Context,
	id string,
	walletID string,
	assetID string,
	allowNegative bool,
) (store.Account, error) {
	tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return store.Account{}, fmt.Errorf("ledger: begin create account: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := e.q.WithTx(tx)
	account, err := q.CreateAccount(ctx, store.CreateAccountParams{
		ID:            id,
		WalletID:      walletID,
		AssetID:       assetID,
		AllowNegative: allowNegative,
	})
	if err != nil {
		return store.Account{}, fmt.Errorf("ledger: create account: %w", err)
	}
	if err := q.CreateBalance(ctx, account.ID); err != nil {
		return store.Account{}, fmt.Errorf("ledger: create balance: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Account{}, fmt.Errorf("ledger: commit create account: %w", err)
	}
	return account, nil
}

// WalletBalances returns every account of the wallet with its materialized
// balances. Accounts always have a balance row (CreateAccount guarantees
// it), so this is a plain join, not a posting-path concern.
func (e *Engine) WalletBalances(ctx context.Context, walletID string) ([]store.GetWalletBalancesRow, error) {
	rows, err := e.q.GetWalletBalances(ctx, walletID)
	if err != nil {
		return nil, fmt.Errorf("ledger: get wallet balances: %w", err)
	}
	return rows, nil
}
