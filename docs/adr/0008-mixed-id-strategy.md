# Mixed ID strategy: UUIDv7 for journal tables, CUID2 for API-facing entities

Primary keys use two schemes by table role. Hot journal tables — `transactions` and `entries` — use UUIDv7 (app-generated via `google/uuid`), stored as native 16-byte `uuid`: time-ordered keys keep B-tree inserts append-mostly on the highest-volume tables. API-facing entity tables — `holders`, `wallets`, `accounts`, `assets` — use CUID2 (`nrednav/cuid2`), stored as text: unguessable, URL-safe identifiers at the API edge.

Uniform UUIDv7 was considered and recommended, but rejected in favor of unguessability on any identifier that crosses the service boundary. The accepted costs: two Go ID types and two validation paths, and CUID2's random insert order (harmless on these cold tables). Unguessable provider/webhook correlation is *not* a job for primary keys — that remains the `reference` column on transactions.
