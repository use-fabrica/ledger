# Postgres as the only storage backend

Postgres is the only supported database, used directly with no storage abstraction layer. Ledger correctness depends on transactional guarantees — multi-statement transactions, row locking (`SELECT ... FOR UPDATE`), constraints — and a pluggable-storage abstraction would force the design to the lowest common denominator across backends. If a second backend is ever needed (e.g. SQLite for embedded use), that will be a deliberate future decision, not an accident of v1 architecture.
