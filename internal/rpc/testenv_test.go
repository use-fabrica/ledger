package rpc_test

// Shared test environment for the rpc package tests: one testcontainers
// harness, booted from the repo's embedded goose migrations and serving
// every Connect handler over httptest — the same mux wiring as cmd/ledger,
// including the auth interceptor on the private services. Every test file
// in this package builds its fixtures on these helpers instead of carrying
// private copies.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/use-fabrica/ledger/db/migrations"
	"github.com/use-fabrica/ledger/internal/ledger"
	"github.com/use-fabrica/ledger/internal/rpc"
	ledgerv1 "github.com/use-fabrica/ledger/proto/ledger/v1"
	ledgerv1connect "github.com/use-fabrica/ledger/proto/ledger/v1/ledgerv1connect"
)

const testAPIKey = "test-ledger-key"

// env is a fully wired ledger server backed by a real Postgres.
type env struct {
	provision ledgerv1connect.ProvisioningServiceClient
	posting   ledgerv1connect.PostingServiceClient
	audit     ledgerv1connect.AuditServiceClient
	health    ledgerv1connect.HealthServiceClient
}

func setup(t *testing.T) *env {
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

	// Fresh migrate up from the embedded migrations, exactly what
	// cmd/migrate runs in a deploy.
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

	// Same wiring as cmd/ledger: health public, everything else behind
	// the API-key interceptor.
	engine := ledger.New(pool)
	mux := http.NewServeMux()
	mux.Handle(ledgerv1connect.NewHealthServiceHandler(rpc.NewHealthHandler(pool)))
	mux.Handle(ledgerv1connect.NewProvisioningServiceHandler(
		rpc.NewProvisioningHandler(engine),
		connect.WithInterceptors(rpc.NewAuthInterceptor(testAPIKey)),
	))
	mux.Handle(ledgerv1connect.NewPostingServiceHandler(
		rpc.NewPostingHandler(engine),
		connect.WithInterceptors(rpc.NewAuthInterceptor(testAPIKey)),
	))
	mux.Handle(ledgerv1connect.NewAuditServiceHandler(
		rpc.NewAuditHandler(engine),
		connect.WithInterceptors(rpc.NewAuthInterceptor(testAPIKey)),
	))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &env{
		provision: ledgerv1connect.NewProvisioningServiceClient(http.DefaultClient, server.URL),
		posting:   ledgerv1connect.NewPostingServiceClient(http.DefaultClient, server.URL),
		audit:     ledgerv1connect.NewAuditServiceClient(http.DefaultClient, server.URL),
		health:    ledgerv1connect.NewHealthServiceClient(http.DefaultClient, server.URL),
	}
}

// authed stamps the test API key on a request.
func authed[T any](t *testing.T, req *connect.Request[T]) *connect.Request[T] {
	t.Helper()
	req.Header().Set("X-Api-Key", testAPIKey)
	return req
}

// createAsset provisions an asset.
func createAsset(t *testing.T, env *env, code string, precision int32) *ledgerv1.Asset {
	t.Helper()
	asset, err := env.provision.CreateAsset(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.CreateAssetRequest{Code: code, Precision: precision})))
	if err != nil {
		t.Fatalf("CreateAsset(%s): %v", code, err)
	}
	return asset.Msg
}

func createHolder(t *testing.T, env *env, ref string) *ledgerv1.Holder {
	t.Helper()
	return createHolderWithType(t, env, ref, ledgerv1.HolderType_HOLDER_TYPE_USER)
}

func createHolderWithType(t *testing.T, env *env, ref string, holderType ledgerv1.HolderType) *ledgerv1.Holder {
	t.Helper()
	holder, err := env.provision.CreateHolder(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.CreateHolderRequest{Type: holderType, ExternalRef: ref})))
	if err != nil {
		t.Fatalf("CreateHolder(%s): %v", ref, err)
	}
	return holder.Msg
}

func createWallet(t *testing.T, env *env, holderID, name string) *ledgerv1.Wallet {
	t.Helper()
	wallet, err := env.provision.CreateWallet(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.CreateWalletRequest{HolderId: holderID, Name: name})))
	if err != nil {
		t.Fatalf("CreateWallet(%s): %v", name, err)
	}
	return wallet.Msg
}

func createAccount(t *testing.T, env *env, walletID, assetID string, allowNegative bool) *ledgerv1.Account {
	t.Helper()
	account, err := env.provision.CreateAccount(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.CreateAccountRequest{
			WalletId:      walletID,
			AssetId:       assetID,
			AllowNegative: &allowNegative,
		})))
	if err != nil {
		t.Fatalf("CreateAccount(%s, %s): %v", walletID, assetID, err)
	}
	return account.Msg
}

// entry is one signed wire entry of a transaction request.
func entry(accountID, amount string) *ledgerv1.TransactionInputEntry {
	return &ledgerv1.TransactionInputEntry{AccountId: accountID, Amount: amount}
}

func stringPtr(s string) *string { return &s }

// post sends CreateTransaction with one entry per account/amount pair.
func post(t *testing.T, env *env, key, reference string, entries ...*ledgerv1.TransactionInputEntry) (*ledgerv1.Transaction, error) {
	t.Helper()
	req := connect.NewRequest(&ledgerv1.CreateTransactionRequest{
		IdempotencyKey: key,
		Reference:      stringPtr(reference),
		Entries:        entries,
	})
	resp, err := env.posting.CreateTransaction(context.Background(), authed(t, req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// mustPost sends CreateTransaction and fails the test on error.
func mustPost(t *testing.T, env *env, key, reference string, entries ...*ledgerv1.TransactionInputEntry) *ledgerv1.Transaction {
	t.Helper()
	transaction, err := post(t, env, key, reference, entries...)
	if err != nil {
		t.Fatalf("CreateTransaction(%s): %v", key, err)
	}
	return transaction
}

// createPending sends CreatePendingTransaction with one entry per
// account/amount pair.
func createPending(t *testing.T, env *env, key, reference string, entries ...*ledgerv1.TransactionInputEntry) (*ledgerv1.Transaction, error) {
	t.Helper()
	req := connect.NewRequest(&ledgerv1.CreatePendingTransactionRequest{
		IdempotencyKey: key,
		Reference:      stringPtr(reference),
		Entries:        entries,
	})
	resp, err := env.posting.CreatePendingTransaction(context.Background(), authed(t, req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// postPending settles a pending transaction via PostTransaction.
func postPending(t *testing.T, env *env, id string) (*ledgerv1.Transaction, error) {
	t.Helper()
	resp, err := env.posting.PostTransaction(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.PostTransactionRequest{Id: id})))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// voidPending releases a pending transaction via VoidTransaction.
func voidPending(t *testing.T, env *env, id string) (*ledgerv1.Transaction, error) {
	t.Helper()
	resp, err := env.posting.VoidTransaction(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.VoidTransactionRequest{Id: id})))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// getTransaction fetches one transaction by ID.
func getTransaction(t *testing.T, env *env, id string) (*ledgerv1.Transaction, error) {
	t.Helper()
	resp, err := env.posting.GetTransaction(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.GetTransactionRequest{Id: id})))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// assertCode fails unless err is a Connect error with the wanted code.
func assertCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %v, got nil", want)
	}
	var connErr *connect.Error
	if !errors.As(err, &connErr) {
		t.Fatalf("error is not a connect.Error: %v (%T)", err, err)
	}
	if connErr.Code() != want {
		t.Fatalf("error code = %v, want %v (err: %v)", connErr.Code(), want, err)
	}
}
