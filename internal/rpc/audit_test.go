package rpc_test

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"github.com/shopspring/decimal"

	ledgerv1 "github.com/use-fabrica/ledger/proto/ledger/v1"
)

// fundedWallet is the standard audit fixture: one asset, a system account
// flagged allow_negative, and one user wallet/account funded with 100.00.
type fundedWallet struct {
	systemAccount *ledgerv1.Account
	userWallet    *ledgerv1.Wallet
	userAccount   *ledgerv1.Account
}

func newFundedWallet(t *testing.T, env *env) fundedWallet {
	t.Helper()
	asset := createAsset(t, env, "USD", 2)

	system := createHolderWithType(t, env, "audit-system", ledgerv1.HolderType_HOLDER_TYPE_SYSTEM)
	systemWallet := createWallet(t, env, system.GetId(), "system ops")
	systemAccount := createAccount(t, env, systemWallet.GetId(), asset.GetId(), true)

	user := createHolder(t, env, "audit-user")
	userWallet := createWallet(t, env, user.GetId(), "user wallet")
	userAccount := createAccount(t, env, userWallet.GetId(), asset.GetId(), false)

	mustPost(t, env, "audit-fund-1", "stripe-charge-fund",
		entry(systemAccount.GetId(), "-100.00"),
		entry(userAccount.GetId(), "100.00"))

	return fundedWallet{
		systemAccount: systemAccount,
		userWallet:    userWallet,
		userAccount:   userAccount,
	}
}

// listWalletTransactions lists one wallet's transactions, optionally
// filtered by status (nil = every status).
func listWalletTransactions(t *testing.T, env *env, walletID string, status *ledgerv1.TransactionStatus) *ledgerv1.ListWalletTransactionsResponse {
	t.Helper()
	req := connect.NewRequest(&ledgerv1.ListWalletTransactionsRequest{WalletId: walletID})
	if status != nil {
		req.Msg.Status = status
	}
	res, err := env.audit.ListWalletTransactions(context.Background(), authed(t, req))
	if err != nil {
		t.Fatalf("ListWalletTransactions(%s): %v", walletID, err)
	}
	return res.Msg
}

// TestListEntriesChronologicalWithRunningTotals: the entry history of an
// account lists every movement in order, and accumulating the signed
// amounts reconstructs the posted balance — the audit trail and the
// materialized balance agree by construction.
func TestListEntriesChronologicalWithRunningTotals(t *testing.T) {
	env := setup(t)
	fx := newFundedWallet(t, env)

	mustPost(t, env, "audit-pay-1", "stripe-charge-1",
		entry(fx.userAccount.GetId(), "-30.00"),
		entry(fx.systemAccount.GetId(), "30.00"))
	mustPost(t, env, "audit-pay-2", "stripe-charge-2",
		entry(fx.userAccount.GetId(), "-12.50"),
		entry(fx.systemAccount.GetId(), "12.50"))

	res, err := env.audit.ListEntries(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.ListEntriesRequest{AccountId: fx.userAccount.GetId()})))
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	entries := res.Msg.GetEntries()
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if res.Msg.NextPageOffset != nil {
		t.Fatalf("single page expected, got next_page_offset=%d", *res.Msg.NextPageOffset)
	}

	// Chronological order with running totals derivable: the funding
	// credit lands first, then the two debits.
	wantAmounts := []string{"100.00", "-30.00", "-12.50"}
	wantTotals := []string{"100.00", "70.00", "57.50"}
	running := decimal.Zero
	for i, entry := range entries {
		amount, err := decimal.NewFromString(entry.GetAmount())
		if err != nil {
			t.Fatalf("entry %d amount %q is not decimal: %v", i, entry.GetAmount(), err)
		}
		want, err := decimal.NewFromString(wantAmounts[i])
		if err != nil || !amount.Equal(want) {
			t.Fatalf("entry %d: amount = %s, want %s", i, entry.GetAmount(), wantAmounts[i])
		}
		if entry.GetAccountId() != fx.userAccount.GetId() {
			t.Fatalf("entry %d: account_id = %s", i, entry.GetAccountId())
		}
		running = running.Add(amount)
		total, _ := decimal.NewFromString(wantTotals[i])
		if !running.Equal(total) {
			t.Fatalf("entry %d: running total = %s, want %s", i, running, wantTotals[i])
		}
	}

	// The accumulated total must equal the materialized posted balance.
	balances, err := env.provision.GetWalletBalances(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.GetWalletBalancesRequest{WalletId: fx.userWallet.GetId()})))
	if err != nil {
		t.Fatalf("GetWalletBalances: %v", err)
	}
	if len(balances.Msg.GetBalances()) != 1 {
		t.Fatalf("expected 1 balance, got %d", len(balances.Msg.GetBalances()))
	}
	posted, err := decimal.NewFromString(balances.Msg.GetBalances()[0].GetPosted())
	if err != nil || !posted.Equal(running) {
		t.Fatalf("posted balance = %s, running total = %s", balances.Msg.GetBalances()[0].GetPosted(), running)
	}
}

// TestListWalletTransactionsReturnsAllTouchingTransactions: a wallet's
// transaction history contains every transaction that touches any of its
// accounts — including a multi-asset transaction that hits two accounts of
// the same wallet, which must appear exactly once.
func TestListWalletTransactionsReturnsAllTouchingTransactions(t *testing.T) {
	env := setup(t)
	fx := newFundedWallet(t, env) // funds user account with 100.00

	// A second user wallet with an account, never party to any
	// transaction.
	other := createHolder(t, env, "audit-other")
	otherWallet := createWallet(t, env, other.GetId(), "other wallet")
	otherAsset := createAsset(t, env, "EUR", 2)
	createAccount(t, env, otherWallet.GetId(), otherAsset.GetId(), false)

	// Moves value out of the user wallet to the other wallet...
	mustPost(t, env, "audit-xfer-out", "stripe-charge-out",
		entry(fx.userAccount.GetId(), "-30.00"),
		entry(fx.systemAccount.GetId(), "30.00"))
	// ...and a multi-asset transaction touching TWO accounts of the user
	// wallet (USD debit + EUR credit through the system accounts). The
	// user wallet must list it once, not twice.
	userEUR := createAccount(t, env, fx.userWallet.GetId(), otherAsset.GetId(), false)
	systemEUR := createAccount(t, env, createWallet(t, env,
		createHolderWithType(t, env, "audit-system-2", ledgerv1.HolderType_HOLDER_TYPE_SYSTEM).GetId(),
		"system ops 2").GetId(), otherAsset.GetId(), true)
	mustPost(t, env, "audit-multi-asset", "stripe-charge-multi",
		entry(fx.userAccount.GetId(), "-10.00"),
		entry(fx.systemAccount.GetId(), "10.00"),
		entry(systemEUR.GetId(), "-20.00"),
		entry(userEUR.GetId(), "20.00"))

	transactions := listWalletTransactions(t, env, fx.userWallet.GetId(), nil).GetTransactions()

	// Expected: the funding credit, the debit, and the multi-asset
	// transaction — exactly 3, with the multi-asset one deduplicated.
	if len(transactions) != 3 {
		t.Fatalf("expected 3 transactions, got %d", len(transactions))
	}
	keys := make(map[string]int, len(transactions))
	for _, transaction := range transactions {
		keys[transaction.GetIdempotencyKey()]++
		if transaction.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_POSTED {
			t.Fatalf("transaction %s: status = %s, want posted", transaction.GetIdempotencyKey(), transaction.GetStatus())
		}
		if len(transaction.GetEntries()) == 0 {
			t.Fatalf("transaction %s: no entries in list response", transaction.GetIdempotencyKey())
		}
	}
	for _, want := range []string{"audit-fund-1", "audit-xfer-out", "audit-multi-asset"} {
		if keys[want] != 1 {
			t.Fatalf("transaction %s appears %d times, want exactly 1", want, keys[want])
		}
	}

	// Chronological order: funding precedes the later movements.
	if transactions[0].GetIdempotencyKey() != "audit-fund-1" {
		t.Fatalf("first transaction = %s, want audit-fund-1", transactions[0].GetIdempotencyKey())
	}

	// The other wallet's history contains none of the user wallet's
	// transactions: it was never party to any.
	if got := listWalletTransactions(t, env, otherWallet.GetId(), nil).GetTransactions(); len(got) != 0 {
		t.Fatalf("other wallet: expected 0 transactions, got %d", len(got))
	}
}

// TestListWalletTransactionsStatusFilter: without a status the listing
// returns transactions of every status — posted, pending, and voided —
// and with one it returns exactly that status, so reconciliation can see
// the complete journal or zoom into one slice of it.
func TestListWalletTransactionsStatusFilter(t *testing.T) {
	env := setup(t)
	fx := newFundedWallet(t, env) // one posted transaction (audit-fund-1)

	pending, err := createPending(t, env, "audit-hold-1", "auth-1",
		entry(fx.userAccount.GetId(), "-10.00"),
		entry(fx.systemAccount.GetId(), "10.00"))
	if err != nil {
		t.Fatalf("CreatePendingTransaction: %v", err)
	}
	if pending.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_PENDING {
		t.Fatalf("status = %v, want PENDING", pending.GetStatus())
	}

	voidSource, err := createPending(t, env, "audit-hold-2", "auth-2",
		entry(fx.userAccount.GetId(), "-5.00"),
		entry(fx.systemAccount.GetId(), "5.00"))
	if err != nil {
		t.Fatalf("CreatePendingTransaction: %v", err)
	}
	voided, err := voidPending(t, env, voidSource.GetId())
	if err != nil {
		t.Fatalf("VoidTransaction: %v", err)
	}
	if voided.GetStatus() != ledgerv1.TransactionStatus_TRANSACTION_STATUS_VOIDED {
		t.Fatalf("status = %v, want VOIDED", voided.GetStatus())
	}

	postedStatus := ledgerv1.TransactionStatus_TRANSACTION_STATUS_POSTED
	pendingStatus := ledgerv1.TransactionStatus_TRANSACTION_STATUS_PENDING
	voidedStatus := ledgerv1.TransactionStatus_TRANSACTION_STATUS_VOIDED

	// Unset lists every status.
	all := listWalletTransactions(t, env, fx.userWallet.GetId(), nil).GetTransactions()
	if len(all) != 3 {
		t.Fatalf("unfiltered listing: got %d transactions, want 3", len(all))
	}
	seen := make(map[ledgerv1.TransactionStatus]int, len(all))
	for _, transaction := range all {
		seen[transaction.GetStatus()]++
	}
	for _, want := range []ledgerv1.TransactionStatus{postedStatus, pendingStatus, voidedStatus} {
		if seen[want] != 1 {
			t.Fatalf("status %s appears %d times in unfiltered listing, want 1", want, seen[want])
		}
	}

	// Each exact filter returns only its slice.
	posted := listWalletTransactions(t, env, fx.userWallet.GetId(), &postedStatus).GetTransactions()
	if len(posted) != 1 || posted[0].GetIdempotencyKey() != "audit-fund-1" {
		t.Fatalf("posted filter: got %v, want only audit-fund-1", keysOf(posted))
	}
	pendings := listWalletTransactions(t, env, fx.userWallet.GetId(), &pendingStatus).GetTransactions()
	if len(pendings) != 1 || pendings[0].GetIdempotencyKey() != "audit-hold-1" {
		t.Fatalf("pending filter: got %v, want only audit-hold-1", keysOf(pendings))
	}
	voideds := listWalletTransactions(t, env, fx.userWallet.GetId(), &voidedStatus).GetTransactions()
	if len(voideds) != 1 || voideds[0].GetIdempotencyKey() != "audit-hold-2" {
		t.Fatalf("voided filter: got %v, want only audit-hold-2", keysOf(voideds))
	}

	// An explicit unspecified status is a caller bug, not a wildcard.
	bad := connect.NewRequest(&ledgerv1.ListWalletTransactionsRequest{
		WalletId: fx.userWallet.GetId(),
		Status:   ledgerv1.TransactionStatus_TRANSACTION_STATUS_UNSPECIFIED.Enum(),
	})
	_, err = env.audit.ListWalletTransactions(context.Background(), authed(t, bad))
	assertCode(t, err, connect.CodeInvalidArgument)
}

func keysOf(transactions []*ledgerv1.Transaction) []string {
	keys := make([]string, 0, len(transactions))
	for _, transaction := range transactions {
		keys = append(keys, transaction.GetIdempotencyKey())
	}
	return keys
}

// TestGetTransactionByIdempotencyKeyAndReference: both lookups return the
// exact transaction (id, key, reference, entries), and unknown keys map
// to a clear not-found.
func TestGetTransactionByIdempotencyKeyAndReference(t *testing.T) {
	env := setup(t)
	fx := newFundedWallet(t, env)

	original := mustPost(t, env, "audit-lookup-1", "stripe-charge-lookup",
		entry(fx.systemAccount.GetId(), "-5.00"),
		entry(fx.userAccount.GetId(), "5.00"))

	byKey, err := env.audit.GetTransactionByIdempotencyKey(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.GetTransactionByIdempotencyKeyRequest{IdempotencyKey: "audit-lookup-1"})))
	if err != nil {
		t.Fatalf("GetTransactionByIdempotencyKey: %v", err)
	}
	byRef, err := env.audit.GetTransactionByReference(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.GetTransactionByReferenceRequest{Reference: "stripe-charge-lookup"})))
	if err != nil {
		t.Fatalf("GetTransactionByReference: %v", err)
	}

	for name, got := range map[string]*ledgerv1.Transaction{"by key": byKey.Msg, "by reference": byRef.Msg} {
		if got.GetId() != original.GetId() {
			t.Fatalf("%s: id = %s, want %s", name, got.GetId(), original.GetId())
		}
		if got.GetIdempotencyKey() != "audit-lookup-1" {
			t.Fatalf("%s: idempotency_key = %s", name, got.GetIdempotencyKey())
		}
		if got.GetReference() != "stripe-charge-lookup" {
			t.Fatalf("%s: reference = %v", name, got.GetReference())
		}
		if len(got.GetEntries()) != len(original.GetEntries()) {
			t.Fatalf("%s: %d entries, want %d", name, len(got.GetEntries()), len(original.GetEntries()))
		}
		for i, entry := range got.GetEntries() {
			if entry.GetId() != original.GetEntries()[i].GetId() {
				t.Fatalf("%s: entry %d id mismatch", name, i)
			}
		}
	}

	// Unknown keys and references are clear not-founds.
	_, err = env.audit.GetTransactionByIdempotencyKey(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.GetTransactionByIdempotencyKeyRequest{IdempotencyKey: "never-used"})))
	assertCode(t, err, connect.CodeNotFound)

	_, err = env.audit.GetTransactionByReference(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.GetTransactionByReferenceRequest{Reference: "stripe-charge-nope"})))
	assertCode(t, err, connect.CodeNotFound)
}

// TestAuditLookupsRequireKeys: empty lookup keys are invalid_argument,
// and listing an unknown account or wallet is not_found.
func TestAuditLookupsRequireKeys(t *testing.T) {
	env := setup(t)
	fx := newFundedWallet(t, env)

	_, err := env.audit.GetTransactionByIdempotencyKey(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.GetTransactionByIdempotencyKeyRequest{})))
	assertCode(t, err, connect.CodeInvalidArgument)

	_, err = env.audit.GetTransactionByReference(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.GetTransactionByReferenceRequest{})))
	assertCode(t, err, connect.CodeInvalidArgument)

	_, err = env.audit.ListEntries(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.ListEntriesRequest{AccountId: "no-such-account"})))
	assertCode(t, err, connect.CodeNotFound)

	_, err = env.audit.ListWalletTransactions(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.ListWalletTransactionsRequest{WalletId: "no-such-wallet"})))
	assertCode(t, err, connect.CodeNotFound)

	// Existing accounts page normally: the fixture's user account has
	// exactly one movement (the funding credit), so a page of size 1 is
	// also the last page.
	res, err := env.audit.ListEntries(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.ListEntriesRequest{AccountId: fx.userAccount.GetId(), PageSize: 1})))
	if err != nil {
		t.Fatalf("ListEntries(existing account): %v", err)
	}
	if len(res.Msg.GetEntries()) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(res.Msg.GetEntries()))
	}
	if res.Msg.NextPageOffset != nil {
		t.Fatalf("single-entry account must not report a next page, got %d", *res.Msg.NextPageOffset)
	}

	// An existing account with no movements at all is a valid empty page,
	// not an error.
	asset := createAsset(t, env, "JPY", 0)
	unused := createAccount(t, env, fx.userWallet.GetId(), asset.GetId(), false)
	empty, err := env.audit.ListEntries(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.ListEntriesRequest{AccountId: unused.GetId()})))
	if err != nil {
		t.Fatalf("ListEntries(unused account): %v", err)
	}
	if len(empty.Msg.GetEntries()) != 0 || empty.Msg.NextPageOffset != nil {
		t.Fatalf("unused account: entries=%d next=%v, want empty page",
			len(empty.Msg.GetEntries()), empty.Msg.NextPageOffset)
	}
}

// TestListEntriesPagination: paging through an account's history with a
// small page size returns every entry exactly once, in stable
// chronological order, with next_page_offset present on every page but
// the last.
func TestListEntriesPagination(t *testing.T) {
	env := setup(t)
	fx := newFundedWallet(t, env)

	// 5 entries total on the user account.
	for i, amount := range []string{"-1.00", "-2.00", "-3.00", "-4.00"} {
		mustPost(t, env, fmt.Sprintf("audit-page-%d", i), "",
			entry(fx.userAccount.GetId(), amount),
			entry(fx.systemAccount.GetId(), amount[1:])) // credit mirrors the debit
	}

	var collected []string
	offset := uint32(0)
	for page := 0; ; page++ {
		res, err := env.audit.ListEntries(context.Background(), authed(t,
			connect.NewRequest(&ledgerv1.ListEntriesRequest{
				AccountId:  fx.userAccount.GetId(),
				PageSize:   2,
				PageOffset: offset,
			})))
		if err != nil {
			t.Fatalf("ListEntries page %d: %v", page, err)
		}
		entries := res.Msg.GetEntries()
		if len(entries) == 0 {
			t.Fatalf("page %d: empty page", page)
		}
		if len(entries) > 2 {
			t.Fatalf("page %d: %d entries, page_size was 2", page, len(entries))
		}
		for _, entry := range entries {
			collected = append(collected, entry.GetAmount())
		}
		if res.Msg.NextPageOffset == nil {
			break
		}
		offset = *res.Msg.NextPageOffset
		if page > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	// Stable chronological order across pages: funding credit first, then
	// the four debits in posting order. Amounts compare numerically —
	// Postgres normalizes NUMERIC display scale ("100.00" comes back as
	// "100").
	want := []string{"100.00", "-1.00", "-2.00", "-3.00", "-4.00"}
	if len(collected) != len(want) {
		t.Fatalf("collected %d entries, want %d", len(collected), len(want))
	}
	for i := range want {
		got, err := decimal.NewFromString(collected[i])
		if err != nil {
			t.Fatalf("entry %d amount %q is not decimal: %v", i, collected[i], err)
		}
		w, _ := decimal.NewFromString(want[i])
		if !got.Equal(w) {
			t.Fatalf("entry %d = %s, want %s (full order: %v)", i, collected[i], want[i], collected)
		}
	}
}

// TestListWalletTransactionsPagination: the wallet listing pages through
// all touching transactions exactly once with a stable order and correct
// next_page_offset bookkeeping.
func TestListWalletTransactionsPagination(t *testing.T) {
	env := setup(t)
	fx := newFundedWallet(t, env)

	for i, amount := range []string{"-1.00", "-2.00", "-3.00"} {
		mustPost(t, env, fmt.Sprintf("audit-wpage-%d", i), "",
			entry(fx.userAccount.GetId(), amount),
			entry(fx.systemAccount.GetId(), amount[1:]))
	}
	// 4 transactions touch the user wallet (funding + 3 debits).

	var collected []string
	offset := uint32(0)
	pages := 0
	for {
		res, err := env.audit.ListWalletTransactions(context.Background(), authed(t,
			connect.NewRequest(&ledgerv1.ListWalletTransactionsRequest{
				WalletId:   fx.userWallet.GetId(),
				PageSize:   3,
				PageOffset: offset,
			})))
		if err != nil {
			t.Fatalf("ListWalletTransactions page %d: %v", pages, err)
		}
		for _, transaction := range res.Msg.GetTransactions() {
			collected = append(collected, transaction.GetIdempotencyKey())
		}
		pages++
		if res.Msg.NextPageOffset == nil {
			break
		}
		offset = *res.Msg.NextPageOffset
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	// 2 pages: 3 transactions then 1.
	if pages != 2 {
		t.Fatalf("expected 2 pages, got %d", pages)
	}
	want := []string{"audit-fund-1", "audit-wpage-0", "audit-wpage-1", "audit-wpage-2"}
	if len(collected) != len(want) {
		t.Fatalf("collected %d transactions, want %d", len(collected), len(want))
	}
	for i := range want {
		if collected[i] != want[i] {
			t.Fatalf("transaction %d = %s, want %s (full order: %v)", i, collected[i], want[i], collected)
		}
	}
}
