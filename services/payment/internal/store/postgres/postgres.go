// Package postgres is the only place in this service that imports pgx
// or touches SQL directly. It implements internal/store.Store /
// internal/store.Tx, so internal/service never depends on this package
// or on pgx at all.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/store"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) WithinTx(ctx context.Context, fn func(store.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: beginning transaction: %w", err)
	}
	ptx := &Tx{tx: tx}
	if fnErr := fn(ptx); fnErr != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return fmt.Errorf("postgres: rolling back after %w: %v", fnErr, rbErr)
		}
		return fnErr
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: committing transaction: %w", err)
	}
	return nil
}

func scanPaymentInstruction(row pgx.Row) (domain.PaymentInstruction, error) {
	var p domain.PaymentInstruction
	var direction, purpose, status string
	var amount int64
	var currency string
	var failureReason *string
	err := row.Scan(
		&p.InstructionID, &p.LoanAccountID, &direction, &purpose, &amount, &currency,
		&p.PartyID, &p.JournalEntryID, &status, &p.Rail, &p.RailReference, &failureReason,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PaymentInstruction{}, store.ErrNotFound
	}
	if err != nil {
		return domain.PaymentInstruction{}, fmt.Errorf("postgres: querying payment instruction: %w", err)
	}
	p.Direction = domain.Direction(direction)
	p.Purpose = domain.Purpose(purpose)
	p.Amount = domain.Money{Amount: amount, Currency: currency}
	p.Status = domain.Status(status)
	if failureReason != nil {
		reason := domain.FailureReason(*failureReason)
		p.FailureReason = &reason
	}
	return p, nil
}

const paymentInstructionColumns = `id, loan_account_id, direction, purpose, amount, currency,
	party_id, journal_entry_id, status, rail, rail_reference, failure_reason, created_at, updated_at`

func (s *Store) GetPaymentInstruction(ctx context.Context, instructionID string) (domain.PaymentInstruction, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+paymentInstructionColumns+" FROM payment_instructions WHERE id = $1", instructionID)
	return scanPaymentInstruction(row)
}

func (s *Store) GetPaymentInstructionByRailReference(ctx context.Context, railReference string) (domain.PaymentInstruction, bool, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+paymentInstructionColumns+" FROM payment_instructions WHERE rail_reference = $1", railReference)
	p, err := scanPaymentInstruction(row)
	if errors.Is(err, store.ErrNotFound) {
		return domain.PaymentInstruction{}, false, nil
	}
	if err != nil {
		return domain.PaymentInstruction{}, false, err
	}
	return p, true, nil
}

func (s *Store) ListSubmittedOutbound(ctx context.Context) ([]domain.PaymentInstruction, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+paymentInstructionColumns+" FROM payment_instructions WHERE direction = 'OUTBOUND' AND status = 'Submitted' ORDER BY created_at ASC")
	if err != nil {
		return nil, fmt.Errorf("postgres: querying submitted outbound instructions: %w", err)
	}
	defer rows.Close()
	var out []domain.PaymentInstruction
	for rows.Next() {
		p, err := scanPaymentInstruction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetInboundCursor(ctx context.Context, name string) (time.Time, bool, error) {
	row := s.pool.QueryRow(ctx, "SELECT cursor_at FROM inbound_cursors WHERE name = $1", name)
	var at time.Time
	err := row.Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("postgres: querying inbound cursor: %w", err)
	}
	return at, true, nil
}

func (s *Store) GetIdempotentResponse(ctx context.Context, idempotencyKey string) (bool, []byte, error) {
	row := s.pool.QueryRow(ctx, "SELECT response_json FROM idempotency_keys WHERE idempotency_key = $1", idempotencyKey)
	var payload []byte
	err := row.Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("postgres: querying idempotent response: %w", err)
	}
	return true, payload, nil
}

func (s *Store) ListUnpublished(ctx context.Context, limit int) ([]outbox.Entry, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT id, topic, payload_json, created_at FROM outbox WHERE published_at IS NULL ORDER BY created_at ASC LIMIT $1", limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying unpublished outbox entries: %w", err)
	}
	defer rows.Close()
	var out []outbox.Entry
	for rows.Next() {
		var e outbox.Entry
		if err := rows.Scan(&e.ID, &e.Topic, &e.PayloadJSON, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scanning outbox entry: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) MarkPublished(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, "UPDATE outbox SET published_at = now() WHERE id = ANY($1)", ids)
	if err != nil {
		return fmt.Errorf("postgres: marking outbox entries published: %w", err)
	}
	return nil
}
