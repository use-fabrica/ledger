package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/use-fabrica/ledger/db/migrations"
	"github.com/use-fabrica/ledger/internal/config"
)

const commandTimeout = 60 * time.Second

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("usage: migrate <up|down|status|version> [args...]")
	}

	// The migrate deploy step runs without server secrets (no API key in
	// scope); only the database URL is needed.
	cfg, err := config.LoadBase()
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("migrate: open database: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		log.Fatalf("migrate: set dialect: %v", err)
	}

	command := os.Args[1]
	args := os.Args[2:]
	if err := goose.RunContext(ctx, command, db, ".", args...); err != nil {
		log.Fatalf("migrate: %v", err)
	}
}
