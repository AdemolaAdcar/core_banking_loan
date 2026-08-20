// Package postgres is the only place in this service that imports pgx or
// touches SQL directly, and the only place PII-adjacent content (case
// notes, close/reopen reasons, interaction notes) is ever decrypted back
// into plaintext (on read) or encrypted (on write). It implements
// internal/store.Store / internal/store.Tx, so internal/service never
// depends on this package or on pgx at all.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/pii"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/store"
)

type Store struct {
	pool *pgxpool.Pool
	enc  pii.Encryptor
}

func New(pool *pgxpool.Pool, enc pii.Encryptor) *Store {
	return &Store{pool: pool, enc: enc}
}

func (s *Store) WithinTx(ctx context.Context, fn func(store.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: beginning transaction: %w", err)
	}
	ptx := &Tx{tx: tx, enc: s.enc}
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

const caseColumns = `id, party_id, loan_account_id, status, reason_code, assigned_to, version,
	sla_due_at, escalated, close_reason_enc, reopen_reason_enc, opened_at, updated_at`

func (s *Store) decodeCase(row pgx.Row) (domain.ServiceCase, error) {
	var c domain.ServiceCase
	var loanAccountID, assignedTo, closeReasonEnc, reopenReasonEnc *string
	var status, reasonCode string
	err := row.Scan(&c.ID, &c.PartyID, &loanAccountID, &status, &reasonCode, &assignedTo, &c.Version,
		&c.SLADueAt, &c.Escalated, &closeReasonEnc, &reopenReasonEnc, &c.OpenedAt, &c.UpdatedAt)
	if err != nil {
		return domain.ServiceCase{}, err
	}
	c.LoanAccountID = loanAccountID
	c.AssignedTo = assignedTo
	c.Status = domain.CaseStatus(status)
	c.ReasonCode = domain.ReasonCode(reasonCode)

	if closeReasonEnc != nil {
		reason, err := s.enc.Decrypt(*closeReasonEnc)
		if err != nil {
			return domain.ServiceCase{}, fmt.Errorf("postgres: decrypting close reason: %w", err)
		}
		if reason != "" {
			c.CloseReason = &reason
		}
	}
	if reopenReasonEnc != nil {
		reason, err := s.enc.Decrypt(*reopenReasonEnc)
		if err != nil {
			return domain.ServiceCase{}, fmt.Errorf("postgres: decrypting reopen reason: %w", err)
		}
		if reason != "" {
			c.ReopenReason = &reason
		}
	}
	return c, nil
}

func (s *Store) GetCase(ctx context.Context, caseID string) (domain.ServiceCase, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+caseColumns+" FROM cases WHERE id = $1", caseID)
	c, err := s.decodeCase(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ServiceCase{}, store.ErrNotFound
	}
	if err != nil {
		return domain.ServiceCase{}, fmt.Errorf("postgres: querying case: %w", err)
	}
	return c, nil
}

func (s *Store) ListOpenCasesForParty(ctx context.Context, partyID string) ([]domain.ServiceCase, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+caseColumns+" FROM cases WHERE party_id = $1 AND status <> 'Closed' ORDER BY opened_at DESC",
		partyID)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying open cases: %w", err)
	}
	defer rows.Close()

	var out []domain.ServiceCase
	for rows.Next() {
		c, err := s.decodeCase(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scanning case: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ListCasesPastSLA(ctx context.Context, now time.Time, limit int) ([]domain.ServiceCase, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+caseColumns+` FROM cases
		 WHERE escalated = FALSE AND status IN ('Open', 'InProgress') AND sla_due_at < $1
		 ORDER BY sla_due_at ASC LIMIT $2`,
		now, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying cases past sla: %w", err)
	}
	defer rows.Close()

	var out []domain.ServiceCase
	for rows.Next() {
		c, err := s.decodeCase(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scanning case: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ListCaseNotes(ctx context.Context, caseID string) ([]domain.CaseNote, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT id, case_id, author_id, body_enc, created_at FROM case_notes WHERE case_id = $1 ORDER BY created_at ASC",
		caseID)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying case notes: %w", err)
	}
	defer rows.Close()

	var out []domain.CaseNote
	for rows.Next() {
		var n domain.CaseNote
		var bodyEnc string
		if err := rows.Scan(&n.ID, &n.CaseID, &n.AuthorID, &bodyEnc, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scanning case note: %w", err)
		}
		body, err := s.enc.Decrypt(bodyEnc)
		if err != nil {
			return nil, fmt.Errorf("postgres: decrypting case note body: %w", err)
		}
		n.Body = body
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) GetCommunicationPreferences(ctx context.Context, partyID string) (domain.CommunicationPreferences, bool, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT party_id, preferred_channel, email_opt_in, sms_opt_in, phone_opt_in, mail_opt_in, do_not_contact, updated_at
		 FROM communication_preferences WHERE party_id = $1`, partyID)
	var p domain.CommunicationPreferences
	var channel *string
	err := row.Scan(&p.PartyID, &channel, &p.EmailOptIn, &p.SMSOptIn, &p.PhoneOptIn, &p.MailOptIn, &p.DoNotContact, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CommunicationPreferences{}, false, nil
	}
	if err != nil {
		return domain.CommunicationPreferences{}, false, fmt.Errorf("postgres: querying communication preferences: %w", err)
	}
	if channel != nil {
		c := domain.PreferredChannel(*channel)
		p.PreferredChannel = &c
	}
	return p, true, nil
}

func (s *Store) GetRelationshipManagerAssignment(ctx context.Context, partyID string) (domain.RelationshipManagerAssignment, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT relationship_manager_id, assigned_at FROM relationship_manager_assignments
		 WHERE party_id = $1 ORDER BY assigned_at DESC LIMIT 1`, partyID)
	var rmID string
	var assignedAt time.Time
	err := row.Scan(&rmID, &assignedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RelationshipManagerAssignment{PartyID: partyID}, nil
	}
	if err != nil {
		return domain.RelationshipManagerAssignment{}, fmt.Errorf("postgres: querying rm assignment: %w", err)
	}
	return domain.RelationshipManagerAssignment{PartyID: partyID, RelationshipManagerID: &rmID, AssignedAt: &assignedAt}, nil
}

func (s *Store) ListLoanAccountIDsForParty(ctx context.Context, partyID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, "SELECT loan_account_id FROM loan_account_links WHERE party_id = $1", partyID)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying loan account links: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("postgres: scanning loan account link: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) LatestInteractionPerLoanAccount(ctx context.Context, loanAccountIDs []string) (map[string]domain.Interaction, error) {
	out := map[string]domain.Interaction{}
	if len(loanAccountIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (loan_account_id) id, loan_account_id, event_type, occurred_at, created_at
		 FROM interactions WHERE loan_account_id = ANY($1)
		 ORDER BY loan_account_id, occurred_at DESC`,
		loanAccountIDs)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying latest interactions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var i domain.Interaction
		var eventType string
		if err := rows.Scan(&i.ID, &i.LoanAccountID, &eventType, &i.OccurredAt, &i.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scanning interaction: %w", err)
		}
		i.EventType = domain.EventType(eventType)
		out[i.LoanAccountID] = i
	}
	return out, rows.Err()
}

func (s *Store) ListRecentInteractionsForLoanAccounts(ctx context.Context, loanAccountIDs []string, limit int) ([]domain.Interaction, error) {
	if len(loanAccountIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, loan_account_id, event_type, occurred_at, notes_enc, created_at
		 FROM interactions WHERE loan_account_id = ANY($1)
		 ORDER BY occurred_at DESC LIMIT $2`,
		loanAccountIDs, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying recent interactions: %w", err)
	}
	defer rows.Close()

	var out []domain.Interaction
	for rows.Next() {
		var i domain.Interaction
		var eventType, notesEnc string
		if err := rows.Scan(&i.ID, &i.LoanAccountID, &eventType, &i.OccurredAt, &notesEnc, &i.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scanning interaction: %w", err)
		}
		i.EventType = domain.EventType(eventType)
		notes, err := s.enc.Decrypt(notesEnc)
		if err != nil {
			return nil, fmt.Errorf("postgres: decrypting interaction notes: %w", err)
		}
		i.Notes = notes
		out = append(out, i)
	}
	return out, rows.Err()
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

// ListUnpublished / MarkPublished satisfy outbox.Reader for internal/relay.
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
