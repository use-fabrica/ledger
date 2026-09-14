// Package rpc hosts the Connect handlers for the ledger API surface.
// Handlers depend on the posting engine in internal/ledger, never on
// internal/store directly.
package rpc

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/emptypb"

	ledgerv1 "github.com/use-fabrica/ledger/proto/ledger/v1"
)

// HealthHandler implements ledger.v1.HealthService. It is public: no auth
// middleware may gate it, so orchestrators can always probe the service.
type HealthHandler struct {
	pool *pgxpool.Pool
}

// NewHealthHandler builds a HealthHandler that reports readiness based on
// pool connectivity. pool may be nil (readiness then reports NOT_SERVING).
func NewHealthHandler(pool *pgxpool.Pool) *HealthHandler {
	return &HealthHandler{pool: pool}
}

// CheckLiveness answers SERVING unconditionally: the process is up and able
// to serve requests regardless of dependency health.
func (h *HealthHandler) CheckLiveness(
	_ context.Context,
	_ *connect.Request[emptypb.Empty],
) (*connect.Response[ledgerv1.HealthCheckResponse], error) {
	return connect.NewResponse(&ledgerv1.HealthCheckResponse{
		Status: ledgerv1.ServingStatus_SERVING,
	}), nil
}

// CheckReadiness answers SERVING only when the database accepts a ping.
func (h *HealthHandler) CheckReadiness(
	ctx context.Context,
	_ *connect.Request[emptypb.Empty],
) (*connect.Response[ledgerv1.HealthCheckResponse], error) {
	if h.pool == nil {
		return notServing(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := h.pool.Ping(ctx); err != nil {
		return notServing(), nil
	}
	return connect.NewResponse(&ledgerv1.HealthCheckResponse{
		Status: ledgerv1.ServingStatus_SERVING,
	}), nil
}

func notServing() *connect.Response[ledgerv1.HealthCheckResponse] {
	return connect.NewResponse(&ledgerv1.HealthCheckResponse{
		Status: ledgerv1.ServingStatus_NOT_SERVING,
	})
}
