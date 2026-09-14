package rpc_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/shopspring/decimal"

	ledgerv1 "github.com/use-fabrica/ledger/proto/ledger/v1"
)

// postingFixture is a funded pair of accounts in one asset: a system
// account flagged allow_negative and a user account starting at zero.
// wallets tracks every wallet id created for the fixture so balances can
// be read back without a list-wallets API.
type postingFixture struct {
	asset   *ledgerv1.Asset
	system  *ledgerv1.Account
	user    *ledgerv1.Account
	wallets []string
}

func newPostingFixture(t *testing.T, env *env) postingFixture {
	t.Helper()
	asset := createAsset(t, env, "USD")

	systemHolder := createHolderWithType(t, env, "system-float", ledgerv1.HolderType_HOLDER_TYPE_SYSTEM)
	systemWallet := createWallet(t, env, systemHolder.GetId(), "float")
	system := createAccount(t, env, systemWallet.GetId(), asset.GetId(), true)

	userHolder := createHolder(t, env, "user-1")
	userWallet := createWallet(t, env, userHolder.GetId(), "main")
	user := createAccount(t, env, userWallet.GetId(), asset.GetId(), false)

	return postingFixture{
		asset:   asset,
		system:  system,
		user:    user,
		wallets: []string{systemWallet.GetId(), userWallet.GetId()},
	}
}

func createHolderWithType(t *testing.T, env *env, ref string, holderType ledgerv1.HolderType) *ledgerv1.Holder {
	t.Helper()
	req := connect.NewRequest(&ledgerv1.CreateHolderRequest{Type: holderType, ExternalRef: ref})
	req.Header().Set("X-Api-Key", testAPIKey)
	holder, err := env.provision.CreateHolder(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateHolder(%s): %v", ref, err)
	}
	return holder.Msg
}

// post sends CreateTransaction with one entry per account/amount pair.
func post(t *testing.T, env *env, key, reference string, entries ...*ledgerv1.TransactionInputEntry) (*ledgerv1.Transaction, error) {
	t.Helper()
	req := connect.NewRequest(&ledgerv1.CreateTransactionRequest{
		IdempotencyKey: key,
		Reference:      stringPtr(reference),
		Entries:        entries,
	})
	req.Header().Set("X-Api-Key", testAPIKey)
	resp, err := env.posting.CreateTransaction(context.Background(), req)
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func entry(accountID, amount string) *ledgerv1.TransactionInputEntry {
	return &ledgerv1.TransactionInputEntry{AccountId: accountID, Amount: amount}
}

func stringPtr(s string) *string { return &s }

// fxBalance reads the materialized posted balance of one account through
// the API, searching the wallets the fixture created.
func fxBalance(t *testing.T, env *env, fx postingFixture, accountID string) decimal.Decimal {
	t.Helper()
	for _, walletID := range fx.wallets {
		req := connect.NewRequest(&ledgerv1.GetWalletBalancesRequest{WalletId: walletID})
		req.Header().Set("X-Api-Key", testAPIKey)
		resp, err := env.provision.GetWalletBalances(context.Background(), req)
		if err != nil {
			t.Fatalf("GetWalletBalances(%s): %v", walletID, err)
		}
		for _, b := range resp.Msg.GetBalances() {
			if b.GetAccount().GetId() == accountID {
				got, err := decimal.NewFromString(b.GetPosted())
				if err != nil {
					t.Fatalf("balance %q is not a decimal: %v", b.GetPosted(), err)
				}
				return got
			}
		}
	}
	t.Fatalf("account %s not found in fixture wallets", accountID)
	return decimal.Zero
}

// assertBalance fails unless the account's posted balance equals want
// (compared numerically — Postgres normalizes NUMERIC display scale).
func assertBalance(t *testing.T, env *env, fx postingFixture, accountID, want string) {
	t.Helper()
	got := fxBalance(t, env, fx, accountID)
	wd, err := decimal.NewFromString(want)
	if err != nil {
		t.Fatalf("bad want amount %q: %v", want, err)
	}
	if !got.Equal(wd) {
		t.Fatalf("posted balance of %s = %s, want %s", accountID, got, want)
	}
}

// TestFundingFromSystemAccount: value enters via a system account that may
// go negative; the user account is funded exactly.
func TestFundingFromSystemAccount(t *testing.T) {
	env := setup(t)
	fx := newPostingFixture(t, env)

	tx, err := post(t, env, "fund-1", "stripe-charge-1",
		entry(fx.system.GetId(), "-100.00"),
		entry(fx.user.GetId(), "100.00"),
	)
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	if tx.GetId() == "" || tx.GetIdempotencyKey() != "fund-1" {
		t.Fatalf("unexpected transaction: %v", tx)
	}
	if tx.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_POSTED {
		t.Fatalf("status = %v, want POSTED", tx.GetStatus())
	}
	if tx.GetPostedAt() == nil {
		t.Fatal("posted transaction missing posted_at")
	}
	if tx.Reference == nil || tx.GetReference() != "stripe-charge-1" {
		t.Fatalf("reference not round-tripped: %v", tx.GetReference())
	}
	if len(tx.GetEntries()) != 2 {
		t.Fatalf("got %d entries, want 2", len(tx.GetEntries()))
	}

	assertBalance(t, env, fx, fx.user.GetId(), "100.00")
	assertBalance(t, env, fx, fx.system.GetId(), "-100.00")
}

// TestAccountToAccountTransfer: funding then transferring between two user
// accounts moves balances exactly, per entry.
func TestAccountToAccountTransfer(t *testing.T) {
	env := setup(t)
	fx := newPostingFixture(t, env)

	if _, err := post(t, env, "fund", "",
		entry(fx.system.GetId(), "-50.00"),
		entry(fx.user.GetId(), "50.00"),
	); err != nil {
		t.Fatalf("fund: %v", err)
	}

	secondHolder := createHolder(t, env, "user-2")
	secondWallet := createWallet(t, env, secondHolder.GetId(), "main")
	second := createAccount(t, env, secondWallet.GetId(), fx.asset.GetId(), false)
	fx.wallets = append(fx.wallets, secondWallet.GetId())

	if _, err := post(t, env, "transfer-1", "",
		entry(fx.user.GetId(), "-20.00"),
		entry(second.GetId(), "20.00"),
	); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	assertBalance(t, env, fx, fx.user.GetId(), "30.00")
	assertBalance(t, env, fx, second.GetId(), "20.00")
	assertBalance(t, env, fx, fx.system.GetId(), "-50.00")
}

// TestNonZeroSumRejectedWithNothingWritten: a transaction whose entries do
// not net to zero is rejected and leaves no trace — no transaction, no
// entries, no balance movement.
func TestNonZeroSumRejectedWithNothingWritten(t *testing.T) {
	env := setup(t)
	fx := newPostingFixture(t, env)

	if _, err := post(t, env, "bad-sum", "",
		entry(fx.system.GetId(), "-100.00"),
		entry(fx.user.GetId(), "99.99"),
	); err == nil {
		t.Fatal("non-zero-sum transaction: expected error")
	} else {
		assertCode(t, err, connect.CodeInvalidArgument)
	}

	assertBalance(t, env, fx, fx.user.GetId(), "0")
	assertBalance(t, env, fx, fx.system.GetId(), "0")

	// The idempotency key was not consumed: a corrected retry succeeds.
	if _, err := post(t, env, "bad-sum", "",
		entry(fx.system.GetId(), "-100.00"),
		entry(fx.user.GetId(), "100.00"),
	); err != nil {
		t.Fatalf("corrected retry: %v", err)
	}
}

// TestOverdraftRejectedUnlessAllowed: drawing more than the posted balance
// fails on a regular account, and the identical movement succeeds when the
// debited account is flagged allow_negative.
func TestOverdraftRejectedUnlessAllowed(t *testing.T) {
	env := setup(t)
	fx := newPostingFixture(t, env)

	// Fund the user with 10.00.
	if _, err := post(t, env, "fund-10", "",
		entry(fx.system.GetId(), "-10.00"),
		entry(fx.user.GetId(), "10.00"),
	); err != nil {
		t.Fatalf("fund: %v", err)
	}

	// Overdraft attempt: user sends 15.00 to a second user account.
	secondHolder := createHolder(t, env, "user-od")
	secondWallet := createWallet(t, env, secondHolder.GetId(), "main")
	second := createAccount(t, env, secondWallet.GetId(), fx.asset.GetId(), false)
	fx.wallets = append(fx.wallets, secondWallet.GetId())

	_, err := post(t, env, "od-attempt", "",
		entry(fx.user.GetId(), "-15.00"),
		entry(second.GetId(), "15.00"),
	)
	assertCode(t, err, connect.CodeFailedPrecondition)
	assertBalance(t, env, fx, fx.user.GetId(), "10.00")
	assertBalance(t, env, fx, second.GetId(), "0")

	// The same shapes of movement against the system account (which is
	// allow_negative) succeed: debit the system by 15, fund the receiver.
	if _, err := post(t, env, "od-allowed", "",
		entry(fx.system.GetId(), "-15.00"),
		entry(second.GetId(), "15.00"),
	); err != nil {
		t.Fatalf("allow_negative overdraft: %v", err)
	}
	assertBalance(t, env, fx, second.GetId(), "15.00")
	assertBalance(t, env, fx, fx.system.GetId(), "-25.00")
}

// TestIdempotentReplayReturnsOriginalWithoutMovement: retrying a write
// with the same idempotency key returns the original transaction and moves
// nothing.
func TestIdempotentReplayReturnsOriginalWithoutMovement(t *testing.T) {
	env := setup(t)
	fx := newPostingFixture(t, env)

	original, err := post(t, env, "idem-1", "provider-ref",
		entry(fx.system.GetId(), "-40.00"),
		entry(fx.user.GetId(), "40.00"),
	)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}

	replay, err := post(t, env, "idem-1", "provider-ref",
		entry(fx.system.GetId(), "-40.00"),
		entry(fx.user.GetId(), "40.00"),
	)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.GetId() != original.GetId() {
		t.Fatalf("replay returned id %s, want original %s", replay.GetId(), original.GetId())
	}
	if len(replay.GetEntries()) != 2 {
		t.Fatalf("replay entries = %d, want 2", len(replay.GetEntries()))
	}

	assertBalance(t, env, fx, fx.user.GetId(), "40.00")
	assertBalance(t, env, fx, fx.system.GetId(), "-40.00")

	// GetTransaction returns the same transaction with its entries.
	got, err := getTransaction(t, env, original.GetId())
	if err != nil {
		t.Fatalf("GetTransaction: %v", err)
	}
	if got.GetIdempotencyKey() != "idem-1" || got.GetReference() != "provider-ref" {
		t.Fatalf("GetTransaction round-trip mismatch: %v", got)
	}
	if len(got.GetEntries()) != 2 {
		t.Fatalf("GetTransaction entries = %d, want 2", len(got.GetEntries()))
	}
}

// TestConcurrentPostingsPreserveExactBalance: parallel writers posting
// disjoint transactions into the same two accounts must leave the
// materialized balance exactly equal to the arithmetic sum — the balance
// locks make every read-modify-write serial. Run with -race locally.
func TestConcurrentPostingsPreserveExactBalance(t *testing.T) {
	env := setup(t)
	fx := newPostingFixture(t, env)

	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			amount := fmt.Sprintf("%d.%02d", i+1, 0)
			_, err := post(t, env, fmt.Sprintf("conc-%d", i), "",
				entry(fx.system.GetId(), "-"+amount),
				entry(fx.user.GetId(), amount),
			)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent post: %v", err)
		}
	}

	// Sum of 1..8 = 36.
	assertBalance(t, env, fx, fx.user.GetId(), "36.00")
	assertBalance(t, env, fx, fx.system.GetId(), "-36.00")
}

// TestPostingValidationErrors covers the remaining request-shape rules:
// missing idempotency key, too few entries, zero amounts, malformed
// amounts, unknown accounts, and precision violations.
func TestPostingValidationErrors(t *testing.T) {
	env := setup(t)
	fx := newPostingFixture(t, env)

	t.Run("missing idempotency key", func(t *testing.T) {
		req := connect.NewRequest(&ledgerv1.CreateTransactionRequest{
			Entries: []*ledgerv1.TransactionInputEntry{
				entry(fx.system.GetId(), "-1.00"),
				entry(fx.user.GetId(), "1.00"),
			},
		})
		req.Header().Set("X-Api-Key", testAPIKey)
		_, err := env.posting.CreateTransaction(context.Background(), req)
		assertCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("single entry", func(t *testing.T) {
		_, err := post(t, env, "one-entry", "", entry(fx.user.GetId(), "1.00"))
		assertCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("zero amount", func(t *testing.T) {
		_, err := post(t, env, "zero-amt", "",
			entry(fx.system.GetId(), "0.00"),
			entry(fx.user.GetId(), "1.00"),
		)
		assertCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("malformed amount", func(t *testing.T) {
		_, err := post(t, env, "bad-amt", "",
			entry(fx.system.GetId(), "abc"),
			entry(fx.user.GetId(), "1.00"),
		)
		assertCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("unknown account", func(t *testing.T) {
		_, err := post(t, env, "unknown-acct", "",
			entry("acct-does-not-exist", "-1.00"),
			entry(fx.user.GetId(), "1.00"),
		)
		assertCode(t, err, connect.CodeNotFound)
	})

	t.Run("precision violation", func(t *testing.T) {
		// USD has precision 2; three fractional digits are rejected.
		_, err := post(t, env, "precision", "",
			entry(fx.system.GetId(), "-1.001"),
			entry(fx.user.GetId(), "1.001"),
		)
		assertCode(t, err, connect.CodeInvalidArgument)
		assertBalance(t, env, fx, fx.user.GetId(), "0")
	})
}

func TestGetTransactionUnknownID(t *testing.T) {
	env := setup(t)

	req := connect.NewRequest(&ledgerv1.GetTransactionRequest{Id: "018f4d9a-7b6c-7000-8000-000000000000"})
	req.Header().Set("X-Api-Key", testAPIKey)
	_, err := env.posting.GetTransaction(context.Background(), req)
	assertCode(t, err, connect.CodeNotFound)
}

func getTransaction(t *testing.T, env *env, id string) (*ledgerv1.Transaction, error) {
	t.Helper()
	req := connect.NewRequest(&ledgerv1.GetTransactionRequest{Id: id})
	req.Header().Set("X-Api-Key", testAPIKey)
	resp, err := env.posting.GetTransaction(context.Background(), req)
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}
