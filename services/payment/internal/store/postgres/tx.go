package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/outbox"
)

// Tx implements internal/store.Tx: every method here runs inside the
// single pgx.Tx opened by Store.WithinTx — the business write and its
// outbox.Entry always commit or roll back together (transactional
// outbox pattern).
type Tx struct {
	tx pgx.Tx
}

func (t *Tx) SavePaymentInstruction(ctx context.Context, p domain.PaymentInstruction) error {
	var failureReason *string
	if p.FailureReason != nil {
		s := string(*p.FailureReason)
		failureReason = &s
	}
	_, err := t.tx.Exec(ctx,
		`INSERT INTO payment_instructions (id, loan_account_id, direction, purpose, amount, currency, party_id, journal_entry_id, status, rail, rail_reference, failure_reason, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		 ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status, rail = EXCLUDED.rail, rail_reference = EXCLUDED.rail_reference,
			journal_entry_id = EXCLUDED.journal_entry_id, failure_reason = EXCLUDED.failure_reason, updated_at = EXCLUDED.updated_at`,
		p.InstructionID, p.LoanAccountID, string(p.Direction), string(p.Purpose), p.Amount.Amount, p.Amount.Currency,
		p.PartyID, p.JournalEntryID, string(p.Status), p.Rail, p.RailReference, failureReason, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving payment instruction: %w", err)
	}
	return nil
}

func (t *Tx) SaveReconciliationException(ctx context.Context, e domain.ReconciliationException) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO reconciliation_exceptions (id, kind, rail_reference, rail, details, occurred_at, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		e.ExceptionID, string(e.Kind), e.RailReference, e.Rail, e.Details, e.OccurredAt, e.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving reconciliation exception: %w", err)
	}
	return nil
}

func (t *Tx) SaveIdempotentResponse(ctx context.Context, idempotencyKey, requestHash string, responseJSON []byte) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO idempotency_keys (idempotency_key, request_hash, response_json) VALUES ($1,$2,$3)
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		idempotencyKey, requestHash, responseJSON,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving idempotent response: %w", err)
	}
	return nil
}

func (t *Tx) SetInboundCursor(ctx context.Context, name string, at time.Time) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO inbound_cursors (name, cursor_at) VALUES ($1,$2)
		 ON CONFLICT (name) DO UPDATE SET cursor_at = EXCLUDED.cursor_at`,
		name, at,
	)
	if err != nil {
		return fmt.Errorf("postgres: setting inbound cursor: %w", err)
	}
	return nil
}

func (t *Tx) InsertOutboxEntry(ctx context.Context, e outbox.Entry) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO outbox (id, topic, payload_json, created_at) VALUES ($1,$2,$3,$4)`, e.ID, e.Topic, e.PayloadJSON, e.CreatedAt)
	if err != nil {
		return fmt.Errorf("postgres: inserting outbox entry: %w", err)
	}
	return nil
}
