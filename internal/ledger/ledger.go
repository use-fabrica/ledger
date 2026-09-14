// Package ledger holds the posting engine: the only package permitted to
// run balance-mutating queries. The RPC layer goes through the engine for
// anything that touches the balances table; provisioning reads and the
// asset/holder/wallet tables may be accessed directly via internal/store.
//
// Posting invariants — zero net per asset, per-asset precision, overdraft
// rules — are enforced inside one database transaction, with affected
// balance rows locked FOR UPDATE before mutation (ticket #4). The pending
// lifecycle (create-pending earmarks, post settles, void releases) lives
// in CreatePending, PostTransaction, and VoidTransaction (ticket #5).
//
// Sign convention for the pending balance column (ADR 0006): pending
// holds the net earmarked amount with the same sign as the entries that
// created it — negative for outgoing holds, positive for incoming — so
// spendable funds are always posted + pending, and releasing an earmark
// subtracts the same delta that created it.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/use-fabrica/ledger/internal/store"
)

// Engine owns every balance-mutating code path. It wraps the sqlc query
// set plus the connection pool so multi-statement invariants (account +
// balance row, transaction + entries + balances) run inside a single
// database transaction.
type Engine struct {
	pool *pgxpool.Pool
	q    *store.Queries
}

func New(pool *pgxpool.Pool) *Engine {
	return &Engine{pool: pool, q: store.New(pool)}
}

// Sentinel errors the posting engine raises. The RPC layer maps them to
// Connect codes; everything else is an internal failure.
var (
	// ErrMissingIdempotencyKey: every transaction write must carry a
	// client-supplied key so retries can be deduplicated.
	ErrMissingIdempotencyKey = errors.New("ledger: idempotency key is required")
	// ErrTooFewEntries: a transaction needs at least two entries — value
	// must move between at least two accounts.
	ErrTooFewEntries = errors.New("ledger: a transaction needs at least two entries")
	// ErrZeroAmount: the entries table rejects zero amounts; catch the
	// client error before it becomes a constraint violation.
	ErrZeroAmount = errors.New("ledger: entry amounts must be non-zero")
	// ErrAccountNotFound wraps an entry referencing a missing account.
	ErrAccountNotFound = errors.New("ledger: account not found")
	// ErrTransactionNotFound wraps a lookup of a missing transaction.
	ErrTransactionNotFound = errors.New("ledger: transaction not found")
	// ErrPrecisionExceeded: an amount carries more fractional digits than
	// its asset's precision allows.
	ErrPrecisionExceeded = errors.New("ledger: amount exceeds asset precision")
	// ErrNonZeroSum: the signed entries of a transaction must net to zero
	// per asset, or value would be created or destroyed.
	ErrNonZeroSum = errors.New("ledger: entries do not net to zero per asset")
	// ErrOverdraft: a resulting posted balance would go negative on an
	// account that is not flagged allow_negative.
	ErrOverdraft = errors.New("ledger: insufficient posted balance")
	// ErrTransactionNotPending: post/void target a transaction that is
	// already posted or voided. Terminal states are final — the journal
	// is append-only, so a settled or released transaction can never
	// move again.
	ErrTransactionNotPending = errors.New("ledger: transaction is not pending")
)

// PostEntry is one signed amount applied to one account, as requested by
// the caller. Amount is a signed decimal string ("-100.00", "42.5").
type PostEntry struct {
	AccountID string
	Amount    decimal.Decimal
}

// PostParams is one atomic posting request: a required idempotency key, an
// optional provider reference, and two or more signed entries.
type PostParams struct {
	IdempotencyKey string
	Reference      string
	Entries        []PostEntry
}

// PostedTransaction is the result of a post: the transaction row, its
// entries, and whether this call was a replay of an earlier write (same
// idempotency key — nothing moved).
type PostedTransaction struct {
	Transaction store.Transaction
	Entries     []store.Entry
	Replay      bool
}

// Post creates and posts a transaction atomically. Everything — the
// idempotency check, validation, balance locks, the transaction and entry
// inserts, and the balance updates — runs in one database transaction, so
// a rejected post writes nothing and a committed post moves balances
// exactly once. Transactions are created directly in status 'posted';
// use CreatePending to earmark funds first.
func (e *Engine) Post(ctx context.Context, p PostParams) (PostedTransaction, error) {
	return e.create(ctx, p, "posted")
}

// CreatePending creates a transaction in status 'pending' that earmarks
// funds: the same validation as Post (zero-sum per asset, precision,
// idempotent replay), but the balance movement lands on the pending
// column only — posted balances are untouched. A pending creation that
// would drive the available balance (posted + pending) negative on an
// account that is not flagged allow_negative is rejected, even when the
// posted balance alone would cover the movement.
func (e *Engine) CreatePending(ctx context.Context, p PostParams) (PostedTransaction, error) {
	return e.create(ctx, p, "pending")
}

// create is the shared body of Post and CreatePending: shape validation,
// the idempotency-key replay check, and the write itself, all in one
// database transaction. status is the initial transaction status and the
// balance column the movement lands on.
func (e *Engine) create(ctx context.Context, p PostParams, status string) (PostedTransaction, error) {
	if p.IdempotencyKey == "" {
		return PostedTransaction{}, ErrMissingIdempotencyKey
	}
	if len(p.Entries) < 2 {
		return PostedTransaction{}, ErrTooFewEntries
	}
	for _, entry := range p.Entries {
		if entry.AccountID == "" {
			return PostedTransaction{}, fmt.Errorf("ledger: %w: empty account id", ErrAccountNotFound)
		}
		if entry.Amount.IsZero() {
			return PostedTransaction{}, ErrZeroAmount
		}
	}

	tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return PostedTransaction{}, fmt.Errorf("ledger: begin post: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := e.q.WithTx(tx)

	// Idempotency: a replay returns the original transaction, entries
	// included, with no additional balance movement.
	if original, err := q.GetTransactionByIdempotencyKey(ctx, p.IdempotencyKey); err == nil {
		entries, err := q.GetEntriesByTransaction(ctx, original.ID)
		if err != nil {
			return PostedTransaction{}, fmt.Errorf("ledger: load replayed entries: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return PostedTransaction{}, fmt.Errorf("ledger: commit replay: %w", err)
		}
		return PostedTransaction{Transaction: original, Entries: entries, Replay: true}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return PostedTransaction{}, fmt.Errorf("ledger: idempotency check: %w", err)
	}

	created, err := e.writeLocked(ctx, q, p, status)
	if err != nil {
		return PostedTransaction{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PostedTransaction{}, fmt.Errorf("ledger: commit post: %w", err)
	}
	return created, nil
}

// writeLocked runs the validation and writes of a create inside the
// caller's transaction, after the idempotency check has established this
// key is new. status selects the balance column the movement lands on and
// the overdraft rule:
//   - "posted": the resulting posted balance must stay non-negative.
//   - "pending": the resulting available balance (posted + pending) must
//     stay non-negative — earmarking may not overdraw spendable funds.
func (e *Engine) writeLocked(ctx context.Context, q *store.Queries, p PostParams, status string) (PostedTransaction, error) {
	// Resolve every entry's account; the double-entry rules (one asset per
	// account, allow_negative flag) live on the account row.
	accounts := make([]store.Account, len(p.Entries))
	for i, entry := range p.Entries {
		account, err := q.GetAccount(ctx, entry.AccountID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return PostedTransaction{}, fmt.Errorf("ledger: %w: %s", ErrAccountNotFound, entry.AccountID)
			}
			return PostedTransaction{}, fmt.Errorf("ledger: load account: %w", err)
		}
		accounts[i] = account
	}

	// Precision: no amount may carry more fractional digits than its
	// asset allows. Assets are few, so the per-post lookup is cheap and
	// keeps the query set boring.
	precisions := make(map[string]int32, len(accounts))
	for _, account := range accounts {
		if _, ok := precisions[account.AssetID]; ok {
			continue
		}
		asset, err := q.GetAsset(ctx, account.AssetID)
		if err != nil {
			return PostedTransaction{}, fmt.Errorf("ledger: load asset: %w", err)
		}
		precisions[account.AssetID] = asset.Precision
	}
	for i, entry := range p.Entries {
		if PrecisionExceeded(entry.Amount, precisions[accounts[i].AssetID]) {
			return PostedTransaction{}, fmt.Errorf("ledger: %w: amount %s has more than %d fractional digits",
				ErrPrecisionExceeded, entry.Amount, precisions[accounts[i].AssetID])
		}
	}

	// Zero-sum: the signed entries must net to zero per asset, grouped by
	// the asset of each entry's account.
	if err := ValidateZeroSum(p.Entries, accounts); err != nil {
		return PostedTransaction{}, err
	}

	// Lock the affected balance rows in sorted account order so
	// concurrent writers lock rows in one global order and cannot
	// deadlock.
	deltas := netDeltas(p.Entries)
	locked := make(map[string]store.Balance, len(deltas))
	accountIDs := make([]string, 0, len(deltas))
	for accountID := range deltas {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Strings(accountIDs)
	for _, accountID := range accountIDs {
		balance, err := q.LockBalance(ctx, accountID)
		if err != nil {
			return PostedTransaction{}, fmt.Errorf("ledger: lock balance: %w", err)
		}
		locked[accountID] = balance
	}

	// Overdraft: on post the resulting posted balance must stay
	// non-negative; on a pending create the resulting available balance
	// (posted + pending) must, so earmarked funds cannot be double-spent.
	for accountID, delta := range deltas {
		result := locked[accountID].Posted.Add(delta)
		if status == "pending" {
			result = result.Add(locked[accountID].Pending)
		}
		if result.IsNegative() {
			for _, account := range accounts {
				if account.ID == accountID && !account.AllowNegative {
					return PostedTransaction{}, fmt.Errorf("ledger: %w: account %s available would go to %s",
						ErrOverdraft, accountID, result)
				}
			}
		}
	}

	// Write: transaction, entries (UUIDv7 per ADR 0008), and balance
	// updates — same transaction. A pending create leaves posted_at NULL;
	// posted_at is stamped by PostTransaction, voided_at by VoidTransaction.
	postedAt := pgtype.Timestamptz{}
	if status == "posted" {
		postedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	}
	transactionID, err := uuid.NewV7()
	if err != nil {
		return PostedTransaction{}, fmt.Errorf("ledger: generate transaction id: %w", err)
	}
	transaction, err := q.CreateTransaction(ctx, store.CreateTransactionParams{
		ID:             pgtype.UUID{Bytes: transactionID, Valid: true},
		IdempotencyKey: p.IdempotencyKey,
		Reference:      pgtype.Text{String: p.Reference, Valid: p.Reference != ""},
		Status:         status,
		PostedAt:       postedAt,
	})
	if err != nil {
		// A concurrent duplicate key lands here rather than in the
		// idempotency check; the unique index is the final arbiter. Treat
		// it as a replay of the winner's transaction.
		if isUniqueViolation(err) {
			original, lerr := q.GetTransactionByIdempotencyKey(ctx, p.IdempotencyKey)
			if lerr != nil {
				return PostedTransaction{}, fmt.Errorf("ledger: race on idempotency key: %w", lerr)
			}
			entries, lerr := q.GetEntriesByTransaction(ctx, original.ID)
			if lerr != nil {
				return PostedTransaction{}, fmt.Errorf("ledger: load raced entries: %w", lerr)
			}
			return PostedTransaction{Transaction: original, Entries: entries, Replay: true}, nil
		}
		return PostedTransaction{}, fmt.Errorf("ledger: create transaction: %w", err)
	}

	entries := make([]store.Entry, 0, len(p.Entries))
	for _, entry := range p.Entries {
		entryID, err := uuid.NewV7()
		if err != nil {
			return PostedTransaction{}, fmt.Errorf("ledger: generate entry id: %w", err)
		}
		if err := q.CreateEntry(ctx, store.CreateEntryParams{
			ID:            pgtype.UUID{Bytes: entryID, Valid: true},
			TransactionID: transaction.ID,
			AccountID:     entry.AccountID,
			Amount:        entry.Amount,
		}); err != nil {
			return PostedTransaction{}, fmt.Errorf("ledger: create entry: %w", err)
		}
		entries = append(entries, store.Entry{
			ID:            pgtype.UUID{Bytes: entryID, Valid: true},
			TransactionID: transaction.ID,
			AccountID:     entry.AccountID,
			Amount:        entry.Amount,
		})
	}

	for accountID, delta := range deltas {
		if status == "pending" {
			err = q.UpdateBalancePending(ctx, store.UpdateBalancePendingParams{
				AccountID: accountID,
				Pending:   delta,
			})
		} else {
			err = q.UpdateBalancePosted(ctx, store.UpdateBalancePostedParams{
				AccountID: accountID,
				Posted:    delta,
			})
		}
		if err != nil {
			return PostedTransaction{}, fmt.Errorf("ledger: update balance: %w", err)
		}
	}

	return PostedTransaction{Transaction: transaction, Entries: entries}, nil
}

// PostTransaction settles a pending transaction: pending → posted. In one
// database transaction it locks the transaction row, verifies the status
// is still pending (posted and voided are terminal — re-posting either is
// rejected with ErrTransactionNotPending and moves nothing, so a retry
// after a timeout is safe), locks the affected balance rows FOR UPDATE in
// sorted order, moves each account's earmarked amount from the pending
// column onto the posted column, enforces allow_negative on the resulting
// posted balance, and stamps posted_at.
func (e *Engine) PostTransaction(ctx context.Context, id uuid.UUID) (PostedTransaction, error) {
	return e.transition(ctx, id, true)
}

// VoidTransaction releases a pending transaction's earmark: pending →
// voided. The pending amounts are removed with no posted movement, so
// value settles nowhere; voided_at is stamped. Like PostTransaction it
// runs in one database transaction, locks the transaction row first, and
// rejects transactions that are no longer pending.
func (e *Engine) VoidTransaction(ctx context.Context, id uuid.UUID) (PostedTransaction, error) {
	return e.transition(ctx, id, false)
}

// transition is the shared body of PostTransaction (settle) and
// VoidTransaction (release).
func (e *Engine) transition(ctx context.Context, id uuid.UUID, settle bool) (PostedTransaction, error) {
	tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return PostedTransaction{}, fmt.Errorf("ledger: begin transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := e.q.WithTx(tx)

	// The transaction row lock serializes concurrent post/void attempts on
	// the same transaction: only the first transition wins, every other
	// sees the updated status and is rejected.
	transaction, err := q.LockTransaction(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PostedTransaction{}, fmt.Errorf("ledger: %w: %s", ErrTransactionNotFound, id)
		}
		return PostedTransaction{}, fmt.Errorf("ledger: lock transaction: %w", err)
	}
	if transaction.Status != "pending" {
		return PostedTransaction{}, fmt.Errorf("ledger: %w: status is %s", ErrTransactionNotPending, transaction.Status)
	}

	// Entries are immutable, so the per-account movement to settle or
	// release is exactly the net of the entries written at creation.
	entries, err := q.GetEntriesByTransaction(ctx, transaction.ID)
	if err != nil {
		return PostedTransaction{}, fmt.Errorf("ledger: load entries: %w", err)
	}
	movements := make([]PostEntry, len(entries))
	for i, entry := range entries {
		movements[i] = PostEntry{AccountID: entry.AccountID, Amount: entry.Amount}
	}
	deltas := netDeltas(movements)

	// Lock the affected balance rows in sorted account order, same global
	// order as every other balance mutation, so concurrent transitions
	// cannot deadlock.
	accountIDs := make([]string, 0, len(deltas))
	for accountID := range deltas {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Strings(accountIDs)
	locked := make(map[string]store.Balance, len(deltas))
	for _, accountID := range accountIDs {
		balance, err := q.LockBalance(ctx, accountID)
		if err != nil {
			return PostedTransaction{}, fmt.Errorf("ledger: lock balance: %w", err)
		}
		locked[accountID] = balance
	}

	// Settling lands the movement on the posted column: the resulting
	// posted balance must stay non-negative unless the account is flagged
	// allow_negative. Releasing only ever raises available, so it needs no
	// overdraft check.
	if settle {
		for _, accountID := range accountIDs {
			result := locked[accountID].Posted.Add(deltas[accountID])
			if !result.IsNegative() {
				continue
			}
			account, err := q.GetAccount(ctx, accountID)
			if err != nil {
				return PostedTransaction{}, fmt.Errorf("ledger: load account: %w", err)
			}
			if !account.AllowNegative {
				return PostedTransaction{}, fmt.Errorf("ledger: %w: account %s would go to %s",
					ErrOverdraft, accountID, result)
			}
		}
	}

	for _, accountID := range accountIDs {
		delta := deltas[accountID]
		if settle {
			err = q.MoveBalancePendingToPosted(ctx, store.MoveBalancePendingToPostedParams{
				AccountID: accountID,
				Posted:    delta,
			})
		} else {
			err = q.UpdateBalancePending(ctx, store.UpdateBalancePendingParams{
				AccountID: accountID,
				Pending:   delta.Neg(),
			})
		}
		if err != nil {
			return PostedTransaction{}, fmt.Errorf("ledger: update balance: %w", err)
		}
	}

	if settle {
		transaction, err = q.MarkTransactionPosted(ctx, transaction.ID)
	} else {
		transaction, err = q.MarkTransactionVoided(ctx, transaction.ID)
	}
	if err != nil {
		return PostedTransaction{}, fmt.Errorf("ledger: mark transaction: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return PostedTransaction{}, fmt.Errorf("ledger: commit transition: %w", err)
	}
	return PostedTransaction{Transaction: transaction, Entries: entries}, nil
}

// GetTransaction returns a transaction with its entries. Entries are
// immutable once written (append-only journal); this is a read.
func (e *Engine) GetTransaction(ctx context.Context, id uuid.UUID) (PostedTransaction, error) {
	transaction, err := e.q.GetTransactionByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PostedTransaction{}, fmt.Errorf("ledger: %w: %s", ErrTransactionNotFound, id)
		}
		return PostedTransaction{}, fmt.Errorf("ledger: get transaction: %w", err)
	}
	entries, err := e.q.GetEntriesByTransaction(ctx, transaction.ID)
	if err != nil {
		return PostedTransaction{}, fmt.Errorf("ledger: get entries: %w", err)
	}
	return PostedTransaction{Transaction: transaction, Entries: entries}, nil
}

// netDeltas folds the entry list into the net movement per account.
// Multiple entries against one account (a split) collapse into a single
// delta — that net is what one balance update applies, at creation,
// settle, and release alike.
func netDeltas(entries []PostEntry) map[string]decimal.Decimal {
	deltas := make(map[string]decimal.Decimal, len(entries))
	for _, entry := range entries {
		deltas[entry.AccountID] = deltas[entry.AccountID].Add(entry.Amount)
	}
	return deltas
}

// ValidateZeroSum returns ErrNonZeroSum unless the signed amounts net to
// zero within every asset group. Accounts[i].AssetID is the asset of
// entries[i]; entries hitting accounts in different assets are validated
// per asset independently.
func ValidateZeroSum(entries []PostEntry, accounts []store.Account) error {
	if len(entries) != len(accounts) {
		return fmt.Errorf("ledger: %w: entries/accounts length mismatch", ErrNonZeroSum)
	}
	sums := make(map[string]decimal.Decimal, len(entries))
	for i, entry := range entries {
		sums[accounts[i].AssetID] = sums[accounts[i].AssetID].Add(entry.Amount)
	}
	for assetID, sum := range sums {
		if !sum.IsZero() {
			return fmt.Errorf("ledger: %w: entries for asset %s net to %s", ErrNonZeroSum, assetID, sum)
		}
	}
	return nil
}

// PrecisionExceeded reports whether amount carries more fractional digits
// than precision allows (precision 2 accepts "1.23" and "1.2", rejects
// "1.234").
func PrecisionExceeded(amount decimal.Decimal, precision int32) bool {
	return -amount.Exponent() > precision
}

// isUniqueViolation reports whether err is a Postgres unique-violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
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
