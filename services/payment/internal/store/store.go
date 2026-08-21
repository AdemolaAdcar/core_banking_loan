// Package store defines the persistence contract internal/service
// depends on. The concrete Postgres implementation lives in
// internal/store/postgres; nothing outside that subpackage imports pgx
// directly.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/outbox"
)

// Tx is a single atomic unit of work: a business write plus its outbox
// entry, committed or rolled back together (transactional outbox
// pattern).
type Tx interface {
	outbox.Inserter

	SavePaymentInstruction(ctx context.Context, p domain.PaymentInstruction) error
	SaveReconciliationException(ctx context.Context, e domain.ReconciliationException) error
	SaveIdempotentResponse(ctx context.Context, idempotencyKey, requestHash string, responseJSON []byte) error
	SetInboundCursor(ctx context.Context, name string, at time.Time) error
}

// Store is the top-level persistence dependency internal/service takes.
// WithinTx is the only way callers can write.
type Store interface {
	WithinTx(ctx context.Context, fn func(Tx) error) error

	GetPaymentInstruction(ctx context.Context, instructionID string) (domain.PaymentInstruction, error)
	GetPaymentInstructionByRailReference(ctx context.Context, railReference string) (domain.PaymentInstruction, bool, error)

	// ListSubmittedOutbound backs a periodic reconciliation sweep
	// (cmd/payment-service) that polls railclient.Client.Confirm for
	// every OUTBOUND instruction still awaiting a terminal outcome —
	// always a live query over current PaymentInstruction state, never
	// a separately maintained queue.
	ListSubmittedOutbound(ctx context.Context) ([]domain.PaymentInstruction, error)

	// GetInboundCursor / the Tx-side SetInboundCursor back
	// ReceiveInbound's `since` parameter — a named, persisted cursor so
	// a process restart doesn't replay (or skip) an inbound batch pull.
	GetInboundCursor(ctx context.Context, name string) (time.Time, bool, error)

	GetIdempotentResponse(ctx context.Context, idempotencyKey string) (found bool, responseJSON []byte, err error)

	ListUnpublished(ctx context.Context, limit int) ([]outbox.Entry, error)
	MarkPublished(ctx context.Context, ids []string) error
}

var ErrNotFound = errors.New("store: resource not found")
