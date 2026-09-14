package rpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/use-fabrica/ledger/internal/ledger"
	ledgerv1 "github.com/use-fabrica/ledger/proto/ledger/v1"
)

// Pagination bounds for the audit list endpoints. A zero page_size means
// the default; anything above the maximum is clamped rather than rejected
// — an audit consumer asking for 10,000 rows gets a usable page, not an
// error. See audit.proto for why offset pagination is the right shape
// here.
const (
	defaultAuditPageSize int32 = 50
	maxAuditPageSize     int32 = 100
)

// AuditHandler implements ledger.v1.AuditService: the read-only
// reconciliation surface (entry history per account, transaction history
// per wallet, exact transaction lookups). It delegates every read to the
// posting engine and never touches the store directly.
//
// Error mapping:
//   - invalid_argument: a missing account_id / wallet_id / lookup key.
//   - not_found: the account or wallet being listed does not exist, or a
//     lookup key matches no transaction.
//   - everything else: internal.
type AuditHandler struct {
	engine *ledger.Engine
}

func NewAuditHandler(engine *ledger.Engine) *AuditHandler {
	return &AuditHandler{engine: engine}
}

func (h *AuditHandler) ListEntries(
	ctx context.Context,
	req *connect.Request[ledgerv1.ListEntriesRequest],
) (*connect.Response[ledgerv1.ListEntriesResponse], error) {
	accountID := req.Msg.GetAccountId()
	if accountID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("rpc: account_id is required"))
	}
	entries, info, err := h.engine.ListEntries(ctx, accountID, auditPage(req.Msg.GetPageSize(), req.Msg.GetPageOffset()))
	if err != nil {
		return nil, mapAuditError(err)
	}
	protoEntries := make([]*ledgerv1.Entry, 0, len(entries))
	for _, entry := range entries {
		protoEntries = append(protoEntries, &ledgerv1.Entry{
			Id:            uuid.UUID(entry.ID.Bytes).String(),
			TransactionId: uuid.UUID(entry.TransactionID.Bytes).String(),
			AccountId:     entry.AccountID,
			Amount:        entry.Amount.String(),
			CreatedAt:     timestamppb.New(entry.CreatedAt.Time),
		})
	}
	return connect.NewResponse(&ledgerv1.ListEntriesResponse{
		Entries:        protoEntries,
		NextPageOffset: nextPageOffset(info),
	}), nil
}

func (h *AuditHandler) ListWalletTransactions(
	ctx context.Context,
	req *connect.Request[ledgerv1.ListWalletTransactionsRequest],
) (*connect.Response[ledgerv1.ListWalletTransactionsResponse], error) {
	walletID := req.Msg.GetWalletId()
	if walletID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("rpc: wallet_id is required"))
	}
	transactions, info, err := h.engine.ListWalletTransactions(ctx, walletID, auditPage(req.Msg.GetPageSize(), req.Msg.GetPageOffset()))
	if err != nil {
		return nil, mapAuditError(err)
	}
	protoTransactions := make([]*ledgerv1.Transaction, 0, len(transactions))
	for _, transaction := range transactions {
		protoTransactions = append(protoTransactions, postedToProto(transaction))
	}
	return connect.NewResponse(&ledgerv1.ListWalletTransactionsResponse{
		Transactions:   protoTransactions,
		NextPageOffset: nextPageOffset(info),
	}), nil
}

func (h *AuditHandler) GetTransactionByIdempotencyKey(
	ctx context.Context,
	req *connect.Request[ledgerv1.GetTransactionByIdempotencyKeyRequest],
) (*connect.Response[ledgerv1.Transaction], error) {
	key := req.Msg.GetIdempotencyKey()
	if key == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("rpc: idempotency_key is required"))
	}
	transaction, err := h.engine.GetTransactionByIdempotencyKey(ctx, key)
	if err != nil {
		return nil, mapAuditError(err)
	}
	return connect.NewResponse(postedToProto(transaction)), nil
}

func (h *AuditHandler) GetTransactionByReference(
	ctx context.Context,
	req *connect.Request[ledgerv1.GetTransactionByReferenceRequest],
) (*connect.Response[ledgerv1.Transaction], error) {
	reference := req.Msg.GetReference()
	if reference == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("rpc: reference is required"))
	}
	transaction, err := h.engine.GetTransactionByReference(ctx, reference)
	if err != nil {
		return nil, mapAuditError(err)
	}
	return connect.NewResponse(postedToProto(transaction)), nil
}

// auditPage normalizes a wire page request: zero means the default size,
// anything above the maximum is clamped. Values fit int32 after clamping.
// The engine fetches one extra row beyond Limit to detect a further page.
func auditPage(size, offset uint32) ledger.Page {
	if size == 0 || size > uint32(maxAuditPageSize) {
		size = uint32(defaultAuditPageSize)
	}
	return ledger.Page{Limit: int32(size) + 1, Offset: int32(offset)}
}

// nextPageOffset renders the engine's page info for the wire; nil stays
// nil so the field is simply absent on the last page.
func nextPageOffset(info ledger.PageInfo) *uint32 {
	if info.NextOffset == nil {
		return nil
	}
	next := uint32(*info.NextOffset)
	return &next
}

// mapAuditError translates audit engine sentinel errors to Connect codes.
// Listing or looking up through a missing account or wallet, and a lookup
// key matching no transaction, are all not_found; everything the posting
// error mapper already knows keeps its existing mapping.
func mapAuditError(err error) error {
	if errors.Is(err, ledger.ErrWalletNotFound) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return mapPostError(err)
}
