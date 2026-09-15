package rpc_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	ledgerv1 "github.com/use-fabrica/ledger/proto/ledger/v1"
)

func TestCreateAndListAssets(t *testing.T) {
	env := setup(t)

	usd := createAsset(t, env, "USD", 2)
	if usd.GetCode() != "USD" || usd.GetPrecision() != 2 {
		t.Fatalf("unexpected asset: %v", usd)
	}
	if usd.GetCreatedAt() == nil {
		t.Fatal("asset missing created_at")
	}

	createAsset(t, env, "EUR", 2)

	listed, err := env.provision.ListAssets(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.ListAssetsRequest{})))
	if err != nil {
		t.Fatalf("ListAssets: %v", err)
	}
	if len(listed.Msg.GetAssets()) != 2 {
		t.Fatalf("ListAssets: got %d assets, want 2", len(listed.Msg.GetAssets()))
	}
}

func TestDuplicateAssetCodeRejected(t *testing.T) {
	env := setup(t)
	createAsset(t, env, "USD", 2)

	_, err := env.provision.CreateAsset(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.CreateAssetRequest{Code: "USD", Precision: 2})))
	assertCode(t, err, connect.CodeAlreadyExists)
}

func TestCreateHolders(t *testing.T) {
	env := setup(t)

	for i, holderType := range []ledgerv1.HolderType{
		ledgerv1.HolderType_HOLDER_TYPE_USER,
		ledgerv1.HolderType_HOLDER_TYPE_SYSTEM,
		ledgerv1.HolderType_HOLDER_TYPE_PROVIDER,
	} {
		holder := createHolderWithType(t, env,
			[]string{"user-1", "system-fees", "stripe"}[i], holderType)
		if holder.GetType() != holderType || holder.GetExternalRef() == "" {
			t.Fatalf("unexpected holder: %v", holder)
		}
	}
}

func TestHolderTypeValidation(t *testing.T) {
	env := setup(t)

	_, err := env.provision.CreateHolder(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.CreateHolderRequest{
			Type:        ledgerv1.HolderType_HOLDER_TYPE_UNSPECIFIED,
			ExternalRef: "no-type",
		})))
	assertCode(t, err, connect.CodeInvalidArgument)
}

func TestDuplicateHolderExternalRefRejected(t *testing.T) {
	env := setup(t)
	createHolderWithType(t, env, "dup-ref", ledgerv1.HolderType_HOLDER_TYPE_USER)

	_, err := env.provision.CreateHolder(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.CreateHolderRequest{
			Type:        ledgerv1.HolderType_HOLDER_TYPE_PROVIDER,
			ExternalRef: "dup-ref",
		})))
	assertCode(t, err, connect.CodeAlreadyExists)
}

func TestCreateWallet(t *testing.T) {
	env := setup(t)
	holder := createHolder(t, env, "wallet-owner")

	wallet := createWallet(t, env, holder.GetId(), "main")
	if wallet.GetHolderId() != holder.GetId() || wallet.GetName() != "main" {
		t.Fatalf("unexpected wallet: %v", wallet)
	}
}

func TestCreateWalletUnknownHolder(t *testing.T) {
	env := setup(t)

	_, err := env.provision.CreateWallet(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.CreateWalletRequest{
			HolderId: "holder-does-not-exist",
			Name:     "main",
		})))
	assertCode(t, err, connect.CodeNotFound)
}

func TestCreateAccountDefaultsAndBalances(t *testing.T) {
	env := setup(t)
	holder := createHolder(t, env, "acct-holder")
	wallet := createWallet(t, env, holder.GetId(), "main")
	usd := createAsset(t, env, "USD", 2)
	eur := createAsset(t, env, "EUR", 2)

	// Unset allow_negative defaults to false.
	account, err := env.provision.CreateAccount(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.CreateAccountRequest{
			WalletId: wallet.GetId(),
			AssetId:  usd.GetId(),
		})))
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
	balances, err := env.provision.GetWalletBalances(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.GetWalletBalancesRequest{WalletId: wallet.GetId()})))
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
	usd := createAsset(t, env, "USD", 2)

	createAccount(t, env, wallet.GetId(), usd.GetId(), false)

	_, err := env.provision.CreateAccount(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.CreateAccountRequest{
			WalletId: wallet.GetId(),
			AssetId:  usd.GetId(),
		})))
	assertCode(t, err, connect.CodeAlreadyExists)

	// A different wallet may hold an account for the same asset.
	other := createWallet(t, env, holder.GetId(), "second")
	createAccount(t, env, other.GetId(), usd.GetId(), false)
}

func TestCreateAccountUnknownReferences(t *testing.T) {
	env := setup(t)
	holder := createHolder(t, env, "ref-holder")
	wallet := createWallet(t, env, holder.GetId(), "main")
	usd := createAsset(t, env, "USD", 2)

	// Unknown asset.
	_, err := env.provision.CreateAccount(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.CreateAccountRequest{
			WalletId: wallet.GetId(),
			AssetId:  "asset-does-not-exist",
		})))
	assertCode(t, err, connect.CodeNotFound)

	// Unknown wallet.
	_, err = env.provision.CreateAccount(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.CreateAccountRequest{
			WalletId: "wallet-does-not-exist",
			AssetId:  usd.GetId(),
		})))
	assertCode(t, err, connect.CodeNotFound)
}

func TestGetWalletBalancesUnknownWallet(t *testing.T) {
	env := setup(t)

	_, err := env.provision.GetWalletBalances(context.Background(), authed(t,
		connect.NewRequest(&ledgerv1.GetWalletBalancesRequest{WalletId: "nope"})))
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
	wrong := connect.NewRequest(&ledgerv1.ListAssetsRequest{})
	wrong.Header().Set("X-Api-Key", "wrong-key")
	_, err := env.provision.ListAssets(context.Background(), wrong)
	assertCode(t, err, connect.CodeUnauthenticated)

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
