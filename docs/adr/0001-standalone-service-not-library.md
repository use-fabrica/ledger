# Standalone service, not an embeddable library

Ledger ships as a standalone network service that apps integrate with over the wire, rather than as a Go library imported into each app. We considered library-first (importable Go module pointed at the app's own database), but a service keeps the ledger's invariants and data ownership in one place and lets non-Go apps integrate. A side effect of this choice: nothing needs to compile into consumers, so the storage layer can be Postgres-specific without worrying about embedded databases like SQLite.
