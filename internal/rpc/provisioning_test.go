package rpc_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/use-fabrica/ledger/db/migrations"
	"github.com/use-fabrica/ledger/internal/ledger"
	"github.com/use-fabrica/ledger/internal/rpc"
	ledgerv1 "github.com/use-fabrica/ledger/proto/ledger/v1"
	ledgerv1connect "github.com/use-fabrica/ledger/proto/ledger/v1/ledgerv1connect"
)

const testAPIKey = "test-ledger-key"

// env boots a real Postgres from the repo's embedded goose migrations and
// serves the Connect handlers over httptest — the same mux wiring as
// cmd/ledger, including the auth interceptor on ProvisioningService.
type env struct {
	provision  ledgerv1connect.ProvisioningServiceClient
	posting    ledgerv1connect.PostingServiceClient
	health     ledgerv1connect.HealthServiceClient
	httpServer *httptest.Server
	container  *postgres.PostgresContainer
	pool       *pgxpool.Pool
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

	// Same wiring as cmd/ledger: health public, provisioning behind the
	// API-key interceptor.
	engine := ledger.New(pool)
	mux := http.NewServeMux()
	mux.Handle(ledgerv1connect.NewHealthServiceHandler(rpc.NewHealthHandler(pool)))
	mux.Handle(ledgerv1connect.NewProvisioningServiceHandler(
		rpc.NewProvisioningHandler(pool, engine),
		connect.WithInterceptors(rpc.NewAuthInterceptor(testAPIKey)),
	))
	mux.Handle(ledgerv1connect.NewPostingServiceHandler(
		rpc.NewPostingHandler(engine),
		connect.WithInterceptors(rpc.NewAuthInterceptor(testAPIKey)),
	))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &env{
		provision:  ledgerv1connect.NewProvisioningServiceClient(http.DefaultClient, server.URL),
		posting:    ledgerv1connect.NewPostingServiceClient(http.DefaultClient, server.URL),
		health:     ledgerv1connect.NewHealthServiceClient(http.DefaultClient, server.URL),
		httpServer: server,
		container:  container,
		pool:       pool,
	}
}

// createAsset provisions an asset with a well-known precision.
func createAsset(t *testing.T, env *env, code string) *ledgerv1.Asset {
	t.Helper()
	req := connect.NewRequest(&ledgerv1.CreateAssetRequest{Code: code, Precision: 2})
	req.Header().Set("X-Api-Key", testAPIKey)
	asset, err := env.provision.CreateAsset(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateAsset(%s): %v", code, err)
	}
	if asset.Msg.GetId() == "" {
		t.Fatalf("CreateAsset(%s): empty id", code)
	}
	return asset.Msg
}

func TestCreateAndListAssets(t *testing.T) {
	env := setup(t)

	usd := createAsset(t, env, "USD")
	if usd.GetCode() != "USD" || usd.GetPrecision() != 2 {
		t.Fatalf("unexpected asset: %v", usd)
	}
	if usd.GetCreatedAt() == nil {
		t.Fatal("asset missing created_at")
	}

	createAsset(t, env, "EUR")

	listReq := connect.NewRequest(&ledgerv1.ListAssetsRequest{})
	listReq.Header().Set("X-Api-Key", testAPIKey)
	listed, err := env.provision.ListAssets(context.Background(), listReq)
	if err != nil {
		t.Fatalf("ListAssets: %v", err)
	}
	if len(listed.Msg.GetAssets()) != 2 {
		t.Fatalf("ListAssets: got %d assets, want 2", len(listed.Msg.GetAssets()))
	}
}

func TestDuplicateAssetCodeRejected(t *testing.T) {
	env := setup(t)
	createAsset(t, env, "USD")

	req := connect.NewRequest(&ledgerv1.CreateAssetRequest{Code: "USD", Precision: 2})
	req.Header().Set("X-Api-Key", testAPIKey)
	_, err := env.provision.CreateAsset(context.Background(), req)
	assertCode(t, err, connect.CodeAlreadyExists)
}

func TestCreateHolders(t *testing.T) {
	env := setup(t)

	for i, holderType := range []ledgerv1.HolderType{
		ledgerv1.HolderType_HOLDER_TYPE_USER,
		ledgerv1.HolderType_HOLDER_TYPE_SYSTEM,
		ledgerv1.HolderType_HOLDER_TYPE_PROVIDER,
	} {
		req := connect.NewRequest(&ledgerv1.CreateHolderRequest{
			Type:        holderType,
			ExternalRef: []string{"user-1", "system-fees", "stripe"}[i],
		})
		req.Header().Set("X-Api-Key", testAPIKey)
		holder, err := env.provision.CreateHolder(context.Background(), req)
		if err != nil {
			t.Fatalf("CreateHolder(%v): %v", holderType, err)
		}
		if holder.Msg.GetType() != holderType || holder.Msg.GetExternalRef() == "" {
			t.Fatalf("unexpected holder: %v", holder.Msg)
		}
	}
}

func TestHolderTypeValidation(t *testing.T) {
	env := setup(t)

	req := connect.NewRequest(&ledgerv1.CreateHolderRequest{
		Type:        ledgerv1.HolderType_HOLDER_TYPE_UNSPECIFIED,
		ExternalRef: "no-type",
	})
	req.Header().Set("X-Api-Key", testAPIKey)
	if _, err := env.provision.CreateHolder(context.Background(), req); err == nil {
		t.Fatal("CreateHolder with unspecified type: expected error")
	} else {
		assertCode(t, err, connect.CodeInvalidArgument)
	}
}

func TestDuplicateHolderExternalRefRejected(t *testing.T) {
	env := setup(t)

	req := connect.NewRequest(&ledgerv1.CreateHolderRequest{
		Type:        ledgerv1.HolderType_HOLDER_TYPE_USER,
		ExternalRef: "dup-ref",
	})
	req.Header().Set("X-Api-Key", testAPIKey)
	if _, err := env.provision.CreateHolder(context.Background(), req); err != nil {
		t.Fatalf("first CreateHolder: %v", err)
	}

	req2 := connect.NewRequest(&ledgerv1.CreateHolderRequest{
		Type:        ledgerv1.HolderType_HOLDER_TYPE_PROVIDER,
		ExternalRef: "dup-ref",
	})
	req2.Header().Set("X-Api-Key", testAPIKey)
	_, err := env.provision.CreateHolder(context.Background(), req2)
	assertCode(t, err, connect.CodeAlreadyExists)
}

func createHolder(t *testing.T, env *env, ref string) *ledgerv1.Holder {
	t.Helper()
	req := connect.NewRequest(&ledgerv1.CreateHolderRequest{
		Type:        ledgerv1.HolderType_HOLDER_TYPE_USER,
		ExternalRef: ref,
	})
	req.Header().Set("X-Api-Key", testAPIKey)
	holder, err := env.provision.CreateHolder(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateHolder(%s): %v", ref, err)
	}
	return holder.Msg
}

func TestCreateWallet(t *testing.T) {
	env := setup(t)
	holder := createHolder(t, env, "wallet-owner")

	req := connect.NewRequest(&ledgerv1.CreateWalletRequest{
		HolderId: holder.GetId(),
		Name:     "main",
	})
	req.Header().Set("X-Api-Key", testAPIKey)
	wallet, err := env.provision.CreateWallet(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateWallet: %v", err)
	}
	if wallet.Msg.GetHolderId() != holder.GetId() || wallet.Msg.GetName() != "main" {
		t.Fatalf("unexpected wallet: %v", wallet.Msg)
	}
}

func TestCreateWalletUnknownHolder(t *testing.T) {
	env := setup(t)

	req := connect.NewRequest(&ledgerv1.CreateWalletRequest{
		HolderId: "holder-does-not-exist",
		Name:     "main",
	})
	req.Header().Set("X-Api-Key", testAPIKey)
	_, err := env.provision.CreateWallet(context.Background(), req)
	assertCode(t, err, connect.CodeNotFound)
}

func createWallet(t *testing.T, env *env, holderID, name string) *ledgerv1.Wallet {
	t.Helper()
	req := connect.NewRequest(&ledgerv1.CreateWalletRequest{HolderId: holderID, Name: name})
	req.Header().Set("X-Api-Key", testAPIKey)
	wallet, err := env.provision.CreateWallet(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateWallet(%s): %v", name, err)
	}
	return wallet.Msg
}

func createAccount(t *testing.T, env *env, walletID, assetID string, allowNegative bool) *ledgerv1.Account {
	t.Helper()
	req := connect.NewRequest(&ledgerv1.CreateAccountRequest{
		WalletId:      walletID,
		AssetId:       assetID,
		AllowNegative: &allowNegative,
	})
	req.Header().Set("X-Api-Key", testAPIKey)
	account, err := env.provision.CreateAccount(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateAccount(%s, %s): %v", walletID, assetID, err)
	}
	return account.Msg
}

func TestCreateAccountDefaultsAndBalances(t *testing.T) {
	env := setup(t)
	holder := createHolder(t, env, "acct-holder")
	wallet := createWallet(t, env, holder.GetId(), "main")
	usd := createAsset(t, env, "USD")
	eur := createAsset(t, env, "EUR")

	// Unset allow_negative defaults to false.
	req := connect.NewRequest(&ledgerv1.CreateAccountRequest{
		WalletId: wallet.GetId(),
		AssetId:  usd.GetId(),
	})
	req.Header().Set("X-Api-Key", testAPIKey)
	account, err := env.provision.CreateAccount(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if account.Msg.GetAllowNegative() {
		t.Fatal("allow_negative must default to false")
	}

	// Explicitly flagged system-style account.
	system := createAccount(t, env, wallet.GetId(), eur.GetId(), true)
	if !system.GetAllowNegative() {
		t.Fatal("explicit allow_negative=true not persisted")
	}

	// Balance read returns every account with zero posted/pending.
	balReq := connect.NewRequest(&ledgerv1.GetWalletBalancesRequest{WalletId: wallet.GetId()})
	balReq.Header().Set("X-Api-Key", testAPIKey)
	balances, err := env.provision.GetWalletBalances(context.Background(), balReq)
	if err != nil {
		t.Fatalf("GetWalletBalances: %v", err)
	}
	got := balances.Msg.GetBalances()
	if len(got) != 2 {
		t.Fatalf("GetWalletBalances: got %d accounts, want 2", len(got))
	}
	for _, b := range got {
		if b.GetPosted() != "0" || b.GetPending() != "0" {
			t.Fatalf("new account balance must be zeroed, got posted=%q pending=%q", b.GetPosted(), b.GetPending())
		}
		if b.GetAccount().GetId() == "" {
			t.Fatal("balance missing account")
		}
	}
}

func TestDuplicateWalletAssetPairRejected(t *testing.T) {
	env := setup(t)
	holder := createHolder(t, env, "pair-holder")
	wallet := createWallet(t, env, holder.GetId(), "main")
	usd := createAsset(t, env, "USD")

	createAccount(t, env, wallet.GetId(), usd.GetId(), false)

	req := connect.NewRequest(&ledgerv1.CreateAccountRequest{
		WalletId: wallet.GetId(),
		AssetId:  usd.GetId(),
	})
	req.Header().Set("X-Api-Key", testAPIKey)
	_, err := env.provision.CreateAccount(context.Background(), req)
	assertCode(t, err, connect.CodeAlreadyExists)

	// A different wallet may hold an account for the same asset.
	other := createWallet(t, env, holder.GetId(), "second")
	createAccount(t, env, other.GetId(), usd.GetId(), false)
}

func TestCreateAccountUnknownReferences(t *testing.T) {
	env := setup(t)
	holder := createHolder(t, env, "ref-holder")
	wallet := createWallet(t, env, holder.GetId(), "main")
	usd := createAsset(t, env, "USD")

	// Unknown asset.
	req := connect.NewRequest(&ledgerv1.CreateAccountRequest{
		WalletId: wallet.GetId(),
		AssetId:  "asset-does-not-exist",
	})
	req.Header().Set("X-Api-Key", testAPIKey)
	_, err := env.provision.CreateAccount(context.Background(), req)
	assertCode(t, err, connect.CodeNotFound)

	// Unknown wallet.
	req2 := connect.NewRequest(&ledgerv1.CreateAccountRequest{
		WalletId: "wallet-does-not-exist",
		AssetId:  usd.GetId(),
	})
	req2.Header().Set("X-Api-Key", testAPIKey)
	_, err = env.provision.CreateAccount(context.Background(), req2)
	assertCode(t, err, connect.CodeNotFound)
}

func TestGetWalletBalancesUnknownWallet(t *testing.T) {
	env := setup(t)

	req := connect.NewRequest(&ledgerv1.GetWalletBalancesRequest{WalletId: "nope"})
	req.Header().Set("X-Api-Key", testAPIKey)
	_, err := env.provision.GetWalletBalances(context.Background(), req)
	assertCode(t, err, connect.CodeNotFound)
}

func TestAuthRejectsMissingAndInvalidKey(t *testing.T) {
	env := setup(t)

	// No key at all.
	if _, err := env.provision.ListAssets(context.Background(), connect.NewRequest(&ledgerv1.ListAssetsRequest{})); err == nil {
		t.Fatal("request without API key: expected error")
	} else {
		assertCode(t, err, connect.CodeUnauthenticated)
	}

	// Wrong key.
	req := connect.NewRequest(&ledgerv1.ListAssetsRequest{})
	req.Header().Set("X-Api-Key", "wrong-key")
	if _, err := env.provision.ListAssets(context.Background(), req); err == nil {
		t.Fatal("request with invalid API key: expected error")
	} else {
		assertCode(t, err, connect.CodeUnauthenticated)
	}

	// Bearer form of the valid key is accepted.
	bearer := connect.NewRequest(&ledgerv1.ListAssetsRequest{})
	bearer.Header().Set("Authorization", "Bearer "+testAPIKey)
	if _, err := env.provision.ListAssets(context.Background(), bearer); err != nil {
		t.Fatalf("request with Bearer API key: %v", err)
	}
}

func TestHealthIsPublic(t *testing.T) {
	env := setup(t)

	// No API key on the health service: orchestrators must always get in.
	resp, err := env.health.CheckLiveness(context.Background(), connect.NewRequest(&emptypb.Empty{}))
	if err != nil {
		t.Fatalf("CheckLiveness without key: %v", err)
	}
	if resp.Msg.GetStatus() != ledgerv1.ServingStatus_SERVING {
		t.Fatalf("liveness = %v, want SERVING", resp.Msg.GetStatus())
	}

	ready, err := env.health.CheckReadiness(context.Background(), connect.NewRequest(&emptypb.Empty{}))
	if err != nil {
		t.Fatalf("CheckReadiness without key: %v", err)
	}
	if ready.Msg.GetStatus() != ledgerv1.ServingStatus_SERVING {
		t.Fatalf("readiness = %v, want SERVING (database reachable)", ready.Msg.GetStatus())
	}
}

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
