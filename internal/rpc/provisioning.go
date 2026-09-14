package rpc

import (
	"context"
	"encoding/json"
	"errors"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nrednav/cuid2"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/use-fabrica/ledger/internal/ledger"
	"github.com/use-fabrica/ledger/internal/store"
	ledgerv1 "github.com/use-fabrica/ledger/proto/ledger/v1"
)

// Postgres error codes we translate into Connect codes. Queried as string
// literals to avoid pulling a pgerrcode dependency for two constants.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
)

// ProvisioningHandler implements ledger.v1.ProvisioningService: the host
// app's setup surface (assets, holders, wallets, accounts, balance reads).
// Asset/holder/wallet writes go straight to the store; anything touching
// the balances table goes through the posting engine in internal/ledger.
type ProvisioningHandler struct {
	q      *store.Queries
	engine *ledger.Engine
}

func NewProvisioningHandler(pool *pgxpool.Pool, engine *ledger.Engine) *ProvisioningHandler {
	return &ProvisioningHandler{q: store.New(pool), engine: engine}
}

func (h *ProvisioningHandler) CreateAsset(
	ctx context.Context,
	req *connect.Request[ledgerv1.CreateAssetRequest],
) (*connect.Response[ledgerv1.Asset], error) {
	msg := req.Msg
	if msg.GetCode() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("rpc: code is required"))
	}
	if msg.GetPrecision() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("rpc: precision must be >= 0"))
	}
	metadata, err := metadataBytes(msg.GetMetadata())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	asset, err := h.q.CreateAsset(ctx, store.CreateAssetParams{
		ID:        cuid2.Generate(),
		Code:      msg.GetCode(),
		Precision: msg.GetPrecision(),
		Metadata:  metadata,
	})
	if err != nil {
		return nil, mapStoreError(err, violation{pgUniqueViolation, "assets_code_key",
			connect.CodeAlreadyExists, "asset code already exists"})
	}
	return connect.NewResponse(assetToProto(asset)), nil
}

func (h *ProvisioningHandler) ListAssets(
	ctx context.Context,
	_ *connect.Request[ledgerv1.ListAssetsRequest],
) (*connect.Response[ledgerv1.ListAssetsResponse], error) {
	rows, err := h.q.ListAssets(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	assets := make([]*ledgerv1.Asset, 0, len(rows))
	for _, row := range rows {
		assets = append(assets, assetToProto(row))
	}
	return connect.NewResponse(&ledgerv1.ListAssetsResponse{Assets: assets}), nil
}

func (h *ProvisioningHandler) CreateHolder(
	ctx context.Context,
	req *connect.Request[ledgerv1.CreateHolderRequest],
) (*connect.Response[ledgerv1.Holder], error) {
	msg := req.Msg
	holderType, ok := holderTypeToStore(msg.GetType())
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("rpc: type must be user, system, or provider"))
	}
	if msg.GetExternalRef() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("rpc: external_ref is required"))
	}
	metadata, err := metadataBytes(msg.GetMetadata())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	holder, err := h.q.CreateHolder(ctx, store.CreateHolderParams{
		ID:          cuid2.Generate(),
		Type:        holderType,
		ExternalRef: msg.GetExternalRef(),
		Metadata:    metadata,
	})
	if err != nil {
		return nil, mapStoreError(err, violation{pgUniqueViolation, "holders_external_ref_key",
			connect.CodeAlreadyExists, "holder external_ref already exists"})
	}
	return connect.NewResponse(holderToProto(holder)), nil
}

func (h *ProvisioningHandler) CreateWallet(
	ctx context.Context,
	req *connect.Request[ledgerv1.CreateWalletRequest],
) (*connect.Response[ledgerv1.Wallet], error) {
	msg := req.Msg
	if msg.GetHolderId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("rpc: holder_id is required"))
	}
	if msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("rpc: name is required"))
	}
	metadata, err := metadataBytes(msg.GetMetadata())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	wallet, err := h.q.CreateWallet(ctx, store.CreateWalletParams{
		ID:       cuid2.Generate(),
		HolderID: msg.GetHolderId(),
		Name:     msg.GetName(),
		Metadata: metadata,
	})
	if err != nil {
		return nil, mapStoreError(err, violation{pgForeignKeyViolation, "wallets_holder_id_fkey",
			connect.CodeNotFound, "holder not found"})
	}
	return connect.NewResponse(walletToProto(wallet)), nil
}

func (h *ProvisioningHandler) CreateAccount(
	ctx context.Context,
	req *connect.Request[ledgerv1.CreateAccountRequest],
) (*connect.Response[ledgerv1.Account], error) {
	msg := req.Msg
	if msg.GetWalletId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("rpc: wallet_id is required"))
	}
	if msg.GetAssetId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("rpc: asset_id is required"))
	}
	// allow_negative defaults to false via the optional proto field.
	account, err := h.engine.CreateAccount(ctx,
		cuid2.Generate(), msg.GetWalletId(), msg.GetAssetId(), msg.GetAllowNegative())
	if err != nil {
		return nil, mapStoreError(err,
			violation{pgUniqueViolation, "accounts_wallet_id_asset_id_key",
				connect.CodeAlreadyExists, "account already exists for this wallet and asset"},
			violation{pgForeignKeyViolation, "accounts_wallet_id_fkey",
				connect.CodeNotFound, "wallet not found"},
			violation{pgForeignKeyViolation, "accounts_asset_id_fkey",
				connect.CodeNotFound, "asset not found"},
		)
	}
	return connect.NewResponse(accountToProto(account)), nil
}

func (h *ProvisioningHandler) GetWalletBalances(
	ctx context.Context,
	req *connect.Request[ledgerv1.GetWalletBalancesRequest],
) (*connect.Response[ledgerv1.GetWalletBalancesResponse], error) {
	walletID := req.Msg.GetWalletId()
	if walletID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("rpc: wallet_id is required"))
	}
	if _, err := h.q.GetWallet(ctx, walletID); err != nil {
		return nil, mapStoreError(err, violation{pgNoRows, "",
			connect.CodeNotFound, "wallet not found"})
	}
	rows, err := h.engine.WalletBalances(ctx, walletID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	balances := make([]*ledgerv1.AccountBalance, 0, len(rows))
	for _, row := range rows {
		balances = append(balances, &ledgerv1.AccountBalance{
			Account: accountToProto(store.Account{
				ID:            row.ID,
				WalletID:      row.WalletID,
				AssetID:       row.AssetID,
				AllowNegative: row.AllowNegative,
				CreatedAt:     row.CreatedAt,
			}),
			Posted:  row.Posted.String(),
			Pending: row.Pending.String(),
		})
	}
	return connect.NewResponse(&ledgerv1.GetWalletBalancesResponse{
		WalletId: walletID,
		Balances: balances,
	}), nil
}

// pgNoRows marks the no-rows sentinel for mapStoreError.
const pgNoRows = "__no_rows__"

// violation maps a specific Postgres failure to a Connect error. code and
// constraint select the error; empty code matches pgx.ErrNoRows instead.
// An empty constraint matches any constraint with that code.
type violation struct {
	code        string
	constraint  string
	connectCode connect.Code
	message     string
}

func mapStoreError(err error, violations ...violation) error {
	if errors.Is(err, pgx.ErrNoRows) {
		for _, v := range violations {
			if v.code == pgNoRows {
				return connect.NewError(v.connectCode, errors.New("rpc: "+v.message))
			}
		}
		return connect.NewError(connect.CodeNotFound, err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		for _, v := range violations {
			if pgErr.Code == v.code && (v.constraint == "" || pgErr.ConstraintName == v.constraint) {
				return connect.NewError(v.connectCode, errors.New("rpc: "+v.message))
			}
		}
	}
	return connect.NewError(connect.CodeInternal, err)
}

// metadataBytes renders an optional Struct to compact JSON for the jsonb
// column; nil metadata stores the default empty object.
func metadataBytes(s *structpb.Struct) ([]byte, error) {
	if s == nil {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, errors.New("rpc: metadata must be a valid JSON object")
	}
	return b, nil
}

func metadataProto(b []byte) *structpb.Struct {
	if len(b) == 0 {
		return nil
	}
	s := &structpb.Struct{}
	if err := json.Unmarshal(b, s); err != nil {
		return nil
	}
	return s
}

func holderTypeToStore(t ledgerv1.HolderType) (string, bool) {
	switch t {
	case ledgerv1.HolderType_HOLDER_TYPE_USER:
		return "user", true
	case ledgerv1.HolderType_HOLDER_TYPE_SYSTEM:
		return "system", true
	case ledgerv1.HolderType_HOLDER_TYPE_PROVIDER:
		return "provider", true
	default:
		return "", false
	}
}

func holderTypeToProto(t string) ledgerv1.HolderType {
	switch t {
	case "user":
		return ledgerv1.HolderType_HOLDER_TYPE_USER
	case "system":
		return ledgerv1.HolderType_HOLDER_TYPE_SYSTEM
	case "provider":
		return ledgerv1.HolderType_HOLDER_TYPE_PROVIDER
	default:
		return ledgerv1.HolderType_HOLDER_TYPE_UNSPECIFIED
	}
}

func assetToProto(a store.Asset) *ledgerv1.Asset {
	return &ledgerv1.Asset{
		Id:        a.ID,
		Code:      a.Code,
		Precision: a.Precision,
		Metadata:  metadataProto(a.Metadata),
		CreatedAt: timestamppb.New(a.CreatedAt.Time),
	}
}

func holderToProto(h store.Holder) *ledgerv1.Holder {
	return &ledgerv1.Holder{
		Id:          h.ID,
		Type:        holderTypeToProto(h.Type),
		ExternalRef: h.ExternalRef,
		Metadata:    metadataProto(h.Metadata),
		CreatedAt:   timestamppb.New(h.CreatedAt.Time),
	}
}

func walletToProto(w store.Wallet) *ledgerv1.Wallet {
	return &ledgerv1.Wallet{
		Id:        w.ID,
		HolderId:  w.HolderID,
		Name:      w.Name,
		Metadata:  metadataProto(w.Metadata),
		CreatedAt: timestamppb.New(w.CreatedAt.Time),
	}
}

func accountToProto(a store.Account) *ledgerv1.Account {
	return &ledgerv1.Account{
		Id:            a.ID,
		WalletId:      a.WalletID,
		AssetId:       a.AssetID,
		AllowNegative: a.AllowNegative,
		CreatedAt:     timestamppb.New(a.CreatedAt.Time),
	}
}
