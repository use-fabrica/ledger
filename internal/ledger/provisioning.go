// Provisioning delegation methods: the thin pass-throughs that let the
// RPC layer keep a single dependency on the engine. These methods run
// plain single-statement reads and writes — no multi-statement
// invariants — but they stay on the engine so internal/store remains an
// internal detail of this package.
package ledger

import (
	"context"
	"fmt"

	"github.com/use-fabrica/ledger/internal/store"
)

// CreateAsset defines a new asset. The unique code constraint is enforced
// by the schema; callers map the resulting Postgres error.
func (e *Engine) CreateAsset(ctx context.Context, id, code string, precision int32, metadata []byte) (store.Asset, error) {
	asset, err := e.q.CreateAsset(ctx, store.CreateAssetParams{
		ID:        id,
		Code:      code,
		Precision: precision,
		Metadata:  metadata,
	})
	if err != nil {
		return store.Asset{}, fmt.Errorf("ledger: create asset: %w", err)
	}
	return asset, nil
}

// ListAssets returns every defined asset.
func (e *Engine) ListAssets(ctx context.Context) ([]store.Asset, error) {
	assets, err := e.q.ListAssets(ctx)
	if err != nil {
		return nil, fmt.Errorf("ledger: list assets: %w", err)
	}
	return assets, nil
}

// CreateHolder registers a holder under the host app's external
// reference. The unique external_ref constraint is enforced by the
// schema; callers map the resulting Postgres error.
func (e *Engine) CreateHolder(ctx context.Context, id, holderType, externalRef string, metadata []byte) (store.Holder, error) {
	holder, err := e.q.CreateHolder(ctx, store.CreateHolderParams{
		ID:          id,
		Type:        holderType,
		ExternalRef: externalRef,
		Metadata:    metadata,
	})
	if err != nil {
		return store.Holder{}, fmt.Errorf("ledger: create holder: %w", err)
	}
	return holder, nil
}

// CreateWallet opens a wallet for an existing holder. The holder foreign
// key is enforced by the schema; callers map the resulting Postgres
// error.
func (e *Engine) CreateWallet(ctx context.Context, id, holderID, name string, metadata []byte) (store.Wallet, error) {
	wallet, err := e.q.CreateWallet(ctx, store.CreateWalletParams{
		ID:       id,
		HolderID: holderID,
		Name:     name,
		Metadata: metadata,
	})
	if err != nil {
		return store.Wallet{}, fmt.Errorf("ledger: create wallet: %w", err)
	}
	return wallet, nil
}

// GetWallet loads one wallet by ID; a miss is pgx.ErrNoRows so callers
// can distinguish "absent" from a failure.
func (e *Engine) GetWallet(ctx context.Context, walletID string) (store.Wallet, error) {
	wallet, err := e.q.GetWallet(ctx, walletID)
	if err != nil {
		return store.Wallet{}, fmt.Errorf("ledger: get wallet: %w", err)
	}
	return wallet, nil
}
