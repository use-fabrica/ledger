package migrations

import "embed"

// FS holds the goose SQL migrations applied by cmd/migrate.
//
//go:embed *.sql
var FS embed.FS
