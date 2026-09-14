package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/use-fabrica/ledger/internal/config"
	"github.com/use-fabrica/ledger/internal/rpc"
	ledgerv1connect "github.com/use-fabrica/ledger/proto/ledger/v1/ledgerv1connect"
)

func main() {
	fx.New(
		fx.Provide(
			config.Load,
			newLogger,
			newPool,
			rpc.NewHealthHandler,
		),
		fx.Invoke(serve),
	).Run()
}

func newLogger() (*zap.Logger, error) {
	return zap.NewProduction()
}

// newPool builds a pgxpool without an initial connectivity check: the server
// must boot (and answer liveness) even when Postgres is unreachable.
// Readiness reports the true state via pool.Ping.
func newPool(cfg config.Config) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("ledger: create pool: %w", err)
	}
	return pool, nil
}

func serve(lc fx.Lifecycle, cfg config.Config, log *zap.Logger, pool *pgxpool.Pool, health *rpc.HealthHandler) {
	// The health service is public: no auth middleware is wired at this layer,
	// so orchestrators can always probe liveness/readiness.
	mux := http.NewServeMux()
	mux.Handle(ledgerv1connect.NewHealthServiceHandler(health))

	server := &http.Server{Addr: cfg.HTTPAddr, Handler: mux}

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			go func() {
				log.Info("ledger listening", zap.String("addr", cfg.HTTPAddr))
				if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
					log.Error("http server error", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			err := server.Shutdown(ctx)
			pool.Close()
			_ = log.Sync()
			return err
		},
	})
}
