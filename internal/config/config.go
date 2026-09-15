// Package config loads service configuration from the environment.
package config

import (
	"fmt"
	"os"
)

// Config holds the runtime configuration for the ledger binaries.
// All values come from environment variables so the same binaries run
// unchanged across local, CI, and deployed environments.
type Config struct {
	// DatabaseURL is the Postgres DSN used by both cmd/ledger (via pgxpool)
	// and cmd/migrate (via goose).
	DatabaseURL string
	// HTTPAddr is the listen address for the Connect HTTP server.
	HTTPAddr string
	// APIKey gates every RPC except the health probes. It is deliberately a
	// middleware concern so mTLS (or any other transport identity) can
	// replace it without touching handlers.
	APIKey string
}

// Load reads the full runtime configuration from the environment,
// applying defaults where a variable is unset. It requires every server
// secret, including LEDGER_API_KEY; binaries that need no secrets (like
// cmd/migrate) use LoadBase instead.
func Load() (Config, error) {
	cfg, err := LoadBase()
	if err != nil {
		return Config{}, err
	}
	cfg.APIKey = os.Getenv("LEDGER_API_KEY")
	if cfg.APIKey == "" {
		return Config{}, fmt.Errorf("config: LEDGER_API_KEY is required")
	}
	return cfg, nil
}

// LoadBase reads the configuration every binary needs — the database URL
// and the HTTP listen address — without requiring server-only secrets.
// The migrate deploy step runs with no API key in scope, so it loads
// through here rather than Load.
func LoadBase() (Config, error) {
	cfg := Config{
		DatabaseURL: os.Getenv("LEDGER_DATABASE_URL"),
		HTTPAddr:    os.Getenv("LEDGER_HTTP_ADDR"),
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8080"
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("config: LEDGER_DATABASE_URL is required")
	}
	return cfg, nil
}
