// Package postgres is the only place in this service that imports pgx
// or touches SQL directly. It implements internal/store.Store /
// internal/store.Tx, so internal/service never depends on this package
// or on pgx at all. GL carries no PII -- unlike services/party and
// services/crm, there is no encryption boundary in this package; every
// column here is plain text/numeric.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/store"
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

const entryColumns = `id, source_event_id, posting_rule_code, posting_rule_version, loan_account_id,
	posted_at, period_id, is_prior_period_adjustment, adjustment_for_period_id, reversal_of_source_event_id, metadata`

func scanEntry(row pgx.Row) (domain.JournalEntry, error) {
	var e domain.JournalEntry
	var loanAccountID *string
	var adjustmentForPeriodID, reversalOfSourceEventID *string
	var metadataJSON []byte
	err := row.Scan(&e.ID, &e.SourceEventID, &e.PostingRuleCode, &e.PostingRuleVersion, &loanAccountID,
		&e.PostedAt, &e.PeriodID, &e.IsPriorPeriodAdjustment, &adjustmentForPeriodID, &reversalOfSourceEventID, &metadataJSON)
	if err != nil {
		return domain.JournalEntry{}, err
	}
	if loanAccountID != nil {
		e.LoanAccountID = *loanAccountID
	}
	e.AdjustmentForPeriodID = adjustmentForPeriodID
	e.ReversalOfSourceEventID = reversalOfSourceEventID
	if len(metadataJSON) > 0 {
		if err := json.Unmarshal(metadataJSON, &e.Metadata); err != nil {
			return domain.JournalEntry{}, fmt.Errorf("postgres: unmarshaling metadata: %w", err)
		}
	}
	return e, nil
}

func (s *Store) loadLines(ctx context.Context, journalEntryID string) ([]domain.JournalEntryLine, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT gl_account, direction, amount, currency, running_balance_after
		 FROM journal_entry_lines WHERE journal_entry_id = $1 ORDER BY line_order ASC`,
		journalEntryID)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying lines: %w", err)
	}
	defer rows.Close()

	var lines []domain.JournalEntryLine
	for rows.Next() {
		var glAccount, direction, currency string
		var amount, runningBalance int64
		if err := rows.Scan(&glAccount, &direction, &amount, &currency, &runningBalance); err != nil {
			return nil, fmt.Errorf("postgres: scanning line: %w", err)
		}
		lines = append(lines, domain.JournalEntryLine{
			Line: domain.Line{
				GLAccount: glAccount,
				Direction: domain.Direction(direction),
				Amount:    domain.Money{Amount: amount, Currency: currency},
			},
			RunningBalanceAfter: domain.Money{Amount: runningBalance, Currency: currency},
		})
	}
	return lines, rows.Err()
}

func (s *Store) GetJournalEntry(ctx context.Context, journalEntryID string) (domain.JournalEntry, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+entryColumns+" FROM journal_entries WHERE id = $1", journalEntryID)
	e, err := scanEntry(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.JournalEntry{}, store.ErrNotFound
	}
	if err != nil {
		return domain.JournalEntry{}, fmt.Errorf("postgres: querying entry: %w", err)
	}
	lines, err := s.loadLines(ctx, e.ID)
	if err != nil {
		return domain.JournalEntry{}, err
	}
	e.Lines = lines
	return e, nil
}

func (s *Store) FindBySourceEventID(ctx context.Context, sourceEventID string) (domain.JournalEntry, bool, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+entryColumns+" FROM journal_entries WHERE source_event_id = $1", sourceEventID)
	e, err := scanEntry(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.JournalEntry{}, false, nil
	}
	if err != nil {
		return domain.JournalEntry{}, false, fmt.Errorf("postgres: querying entry by source event id: %w", err)
	}
	lines, err := s.loadLines(ctx, e.ID)
	if err != nil {
		return domain.JournalEntry{}, false, err
	}
	e.Lines = lines
	return e, true, nil
}

// GetLatestRunningBalance is a live query -- see store.Store's doc
// comment on this method for why this is not invariant-6-violating.
func (s *Store) GetLatestRunningBalance(ctx context.Context, loanAccountID, glAccount, currency string) (domain.Money, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT jel.running_balance_after FROM journal_entry_lines jel
		 JOIN journal_entries je ON je.id = jel.journal_entry_id
		 WHERE je.loan_account_id = $1 AND jel.gl_account = $2
		 ORDER BY je.posted_at DESC, jel.line_order DESC LIMIT 1`,
		loanAccountID, glAccount)
	var balance int64
	err := row.Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Money{Amount: 0, Currency: currency}, nil
	}
	if err != nil {
		return domain.Money{}, fmt.Errorf("postgres: querying latest running balance: %w", err)
	}
	return domain.Money{Amount: balance, Currency: currency}, nil
}

func (s *Store) GetTrialBalance(ctx context.Context, asOf time.Time) ([]store.TrialBalanceLine, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT jel.gl_account,
			coalesce(sum(jel.amount) FILTER (WHERE jel.direction = 'DEBIT'), 0),
			coalesce(sum(jel.amount) FILTER (WHERE jel.direction = 'CREDIT'), 0),
			max(jel.currency)
		 FROM journal_entry_lines jel
		 JOIN journal_entries je ON je.id = jel.journal_entry_id
		 WHERE je.posted_at <= $1
		 GROUP BY jel.gl_account
		 ORDER BY jel.gl_account`,
		asOf)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying trial balance: %w", err)
	}
	defer rows.Close()

	var out []store.TrialBalanceLine
	for rows.Next() {
		var l store.TrialBalanceLine
		if err := rows.Scan(&l.GLAccount, &l.DebitTotal, &l.CreditTotal, &l.Currency); err != nil {
			return nil, fmt.Errorf("postgres: scanning trial balance line: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) GetStatementOfAccount(ctx context.Context, loanAccountID string, asOf time.Time) ([]store.StatementLine, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT je.id, je.posted_at, je.posting_rule_code, jel.gl_account, jel.direction, jel.amount, jel.currency, jel.running_balance_after
		 FROM journal_entry_lines jel
		 JOIN journal_entries je ON je.id = jel.journal_entry_id
		 WHERE je.loan_account_id = $1 AND je.posted_at <= $2
		 ORDER BY je.posted_at ASC, jel.line_order ASC`,
		loanAccountID, asOf)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying statement of account: %w", err)
	}
	defer rows.Close()

	var out []store.StatementLine
	for rows.Next() {
		var l store.StatementLine
		var direction, currency string
		var amount, runningBalance int64
		if err := rows.Scan(&l.JournalEntryID, &l.PostedAt, &l.PostingRuleCode, &l.GLAccount, &direction, &amount, &currency, &runningBalance); err != nil {
			return nil, fmt.Errorf("postgres: scanning statement line: %w", err)
		}
		l.Direction = domain.Direction(direction)
		l.Amount = domain.Money{Amount: amount, Currency: currency}
		l.RunningBalanceAfter = domain.Money{Amount: runningBalance, Currency: currency}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) GetAccountBalance(ctx context.Context, glAccountCode string, asOf time.Time) (domain.Money, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT
			coalesce(sum(jel.amount) FILTER (WHERE jel.direction = 'DEBIT'), 0) - coalesce(sum(jel.amount) FILTER (WHERE jel.direction = 'CREDIT'), 0),
			coalesce(max(jel.currency), '')
		 FROM journal_entry_lines jel
		 JOIN journal_entries je ON je.id = jel.journal_entry_id
		 WHERE jel.gl_account = $1 AND je.posted_at <= $2`,
		glAccountCode, asOf)
	var balance int64
	var currency string
	if err := row.Scan(&balance, &currency); err != nil {
		return domain.Money{}, fmt.Errorf("postgres: querying account balance: %w", err)
	}
	return domain.Money{Amount: balance, Currency: currency}, nil
}

func (s *Store) GetPeriod(ctx context.Context, periodID string) (domain.Period, error) {
	row := s.pool.QueryRow(ctx, "SELECT id, status, closed_at, closed_by FROM periods WHERE id = $1", periodID)
	var p domain.Period
	var status string
	err := row.Scan(&p.ID, &status, &p.ClosedAt, &p.ClosedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Period{ID: periodID, Status: domain.PeriodOpen}, nil
	}
	if err != nil {
		return domain.Period{}, fmt.Errorf("postgres: querying period: %w", err)
	}
	p.Status = domain.PeriodStatus(status)
	return p, nil
}

func (s *Store) EarliestOpenPeriodBefore(ctx context.Context, periodID string) (string, bool, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT DISTINCT je.period_id FROM journal_entries je
		 WHERE je.period_id < $1
		   AND je.period_id NOT IN (SELECT id FROM periods WHERE status = 'Closed')
		 ORDER BY je.period_id ASC LIMIT 1`,
		periodID)
	var earliest string
	err := row.Scan(&earliest)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("postgres: querying earliest open period: %w", err)
	}
	return earliest, true, nil
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
		"SELECT id, topic, payload_json, created_at FROM outbox WHERE published_at IS NULL ORDER BY created_at ASC LIMIT $1",
		limit)
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
