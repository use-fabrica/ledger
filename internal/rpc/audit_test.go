package rpc_test

// Self-contained test environment for ticket #6: it duplicates the
// testcontainers setup from provisioning_test.go under its own names so
// the shared test files (owned by ticket #5) can evolve without breaking
// these tests. All fixture helpers are prefixed audit* for the same
// reason.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/shopspring/decimal"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/use-fabrica/ledger/db/migrations"
	"github.com/use-fabrica/ledger/internal/ledger"
	"github.com/use-fabrica/ledger/internal/rpc"
	ledgerv1 "github.com/use-fabrica/ledger/proto/ledger/v1"
	ledgerv1connect "github.com/use-fabrica/ledger/proto/ledger/v1/ledgerv1connect"
)

const auditAPIKey = "test-ledger-key"

// auditEnv boots a real Postgres from the embedded goose migrations and
// serves every Connect handler over httptest — the same mux wiring as
// cmd/ledger, including the auth interceptor on the private services.
type auditEnv struct {
	provision ledgerv1connect.ProvisioningServiceClient
	posting   ledgerv1connect.PostingServiceClient
	audit     ledgerv1connect.AuditServiceClient
	pool      *pgxpool.Pool
}

func setupAudit(t *testing.T) *auditEnv {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("ledger"),
		postgres.WithUsername("ledger"),
		postgres.WithPassword("ledger"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("container connection string: %v", err)
	}

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	migDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open migration db: %v", err)
	}
	if err := goose.Up(migDB, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	_ = migDB.Close()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)

	engine := ledger.New(pool)
	mux := http.NewServeMux()
	mux.Handle(ledgerv1connect.NewProvisioningServiceHandler(
		rpc.NewProvisioningHandler(pool, engine),
		connect.WithInterceptors(rpc.NewAuthInterceptor(auditAPIKey)),
	))
	mux.Handle(ledgerv1connect.NewPostingServiceHandler(
		rpc.NewPostingHandler(engine),
		connect.WithInterceptors(rpc.NewAuthInterceptor(auditAPIKey)),
	))
	mux.Handle(ledgerv1connect.NewAuditServiceHandler(
		rpc.NewAuditHandler(engine),
		connect.WithInterceptors(rpc.NewAuthInterceptor(auditAPIKey)),
	))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &auditEnv{
		provision: ledgerv1connect.NewProvisioningServiceClient(http.DefaultClient, server.URL),
		posting:   ledgerv1connect.NewPostingServiceClient(http.DefaultClient, server.URL),
		audit:     ledgerv1connect.NewAuditServiceClient(http.DefaultClient, server.URL),
		pool:      pool,
	}
}

func auditAuthed[T any](t *testing.T, req *connect.Request[T]) *connect.Request[T] {
	t.Helper()
	req.Header().Set("X-Api-Key", auditAPIKey)
	return req
}

func auditAsset(t *testing.T, env *auditEnv, code string, precision int32) *ledgerv1.Asset {
	t.Helper()
	res, err := env.provision.CreateAsset(context.Background(), auditAuthed(t,
		connect.NewRequest(&ledgerv1.CreateAssetRequest{Code: code, Precision: precision})))
	if err != nil {
		t.Fatalf("CreateAsset(%s): %v", code, err)
	}
	return res.Msg
}

func auditHolder(t *testing.T, env *auditEnv, ref string, holderType ledgerv1.HolderType) *ledgerv1.Holder {
	t.Helper()
	res, err := env.provision.CreateHolder(context.Background(), auditAuthed(t,
		connect.NewRequest(&ledgerv1.CreateHolderRequest{Type: holderType, ExternalRef: ref})))
	if err != nil {
		t.Fatalf("CreateHolder(%s): %v", ref, err)
	}
	return res.Msg
}

func auditWallet(t *testing.T, env *auditEnv, holderID, name string) *ledgerv1.Wallet {
	t.Helper()
	res, err := env.provision.CreateWallet(context.Background(), auditAuthed(t,
		connect.NewRequest(&ledgerv1.CreateWalletRequest{HolderId: holderID, Name: name})))
	if err != nil {
		t.Fatalf("CreateWallet(%s): %v", name, err)
	}
	return res.Msg
}

func auditAccount(t *testing.T, env *auditEnv, walletID, assetID string, allowNegative bool) *ledgerv1.Account {
	t.Helper()
	res, err := env.provision.CreateAccount(context.Background(), auditAuthed(t,
		connect.NewRequest(&ledgerv1.CreateAccountRequest{
			WalletId: walletID, AssetId: assetID, AllowNegative: &allowNegative,
		})))
	if err != nil {
		t.Fatalf("CreateAccount(%s): %v", walletID, err)
	}
	return res.Msg
}

// auditAssertCode fails unless err is a Connect error with the wanted
// code. Local rather than the shared assertCode helper: these tests must
// not depend on files other tickets own.
func auditAssertCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected Connect code %s, got nil error", want)
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("error is not a Connect error: %v", err)
	}
	if connectErr.Code() != want {
		t.Fatalf("Connect code = %s, want %s (error: %v)", connectErr.Code(), want, err)
	}
}

// auditPost sends CreateTransaction and fails the test on error.
func auditPost(t *testing.T, env *auditEnv, key, reference string, entries ...*ledgerv1.TransactionInputEntry) *ledgerv1.Transaction {
	t.Helper()
	req := connect.NewRequest(&ledgerv1.CreateTransactionRequest{
		IdempotencyKey: key,
		Entries:        entries,
	})
	if reference != "" {
		req.Msg.Reference = &reference
	}
	res, err := env.posting.CreateTransaction(context.Background(), auditAuthed(t, req))
	if err != nil {
		t.Fatalf("CreateTransaction(%s): %v", key, err)
	}
	return res.Msg
}

func auditEntry(accountID, amount string) *ledgerv1.TransactionInputEntry {
	return &ledgerv1.TransactionInputEntry{AccountId: accountID, Amount: amount}
}

// auditFundedWallet is the standard fixture: one asset, a system account
// flagged allow_negative, and one user wallet/account starting at zero.
type auditFundedWallet struct {
	systemAccount *ledgerv1.Account
	userWallet    *ledgerv1.Wallet
	userAccount   *ledgerv1.Account
}

func newAuditFundedWallet(t *testing.T, env *auditEnv) auditFundedWallet {
	t.Helper()
	asset := auditAsset(t, env, "USD", 2)

	system := auditHolder(t, env, "audit-system", ledgerv1.HolderType_HOLDER_TYPE_SYSTEM)
	systemWallet := auditWallet(t, env, system.GetId(), "system ops")
	systemAccount := auditAccount(t, env, systemWallet.GetId(), asset.GetId(), true)

	user := auditHolder(t, env, "audit-user", ledgerv1.HolderType_HOLDER_TYPE_USER)
	userWallet := auditWallet(t, env, user.GetId(), "user wallet")
	userAccount := auditAccount(t, env, userWallet.GetId(), asset.GetId(), false)

	auditPost(t, env, "audit-fund-1", "stripe-charge-fund",
		auditEntry(systemAccount.GetId(), "-100.00"),
		auditEntry(userAccount.GetId(), "100.00"))

	return auditFundedWallet{
		systemAccount: systemAccount,
		userWallet:    userWallet,
		userAccount:   userAccount,
	}
}

// TestListEntriesChronologicalWithRunningTotals: the entry history of an
// account lists every movement in order, and accumulating the signed
// amounts reconstructs the posted balance — the audit trail and the
// materialized balance agree by construction.
func TestListEntriesChronologicalWithRunningTotals(t *testing.T) {
	env := setupAudit(t)
	fx := newAuditFundedWallet(t, env)

	auditPost(t, env, "audit-pay-1", "stripe-charge-1",
		auditEntry(fx.userAccount.GetId(), "-30.00"),
		auditEntry(fx.systemAccount.GetId(), "30.00"))
	auditPost(t, env, "audit-pay-2", "stripe-charge-2",
		auditEntry(fx.userAccount.GetId(), "-12.50"),
		auditEntry(fx.systemAccount.GetId(), "12.50"))

	res, err := env.audit.ListEntries(context.Background(), auditAuthed(t,
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
	balances, err := env.provision.GetWalletBalances(context.Background(), auditAuthed(t,
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
// transaction history contains every posted transaction that touches any
// of its accounts — including a multi-asset transaction that hits two
// accounts of the same wallet, which must appear exactly once.
func TestListWalletTransactionsReturnsAllTouchingTransactions(t *testing.T) {
	env := setupAudit(t)
	fx := newAuditFundedWallet(t, env) // funds user account with 100.00

	// A second user wallet with an account, never party to any
	// transaction.
	other := auditHolder(t, env, "audit-other", ledgerv1.HolderType_HOLDER_TYPE_USER)
	otherWallet := auditWallet(t, env, other.GetId(), "other wallet")
	otherAsset := auditAsset(t, env, "EUR", 2)
	auditAccount(t, env, otherWallet.GetId(), otherAsset.GetId(), false)

	// Moves value out of the user wallet to the other wallet...
	auditPost(t, env, "audit-xfer-out", "stripe-charge-out",
		auditEntry(fx.userAccount.GetId(), "-30.00"),
		auditEntry(fx.systemAccount.GetId(), "30.00"))
	// ...and a multi-asset transaction touching TWO accounts of the user
	// wallet (USD debit + EUR credit through the system accounts). The
	// user wallet must list it once, not twice.
	userEUR := auditAccount(t, env, fx.userWallet.GetId(), otherAsset.GetId(), false)
	systemEUR := auditAccount(t, env, auditWallet(t, env,
		auditHolder(t, env, "audit-system-2", ledgerv1.HolderType_HOLDER_TYPE_SYSTEM).GetId(),
		"system ops 2").GetId(), otherAsset.GetId(), true)
	auditPost(t, env, "audit-multi-asset", "stripe-charge-multi",
		auditEntry(fx.userAccount.GetId(), "-10.00"),
		auditEntry(fx.systemAccount.GetId(), "10.00"),
		auditEntry(systemEUR.GetId(), "-20.00"),
		auditEntry(userEUR.GetId(), "20.00"))

	res, err := env.audit.ListWalletTransactions(context.Background(), auditAuthed(t,
		connect.NewRequest(&ledgerv1.ListWalletTransactionsRequest{WalletId: fx.userWallet.GetId()})))
	if err != nil {
		t.Fatalf("ListWalletTransactions: %v", err)
	}
	transactions := res.Msg.GetTransactions()

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
	otherRes, err := env.audit.ListWalletTransactions(context.Background(), auditAuthed(t,
		connect.NewRequest(&ledgerv1.ListWalletTransactionsRequest{WalletId: otherWallet.GetId()})))
	if err != nil {
		t.Fatalf("ListWalletTransactions(other): %v", err)
	}
	if len(otherRes.Msg.GetTransactions()) != 0 {
		t.Fatalf("other wallet: expected 0 transactions, got %d", len(otherRes.Msg.GetTransactions()))
	}
}

// TestGetTransactionByIdempotencyKeyAndReference: both lookups return the
// exact transaction (id, key, reference, entries), and unknown keys map
// to a clear not-found.
func TestGetTransactionByIdempotencyKeyAndReference(t *testing.T) {
	env := setupAudit(t)
	fx := newAuditFundedWallet(t, env)

	original := auditPost(t, env, "audit-lookup-1", "stripe-charge-lookup",
		auditEntry(fx.systemAccount.GetId(), "-5.00"),
		auditEntry(fx.userAccount.GetId(), "5.00"))

	byKey, err := env.audit.GetTransactionByIdempotencyKey(context.Background(), auditAuthed(t,
		connect.NewRequest(&ledgerv1.GetTransactionByIdempotencyKeyRequest{IdempotencyKey: "audit-lookup-1"})))
	if err != nil {
		t.Fatalf("GetTransactionByIdempotencyKey: %v", err)
	}
	byRef, err := env.audit.GetTransactionByReference(context.Background(), auditAuthed(t,
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
	_, err = env.audit.GetTransactionByIdempotencyKey(context.Background(), auditAuthed(t,
		connect.NewRequest(&ledgerv1.GetTransactionByIdempotencyKeyRequest{IdempotencyKey: "never-used"})))
	auditAssertCode(t, err, connect.CodeNotFound)

	_, err = env.audit.GetTransactionByReference(context.Background(), auditAuthed(t,
		connect.NewRequest(&ledgerv1.GetTransactionByReferenceRequest{Reference: "stripe-charge-nope"})))
	auditAssertCode(t, err, connect.CodeNotFound)
}

// TestAuditLookupsRequireKeys: empty lookup keys are invalid_argument,
// and listing an unknown account or wallet is not_found.
func TestAuditLookupsRequireKeys(t *testing.T) {
	env := setupAudit(t)
	fx := newAuditFundedWallet(t, env)

	_, err := env.audit.GetTransactionByIdempotencyKey(context.Background(), auditAuthed(t,
		connect.NewRequest(&ledgerv1.GetTransactionByIdempotencyKeyRequest{})))
	auditAssertCode(t, err, connect.CodeInvalidArgument)

	_, err = env.audit.GetTransactionByReference(context.Background(), auditAuthed(t,
		connect.NewRequest(&ledgerv1.GetTransactionByReferenceRequest{})))
	auditAssertCode(t, err, connect.CodeInvalidArgument)

	_, err = env.audit.ListEntries(context.Background(), auditAuthed(t,
		connect.NewRequest(&ledgerv1.ListEntriesRequest{AccountId: "no-such-account"})))
	auditAssertCode(t, err, connect.CodeNotFound)

	_, err = env.audit.ListWalletTransactions(context.Background(), auditAuthed(t,
		connect.NewRequest(&ledgerv1.ListWalletTransactionsRequest{WalletId: "no-such-wallet"})))
	auditAssertCode(t, err, connect.CodeNotFound)

	// Existing accounts page normally: the fixture's user account has
	// exactly one movement (the funding credit), so a page of size 1 is
	// also the last page.
	res, err := env.audit.ListEntries(context.Background(), auditAuthed(t,
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
	asset := auditAsset(t, env, "JPY", 0)
	unused := auditAccount(t, env, fx.userWallet.GetId(), asset.GetId(), false)
	empty, err := env.audit.ListEntries(context.Background(), auditAuthed(t,
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
	env := setupAudit(t)
	fx := newAuditFundedWallet(t, env)

	// 5 entries total on the user account.
	for i, amount := range []string{"-1.00", "-2.00", "-3.00", "-4.00"} {
		auditPost(t, env, fmt.Sprintf("audit-page-%d", i), "",
			auditEntry(fx.userAccount.GetId(), amount),
			auditEntry(fx.systemAccount.GetId(), amount[1:])) // credit mirrors the debit
	}

	var collected []string
	offset := uint32(0)
	for page := 0; ; page++ {
		res, err := env.audit.ListEntries(context.Background(), auditAuthed(t,
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
	env := setupAudit(t)
	fx := newAuditFundedWallet(t, env)

	for i, amount := range []string{"-1.00", "-2.00", "-3.00"} {
		auditPost(t, env, fmt.Sprintf("audit-wpage-%d", i), "",
			auditEntry(fx.userAccount.GetId(), amount),
			auditEntry(fx.systemAccount.GetId(), amount[1:]))
	}
	// 4 transactions touch the user wallet (funding + 3 debits).

	var collected []string
	offset := uint32(0)
	pages := 0
	for {
		res, err := env.audit.ListWalletTransactions(context.Background(), auditAuthed(t,
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
