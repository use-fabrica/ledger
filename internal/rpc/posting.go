package rpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/use-fabrica/ledger/internal/ledger"
	ledgerv1 "github.com/use-fabrica/ledger/proto/ledger/v1"
)

// PostingHandler implements ledger.v1.PostingService: the host app's
// value-movement surface. It delegates every balance mutation to the
// posting engine in internal/ledger and never touches the store directly.
//
// Error mapping, chosen to distinguish client-fixable request problems
// from system-state problems:
//   - invalid_argument: missing idempotency key, malformed amounts, fewer
//     than two entries, zero amounts, unknown account in a request is NOT
//     here — unknown account is not_found; precision violations and
//     non-zero-sum entries are rejected as invalid_argument (the request
//     can be fixed and retried with a new key).
//   - failed_precondition: the request is well-formed but the current
//     balances forbid it — an overdraft on an account that is not flagged
//     allow_negative.
//   - not_found: a referenced account or transaction does not exist.
//   - already_exists: unused for now; idempotent replays succeed with the
//     original transaction instead of conflicting.
type PostingHandler struct {
	engine *ledger.Engine
}

func NewPostingHandler(engine *ledger.Engine) *PostingHandler {
	return &PostingHandler{engine: engine}
}

func (h *PostingHandler) CreateTransaction(
	ctx context.Context,
	req *connect.Request[ledgerv1.CreateTransactionRequest],
) (*connect.Response[ledgerv1.Transaction], error) {
	msg := req.Msg
	if msg.GetIdempotencyKey() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("rpc: idempotency_key is required"))
	}
	if len(msg.GetEntries()) < 2 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("rpc: at least two entries are required"))
	}
	entries := make([]ledger.PostEntry, 0, len(msg.GetEntries()))
	for _, entry := range msg.GetEntries() {
		if entry.GetAccountId() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errors.New("rpc: every entry needs an account_id"))
		}
		amount, err := decimal.NewFromString(entry.GetAmount())
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errors.New("rpc: entry amounts must be decimal strings"))
		}
		entries = append(entries, ledger.PostEntry{
			AccountID: entry.GetAccountId(),
			Amount:    amount,
		})
	}

	posted, err := h.engine.Post(ctx, ledger.PostParams{
		IdempotencyKey: msg.GetIdempotencyKey(),
		Reference:      msg.GetReference(),
		Entries:        entries,
	})
	if err != nil {
		return nil, mapPostError(err)
	}
	return connect.NewResponse(postedToProto(posted)), nil
}

func (h *PostingHandler) GetTransaction(
	ctx context.Context,
	req *connect.Request[ledgerv1.GetTransactionRequest],
) (*connect.Response[ledgerv1.Transaction], error) {
	id, err := uuid.Parse(req.Msg.GetId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("rpc: id must be a UUID"))
	}
	posted, err := h.engine.GetTransaction(ctx, id)
	if err != nil {
		return nil, mapPostError(err)
	}
	return connect.NewResponse(postedToProto(posted)), nil
}

// mapPostError translates posting engine sentinel errors to Connect codes;
// anything else is an internal failure. See the handler doc comment for
// the rationale behind each mapping.
func mapPostError(err error) error {
	switch {
	case errors.Is(err, ledger.ErrMissingIdempotencyKey),
		errors.Is(err, ledger.ErrTooFewEntries),
		errors.Is(err, ledger.ErrZeroAmount),
		errors.Is(err, ledger.ErrPrecisionExceeded),
		errors.Is(err, ledger.ErrNonZeroSum):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, ledger.ErrOverdraft):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, ledger.ErrAccountNotFound),
		errors.Is(err, ledger.ErrTransactionNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

// postedToProto renders an engine result as the wire Transaction, entries
// included. Replay results render identically to first writes — the
// idempotency contract is that a replay is indistinguishable from the
// original response.
func postedToProto(p ledger.PostedTransaction) *ledgerv1.Transaction {
	tx := p.Transaction
	proto := &ledgerv1.Transaction{
		Id:             uuid.UUID(tx.ID.Bytes).String(),
		IdempotencyKey: tx.IdempotencyKey,
		Status:         transactionStatusToProto(tx.Status),
		CreatedAt:      timestamppb.New(tx.CreatedAt.Time),
		Entries:        make([]*ledgerv1.Entry, 0, len(p.Entries)),
	}
	if tx.Reference.Valid {
		proto.Reference = &tx.Reference.String
	}
	if tx.PostedAt.Valid {
		proto.PostedAt = timestamppb.New(tx.PostedAt.Time)
	}
	for _, entry := range p.Entries {
		proto.Entries = append(proto.Entries, &ledgerv1.Entry{
			Id:            uuid.UUID(entry.ID.Bytes).String(),
			TransactionId: uuid.UUID(entry.TransactionID.Bytes).String(),
			AccountId:     entry.AccountID,
			Amount:        entry.Amount.String(),
			CreatedAt:     timestamppb.New(entry.CreatedAt.Time),
		})
	}
	return proto
}

func transactionStatusToProto(status string) ledgerv1.TransactionStatus {
	switch status {
	case "pending":
		return ledgerv1.TransactionStatus_TRANSACTION_STATUS_PENDING
	case "posted":
		return ledgerv1.TransactionStatus_TRANSACTION_STATUS_POSTED
	case "voided":
		return ledgerv1.TransactionStatus_TRANSACTION_STATUS_VOIDED
	default:
		return ledgerv1.TransactionStatus_TRANSACTION_STATUS_UNSPECIFIED
	}
}
