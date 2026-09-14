# sqlc + pgx + goose for data access

Data access uses sqlc (generating type-safe Go from hand-written SQL) over the pgx/v5 driver, with goose for migrations. An ORM was deliberately rejected: in a ledger, every query that mutates balances must be explicit, reviewable SQL with visible transaction and locking semantics, which ORMs obscure. sqlc gives compile-time-checked queries without hiding the SQL. goose was chosen over golang-migrate because it is Go-native and embeds migrations via `embed.FS`, keeping migration execution inside our own binaries.
