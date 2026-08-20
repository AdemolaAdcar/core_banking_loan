package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/store"
)

// Tx implements internal/store.Tx: every method here runs inside the
// single pgx.Tx opened by Store.WithinTx. Deliberately has no
// UpdateJournalEntry or DeleteJournalEntry method -- invariant 3.
type Tx struct {
	tx pgx.Tx
}

// CreateJournalEntry inserts the entry row and every line row inside
// this transaction. The database's own deferred constraint trigger
// (see migrations/0001_init.up.sql) re-validates invariant 1
// independently at COMMIT time, regardless of what
// domain.NewJournalEntry already checked in memory -- this method does
// not skip that check by construction; it can't, the trigger fires on
// every INSERT to journal_entry_lines unconditionally.
func (t *Tx) CreateJournalEntry(ctx context.Context, e domain.JournalEntry) error {
	var metadataJSON []byte
	if e.Metadata != nil {
		var err error
		metadataJSON, err = json.Marshal(e.Metadata)
		if err != nil {
			return fmt.Errorf("postgres: marshaling metadata: %w", err)
		}
	}

	_, err := t.tx.Exec(ctx,
		`INSERT INTO journal_entries (
			id, source_event_id, posting_rule_code, posting_rule_version, loan_account_id,
			posted_at, period_id, is_prior_period_adjustment, adjustment_for_period_id,
			reversal_of_source_event_id, metadata
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		e.ID, e.SourceEventID, e.PostingRuleCode, e.PostingRuleVersion, nullableString(e.LoanAccountID),
		e.PostedAt, e.PeriodID, e.IsPriorPeriodAdjustment, e.AdjustmentForPeriodID,
		e.ReversalOfSourceEventID, metadataJSON,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		// 23505 = unique_violation. journal_entries has exactly one
		// UNIQUE constraint (source_event_id) besides its primary key,
		// so any unique violation here is that one -- the concurrent
		// idempotency-key race this sentinel exists for.
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return store.ErrDuplicateSourceEventID
		}
		return fmt.Errorf("postgres: inserting journal entry: %w", err)
	}

	for i, l := range e.Lines {
		_, err := t.tx.Exec(ctx,
			`INSERT INTO journal_entry_lines (journal_entry_id, line_order, gl_account, direction, amount, currency, running_balance_after)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			e.ID, i, l.GLAccount, string(l.Direction), l.Amount.Amount, l.Amount.Currency, l.RunningBalanceAfter.Amount,
		)
		if err != nil {
			return fmt.Errorf("postgres: inserting journal entry line %d: %w", i, err)
		}
	}
	return nil
}

func (t *Tx) ClosePeriod(ctx context.Context, p domain.Period) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO periods (id, status, closed_at, closed_by) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, closed_at = EXCLUDED.closed_at, closed_by = EXCLUDED.closed_by`,
		p.ID, string(p.Status), p.ClosedAt, p.ClosedBy,
	)
	if err != nil {
		return fmt.Errorf("postgres: closing period: %w", err)
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

func (t *Tx) InsertOutboxEntry(ctx context.Context, e outbox.Entry) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO outbox (id, topic, payload_json, created_at) VALUES ($1,$2,$3,$4)`,
		e.ID, e.Topic, e.PayloadJSON, e.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: inserting outbox entry: %w", err)
	}
	return nil
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
