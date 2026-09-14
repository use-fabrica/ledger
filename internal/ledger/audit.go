// Audit and lookup reads (ticket #6): the engine's read-only surface for
// the AuditService. Nothing here mutates state — these methods only run
// SELECTs, so they take no locks and never participate in the posting
// invariants from ledger.go. Reads go through the engine rather than the
// store so the RPC layer keeps a single dependency (established in
// ticket #3).
package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/use-fabrica/ledger/internal/store"
)

// ErrWalletNotFound wraps a lookup or listing scoped to a missing wallet.
// Defined here rather than in ledger.go (ticket #5's file); the RPC layer
// maps it to CodeNotFound like the other engine sentinels.
var ErrWalletNotFound = errors.New("ledger: wallet not found")

// Page is one offset-based page request. Limit is the caller's page size
// plus one: fetching one extra row lets the engine report whether a
// further page exists without a COUNT query, then the extra row is
// trimmed. Offset is 0-based.
type Page struct {
	Limit  int32
	Offset int32
}

// PageInfo describes what the caller can do after a page. NextOffset is
// nil exactly when the returned rows are the last page.
type PageInfo struct {
	NextOffset *int32
}

// ListEntries returns one page of the account's immutable entries in
// chronological order (created_at, id). The account must exist — listing
// an unknown account is a caller bug, not an empty history. Running
// totals are derivable by accumulating the signed amounts in order; the
// materialized balance equals that accumulation.
func (e *Engine) ListEntries(ctx context.Context, accountID string, page Page) ([]store.Entry, PageInfo, error) {
	if _, err := e.q.GetAccount(ctx, accountID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, PageInfo{}, fmt.Errorf("ledger: %w: %s", ErrAccountNotFound, accountID)
		}
		return nil, PageInfo{}, fmt.Errorf("ledger: load account: %w", err)
	}
	rows, err := e.q.ListAccountEntries(ctx, store.ListAccountEntriesParams{
		AccountID: accountID,
		Limit:     page.Limit,
		Offset:    page.Offset,
	})
	if err != nil {
		return nil, PageInfo{}, fmt.Errorf("ledger: list entries: %w", err)
	}
	entries, info := trimPage(rows, page)
	return entries, info, nil
}

// ListWalletTransactions returns one page of every posted transaction
// touching any account of the wallet, chronologically (created_at, id),
// entries included. A transaction that moves value between two accounts
// of the same wallet appears exactly once. Status is scoped to 'posted'
// only for now — pending-state filtering arrives with ticket #5. The
// wallet must exist.
func (e *Engine) ListWalletTransactions(ctx context.Context, walletID string, page Page) ([]PostedTransaction, PageInfo, error) {
	if _, err := e.q.GetWallet(ctx, walletID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, PageInfo{}, fmt.Errorf("ledger: %w: %s", ErrWalletNotFound, walletID)
		}
		return nil, PageInfo{}, fmt.Errorf("ledger: load wallet: %w", err)
	}
	rows, err := e.q.ListWalletPostedTransactions(ctx, store.ListWalletPostedTransactionsParams{
		WalletID: walletID,
		Limit:    page.Limit,
		Offset:   page.Offset,
	})
	if err != nil {
		return nil, PageInfo{}, fmt.Errorf("ledger: list wallet transactions: %w", err)
	}
	transactions, info := trimPage(rows, page)
	result := make([]PostedTransaction, 0, len(transactions))
	for _, transaction := range transactions {
		entries, err := e.q.GetEntriesByTransaction(ctx, transaction.ID)
		if err != nil {
			return nil, PageInfo{}, fmt.Errorf("ledger: load entries: %w", err)
		}
		result = append(result, PostedTransaction{Transaction: transaction, Entries: entries})
	}
	return result, info, nil
}

// GetTransactionByIdempotencyKey returns the transaction the key
// originally wrote, entries included. Idempotency keys are unique, so the
// match is exact; a miss is ErrTransactionNotFound.
func (e *Engine) GetTransactionByIdempotencyKey(ctx context.Context, key string) (PostedTransaction, error) {
	transaction, err := e.q.GetTransactionByIdempotencyKey(ctx, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PostedTransaction{}, fmt.Errorf("ledger: %w: key %s", ErrTransactionNotFound, key)
		}
		return PostedTransaction{}, fmt.Errorf("ledger: get transaction by idempotency key: %w", err)
	}
	return e.withEntries(ctx, transaction)
}

// GetTransactionByReference returns the most recently created transaction
// carrying the provider reference, entries included. The schema indexes
// but does not unique references — a charge and its refund may share one —
// so "the" transaction for a reference is defined as the latest match; a
// miss is ErrTransactionNotFound.
func (e *Engine) GetTransactionByReference(ctx context.Context, reference string) (PostedTransaction, error) {
	transaction, err := e.q.GetTransactionByReference(ctx, pgtype.Text{String: reference, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PostedTransaction{}, fmt.Errorf("ledger: %w: reference %s", ErrTransactionNotFound, reference)
		}
		return PostedTransaction{}, fmt.Errorf("ledger: get transaction by reference: %w", err)
	}
	return e.withEntries(ctx, transaction)
}

// withEntries loads the entries of one transaction row.
func (e *Engine) withEntries(ctx context.Context, transaction store.Transaction) (PostedTransaction, error) {
	entries, err := e.q.GetEntriesByTransaction(ctx, transaction.ID)
	if err != nil {
		return PostedTransaction{}, fmt.Errorf("ledger: get entries: %w", err)
	}
	return PostedTransaction{Transaction: transaction, Entries: entries}, nil
}

// trimPage cuts the extra probe row (fetched Limit = page size + 1) and
// reports whether a further page exists.
func trimPage[T any](rows []T, page Page) ([]T, PageInfo) {
	var info PageInfo
	if int32(len(rows)) > page.Limit-1 {
		next := page.Offset + (page.Limit - 1)
		info.NextOffset = &next
		rows = rows[:page.Limit-1]
	}
	return rows, info
}
