-- +goose Up
-- +goose StatementBegin
CREATE TABLE assets (
    id         text PRIMARY KEY,
    code       text NOT NULL UNIQUE,
    precision  integer NOT NULL,
    metadata   jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE holders (
    id           text PRIMARY KEY,
    type         text NOT NULL CHECK (type IN ('user', 'system', 'provider')),
    external_ref text NOT NULL UNIQUE,
    metadata     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE wallets (
    id         text PRIMARY KEY,
    holder_id  text NOT NULL REFERENCES holders (id),
    name       text NOT NULL,
    metadata   jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE accounts (
    id             text PRIMARY KEY,
    wallet_id      text NOT NULL REFERENCES wallets (id),
    asset_id       text NOT NULL REFERENCES assets (id),
    allow_negative boolean NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (wallet_id, asset_id)
);

CREATE TABLE balances (
    account_id text PRIMARY KEY REFERENCES accounts (id),
    posted     numeric NOT NULL DEFAULT 0,
    pending    numeric NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE transactions (
    id              uuid PRIMARY KEY,
    idempotency_key text NOT NULL UNIQUE,
    reference       text,
    status          text NOT NULL CHECK (status IN ('pending', 'posted', 'voided')),
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    posted_at       timestamptz,
    voided_at       timestamptz
);

CREATE TABLE entries (
    id             uuid PRIMARY KEY,
    transaction_id uuid NOT NULL REFERENCES transactions (id),
    account_id     text NOT NULL REFERENCES accounts (id),
    amount         numeric NOT NULL CHECK (amount <> 0),
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_transactions_reference  ON transactions (reference);
CREATE INDEX idx_entries_transaction_id  ON entries (transaction_id);
CREATE INDEX idx_entries_account_id      ON entries (account_id);
CREATE INDEX idx_wallets_holder_id       ON wallets (holder_id);
CREATE INDEX idx_accounts_wallet_id      ON accounts (wallet_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS entries;
DROP TABLE IF EXISTS transactions;
DROP TABLE IF EXISTS balances;
DROP TABLE IF EXISTS accounts;
DROP TABLE IF EXISTS wallets;
DROP TABLE IF EXISTS holders;
DROP TABLE IF EXISTS assets;
-- +goose StatementEnd
