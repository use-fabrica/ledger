# Ledger

A standalone service that apps use to record and query movements of value — money, credits, tokens, and virtual currencies — using double-entry bookkeeping. One deployment serves exactly one app.

## Language

**Asset**:
A type of value the ledger tracks (a fiat currency, credit, token, or virtual currency), defined by a code and a decimal precision.
_Avoid_: currency (too fiat-specific), coin

**Holder**:
A party that owns wallets: an end user of the host app, the operating platform itself, or an external payment provider. Identified only by the host app's opaque external reference; the ledger stores no identity data beyond that.
_Avoid_: user, customer

**Wallet**:
A container owned by one holder that groups that holder's accounts across assets.
_Avoid_: account (the wallet is the container, not the participant)

**Account**:
The double-entry participant: tracks value in exactly one asset within exactly one wallet, and is what entries debit or credit. May run a negative balance only if explicitly flagged (reserved for system accounts).
_Avoid_: wallet, sub-account

**Transaction**:
An atomic, append-only movement of value composed of two or more entries whose net effect per asset is zero. Created `pending` (funds earmarked) and later transitions to `posted` or `voided`. Carries a client-supplied idempotency key; mistakes after posting are corrected with a compensating transaction, never by editing history.
_Avoid_: payment, transfer

**Entry**:
A single signed amount applied to one account as part of a transaction. Immutable once written.
_Avoid_: posting, line item

**Balance**:
The materialized current total of an account's entries in one asset, split into posted and pending amounts, updated in the same database transaction as the entries it summarizes.
_Avoid_: wallet balance
