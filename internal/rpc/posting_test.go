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
	asset := createAsset(t, env, "USD", 2)

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

// fxAccountBalance reads the materialized posted, pending, and available
// balances of one account through the API, searching the wallets the
// fixture created.
func fxAccountBalance(t *testing.T, env *env, fx postingFixture, accountID string) *ledgerv1.AccountBalance {
	t.Helper()
	for _, walletID := range fx.wallets {
		resp, err := env.provision.GetWalletBalances(context.Background(), authed(t,
			connect.NewRequest(&ledgerv1.GetWalletBalancesRequest{WalletId: walletID})))
		if err != nil {
			t.Fatalf("GetWalletBalances(%s): %v", walletID, err)
		}
		for _, b := range resp.Msg.GetBalances() {
			if b.GetAccount().GetId() == accountID {
				return b
			}
		}
	}
	t.Fatalf("account %s not found in fixture wallets", accountID)
	return nil
}

// assertBalance fails unless the account's posted balance equals want
// (compared numerically — Postgres normalizes NUMERIC display scale).
func assertBalance(t *testing.T, env *env, fx postingFixture, accountID, want string) {
	t.Helper()
	assertAmount(t, "posted", accountID, fxAccountBalance(t, env, fx, accountID).GetPosted(), want)
}

// assertBalances fails unless the account's posted, pending, and
// available balances equal the wants (compared numerically).
func assertBalances(t *testing.T, env *env, fx postingFixture, accountID, posted, pending, available string) {
	t.Helper()
	b := fxAccountBalance(t, env, fx, accountID)
	assertAmount(t, "posted", accountID, b.GetPosted(), posted)
	assertAmount(t, "pending", accountID, b.GetPending(), pending)
	assertAmount(t, "available", accountID, b.GetAvailable(), available)
}

func assertAmount(t *testing.T, column, accountID, got, want string) {
	t.Helper()
	gd, err := decimal.NewFromString(got)
	if err != nil {
		t.Fatalf("%s balance of %s = %q, not a decimal: %v", column, accountID, got, err)
	}
	wd, err := decimal.NewFromString(want)
	if err != nil {
		t.Fatalf("bad want amount %q: %v", want, err)
	}
	if !gd.Equal(wd) {
		t.Fatalf("%s balance of %s = %s, want %s", column, accountID, got, want)
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

	_, err := post(t, env, "bad-sum", "",
		entry(fx.system.GetId(), "-100.00"),
		entry(fx.user.GetId(), "99.99"),
	)
	assertCode(t, err, connect.CodeInvalidArgument)

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
		_, err := env.posting.CreateTransaction(context.Background(), authed(t,
			connect.NewRequest(&ledgerv1.CreateTransactionRequest{
				Entries: []*ledgerv1.TransactionInputEntry{
					entry(fx.system.GetId(), "-1.00"),
					entry(fx.user.GetId(), "1.00"),
				},
			})))
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

	_, err := getTransaction(t, env, "018f4d9a-7b6c-7000-8000-000000000000")
	assertCode(t, err, connect.CodeNotFound)
}

// TestPendingCreationEarmarksOnly: creating a pending transaction moves
// the pending column only; posted balances are untouched and available
// (posted + pending) reflects the earmark.
func TestPendingCreationEarmarksOnly(t *testing.T) {
	env := setup(t)
	fx := newPostingFixture(t, env)

	if _, err := post(t, env, "fund", "",
		entry(fx.system.GetId(), "-100.00"),
		entry(fx.user.GetId(), "100.00"),
	); err != nil {
		t.Fatalf("fund: %v", err)
	}

	pending, err := createPending(t, env, "hold-1", "auth-1",
		entry(fx.user.GetId(), "-30.00"),
		entry(fx.system.GetId(), "30.00"),
	)
	if err != nil {
		t.Fatalf("CreatePendingTransaction: %v", err)
	}
	if pending.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_PENDING {
		t.Fatalf("status = %v, want PENDING", pending.GetStatus())
	}
	if pending.GetPostedAt() != nil {
		t.Fatal("pending transaction must not carry posted_at")
	}
	if pending.GetVoidedAt() != nil {
		t.Fatal("pending transaction must not carry voided_at")
	}
	if len(pending.GetEntries()) != 2 {
		t.Fatalf("got %d entries, want 2", len(pending.GetEntries()))
	}

	// The earmark moved pending only; available excludes the outgoing hold.
	assertBalances(t, env, fx, fx.user.GetId(), "100.00", "-30.00", "70.00")
	assertBalances(t, env, fx, fx.system.GetId(), "-100.00", "30.00", "-70.00")

	got, err := getTransaction(t, env, pending.GetId())
	if err != nil {
		t.Fatalf("GetTransaction: %v", err)
	}
	if got.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_PENDING {
		t.Fatalf("GetTransaction status = %v, want PENDING", got.GetStatus())
	}
}

// TestPostPendingSettles: posting a pending transaction moves the
// earmarked amounts from pending to posted exactly and stamps posted_at.
func TestPostPendingSettles(t *testing.T) {
	env := setup(t)
	fx := newPostingFixture(t, env)

	if _, err := post(t, env, "fund", "",
		entry(fx.system.GetId(), "-100.00"),
		entry(fx.user.GetId(), "100.00"),
	); err != nil {
		t.Fatalf("fund: %v", err)
	}
	pending, err := createPending(t, env, "hold-2", "",
		entry(fx.user.GetId(), "-40.00"),
		entry(fx.system.GetId(), "40.00"),
	)
	if err != nil {
		t.Fatalf("CreatePendingTransaction: %v", err)
	}

	settled, err := postPending(t, env, pending.GetId())
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	if settled.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_POSTED {
		t.Fatalf("status = %v, want POSTED", settled.GetStatus())
	}
	if settled.GetPostedAt() == nil {
		t.Fatal("settled transaction missing posted_at")
	}
	if settled.GetVoidedAt() != nil {
		t.Fatal("settled transaction must not carry voided_at")
	}

	assertBalances(t, env, fx, fx.user.GetId(), "60.00", "0", "60.00")
	assertBalances(t, env, fx, fx.system.GetId(), "-60.00", "0", "-60.00")

	got, err := getTransaction(t, env, pending.GetId())
	if err != nil {
		t.Fatalf("GetTransaction: %v", err)
	}
	if got.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_POSTED || got.GetPostedAt() == nil {
		t.Fatalf("GetTransaction did not reflect settle: %v", got)
	}
}

// TestVoidPendingReleases: voiding a pending transaction removes the
// earmark with no posted movement and stamps voided_at.
func TestVoidPendingReleases(t *testing.T) {
	env := setup(t)
	fx := newPostingFixture(t, env)

	if _, err := post(t, env, "fund", "",
		entry(fx.system.GetId(), "-100.00"),
		entry(fx.user.GetId(), "100.00"),
	); err != nil {
		t.Fatalf("fund: %v", err)
	}
	pending, err := createPending(t, env, "hold-3", "",
		entry(fx.user.GetId(), "-25.00"),
		entry(fx.system.GetId(), "25.00"),
	)
	if err != nil {
		t.Fatalf("CreatePendingTransaction: %v", err)
	}

	voided, err := voidPending(t, env, pending.GetId())
	if err != nil {
		t.Fatalf("VoidTransaction: %v", err)
	}
	if voided.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_VOIDED {
		t.Fatalf("status = %v, want VOIDED", voided.GetStatus())
	}
	if voided.GetVoidedAt() == nil {
		t.Fatal("voided transaction missing voided_at")
	}
	if voided.GetPostedAt() != nil {
		t.Fatal("voided transaction must not carry posted_at")
	}

	// Posted never moved; the earmark is gone, so available is restored.
	assertBalances(t, env, fx, fx.user.GetId(), "100.00", "0", "100.00")
	assertBalances(t, env, fx, fx.system.GetId(), "-100.00", "0", "-100.00")
}

// TestPendingOverdrawAvailableRejected: a pending creation that would
// drive available balance negative is rejected even when the posted
// balance alone would cover the movement. The rejected key is not
// consumed: a corrected retry with the same key succeeds.
func TestPendingOverdrawAvailableRejected(t *testing.T) {
	env := setup(t)
	fx := newPostingFixture(t, env)

	if _, err := post(t, env, "fund", "",
		entry(fx.system.GetId(), "-100.00"),
		entry(fx.user.GetId(), "100.00"),
	); err != nil {
		t.Fatalf("fund: %v", err)
	}

	// Earmark most of the balance: available is now 40.
	if _, err := createPending(t, env, "hold-part", "",
		entry(fx.user.GetId(), "-60.00"),
		entry(fx.system.GetId(), "60.00"),
	); err != nil {
		t.Fatalf("partial hold: %v", err)
	}

	// Posted alone (100) would allow a 50.00 movement; available (40) does
	// not.
	_, err := createPending(t, env, "hold-over", "",
		entry(fx.user.GetId(), "-50.00"),
		entry(fx.system.GetId(), "50.00"),
	)
	assertCode(t, err, connect.CodeFailedPrecondition)
	assertBalances(t, env, fx, fx.user.GetId(), "100.00", "-60.00", "40.00")

	// The rejected key was not consumed: a smaller retry succeeds.
	if _, err := createPending(t, env, "hold-over", "",
		entry(fx.user.GetId(), "-40.00"),
		entry(fx.system.GetId(), "40.00"),
	); err != nil {
		t.Fatalf("corrected retry on rejected key: %v", err)
	}
	assertBalances(t, env, fx, fx.user.GetId(), "100.00", "-100.00", "0")

	// A hold that exceeds the posted balance outright is rejected too.
	_, err = createPending(t, env, "hold-too-big", "",
		entry(fx.user.GetId(), "-200.00"),
		entry(fx.system.GetId(), "200.00"),
	)
	assertCode(t, err, connect.CodeFailedPrecondition)
	assertBalances(t, env, fx, fx.user.GetId(), "100.00", "-100.00", "0")
}

// TestTerminalStatesFinal: repeating a transition that already reached
// its terminal state is a safe retry — it returns the transaction and
// moves nothing — while the opposite transition on a terminal transaction
// is rejected with failed_precondition.
func TestTerminalStatesFinal(t *testing.T) {
	env := setup(t)
	fx := newPostingFixture(t, env)

	if _, err := post(t, env, "fund", "",
		entry(fx.system.GetId(), "-100.00"),
		entry(fx.user.GetId(), "100.00"),
	); err != nil {
		t.Fatalf("fund: %v", err)
	}

	// Settle path: post, then re-posting is a safe retry and voiding is
	// rejected.
	pending, err := createPending(t, env, "hold-a", "",
		entry(fx.user.GetId(), "-30.00"),
		entry(fx.system.GetId(), "30.00"),
	)
	if err != nil {
		t.Fatalf("CreatePendingTransaction: %v", err)
	}
	settled, err := postPending(t, env, pending.GetId())
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}

	replay, err := postPending(t, env, pending.GetId())
	if err != nil {
		t.Fatalf("re-posting a posted transaction: expected safe retry, got %v", err)
	}
	if replay.GetId() != settled.GetId() || replay.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_POSTED {
		t.Fatalf("re-post returned id %s status %v, want original %s posted", replay.GetId(), replay.GetStatus(), settled.GetId())
	}
	if _, err := voidPending(t, env, pending.GetId()); err == nil {
		t.Fatal("voiding a posted transaction: expected error")
	} else {
		assertCode(t, err, connect.CodeFailedPrecondition)
	}

	got, err := getTransaction(t, env, pending.GetId())
	if err != nil {
		t.Fatalf("GetTransaction: %v", err)
	}
	if got.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_POSTED {
		t.Fatalf("status after replay and rejected void = %v, want POSTED", got.GetStatus())
	}
	if !got.GetPostedAt().AsTime().Equal(settled.GetPostedAt().AsTime()) {
		t.Fatal("posted_at changed after re-post")
	}
	assertBalances(t, env, fx, fx.user.GetId(), "70.00", "0", "70.00")

	// Void path: void, then re-voiding is a safe retry and posting is
	// rejected.
	released, err := createPending(t, env, "hold-b", "",
		entry(fx.user.GetId(), "-20.00"),
		entry(fx.system.GetId(), "20.00"),
	)
	if err != nil {
		t.Fatalf("CreatePendingTransaction: %v", err)
	}
	voided, err := voidPending(t, env, released.GetId())
	if err != nil {
		t.Fatalf("VoidTransaction: %v", err)
	}

	revoid, err := voidPending(t, env, released.GetId())
	if err != nil {
		t.Fatalf("re-voiding a voided transaction: expected safe retry, got %v", err)
	}
	if revoid.GetId() != voided.GetId() || revoid.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_VOIDED {
		t.Fatalf("re-void returned id %s status %v, want original %s voided", revoid.GetId(), revoid.GetStatus(), voided.GetId())
	}
	if _, err := postPending(t, env, released.GetId()); err == nil {
		t.Fatal("posting a voided transaction: expected error")
	} else {
		assertCode(t, err, connect.CodeFailedPrecondition)
	}

	got, err = getTransaction(t, env, released.GetId())
	if err != nil {
		t.Fatalf("GetTransaction: %v", err)
	}
	if got.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_VOIDED {
		t.Fatalf("status after replay and rejected post = %v, want VOIDED", got.GetStatus())
	}
	if !got.GetVoidedAt().AsTime().Equal(voided.GetVoidedAt().AsTime()) {
		t.Fatal("voided_at changed after re-void")
	}
	assertBalances(t, env, fx, fx.user.GetId(), "70.00", "0", "70.00")
	assertBalances(t, env, fx, fx.system.GetId(), "-70.00", "0", "-70.00")

	// Unknown transactions are not_found on both transitions.
	_, err = postPending(t, env, "018f4d9a-7b6c-7000-8000-000000000000")
	assertCode(t, err, connect.CodeNotFound)
	_, err = voidPending(t, env, "018f4d9a-7b6c-7000-8000-000000000000")
	assertCode(t, err, connect.CodeNotFound)
}

// TestIdempotentReplayPendingCreate: retrying a pending creation with the
// same idempotency key returns the original transaction and earmarks
// nothing twice.
func TestIdempotentReplayPendingCreate(t *testing.T) {
	env := setup(t)
	fx := newPostingFixture(t, env)

	if _, err := post(t, env, "fund", "",
		entry(fx.system.GetId(), "-100.00"),
		entry(fx.user.GetId(), "100.00"),
	); err != nil {
		t.Fatalf("fund: %v", err)
	}

	original, err := createPending(t, env, "idem-p", "auth-replay",
		entry(fx.user.GetId(), "-20.00"),
		entry(fx.system.GetId(), "20.00"),
	)
	if err != nil {
		t.Fatalf("first pending create: %v", err)
	}

	replay, err := createPending(t, env, "idem-p", "auth-replay",
		entry(fx.user.GetId(), "-20.00"),
		entry(fx.system.GetId(), "20.00"),
	)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.GetId() != original.GetId() {
		t.Fatalf("replay returned id %s, want original %s", replay.GetId(), original.GetId())
	}
	if replay.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_PENDING {
		t.Fatalf("replay status = %v, want PENDING", replay.GetStatus())
	}
	if len(replay.GetEntries()) != 2 {
		t.Fatalf("replay entries = %d, want 2", len(replay.GetEntries()))
	}

	// A single earmark, not two.
	assertBalances(t, env, fx, fx.user.GetId(), "100.00", "-20.00", "80.00")
	assertBalances(t, env, fx, fx.system.GetId(), "-100.00", "20.00", "-80.00")

	// The replayed hold still settles exactly once.
	if _, err := postPending(t, env, original.GetId()); err != nil {
		t.Fatalf("post after replay: %v", err)
	}
	assertBalances(t, env, fx, fx.user.GetId(), "80.00", "0", "80.00")
}
