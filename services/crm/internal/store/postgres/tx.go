package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/pii"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/store"
)

// Tx implements internal/store.Tx: every method here runs inside the
// single pgx.Tx opened by Store.WithinTx.
type Tx struct {
	tx  pgx.Tx
	enc pii.Encryptor
}

func (t *Tx) CreateInteraction(ctx context.Context, i domain.Interaction) error {
	notesEnc, err := t.enc.Encrypt(i.Notes)
	if err != nil {
		return fmt.Errorf("postgres: encrypting interaction notes: %w", err)
	}
	_, err = t.tx.Exec(ctx,
		`INSERT INTO interactions (id, loan_account_id, event_type, occurred_at, notes_enc, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		i.ID, i.LoanAccountID, string(i.EventType), i.OccurredAt, notesEnc, i.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: inserting interaction: %w", err)
	}
	return nil
}

func (t *Tx) CreateCase(ctx context.Context, c domain.ServiceCase) error {
	closeReasonEnc, err := encryptOptional(t.enc, c.CloseReason)
	if err != nil {
		return fmt.Errorf("postgres: encrypting close reason: %w", err)
	}
	reopenReasonEnc, err := encryptOptional(t.enc, c.ReopenReason)
	if err != nil {
		return fmt.Errorf("postgres: encrypting reopen reason: %w", err)
	}
	_, err = t.tx.Exec(ctx,
		`INSERT INTO cases (
			id, party_id, loan_account_id, status, reason_code, assigned_to, version,
			sla_due_at, escalated, close_reason_enc, reopen_reason_enc, opened_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		c.ID, c.PartyID, c.LoanAccountID, string(c.Status), string(c.ReasonCode), c.AssignedTo, c.Version,
		c.SLADueAt, c.Escalated, closeReasonEnc, reopenReasonEnc, c.OpenedAt, c.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: inserting case: %w", err)
	}
	return nil
}

// UpdateCaseConditional writes c only if the stored row's version still
// equals priorVersion -- see store.Tx's doc comment for why this DB-level
// guard exists alongside the domain-level check.
func (t *Tx) UpdateCaseConditional(ctx context.Context, c domain.ServiceCase, priorVersion int) error {
	closeReasonEnc, err := encryptOptional(t.enc, c.CloseReason)
	if err != nil {
		return fmt.Errorf("postgres: encrypting close reason: %w", err)
	}
	reopenReasonEnc, err := encryptOptional(t.enc, c.ReopenReason)
	if err != nil {
		return fmt.Errorf("postgres: encrypting reopen reason: %w", err)
	}
	tag, err := t.tx.Exec(ctx,
		`UPDATE cases SET
			status = $1, assigned_to = $2, version = $3, sla_due_at = $4, escalated = $5,
			close_reason_enc = $6, reopen_reason_enc = $7, updated_at = $8
		 WHERE id = $9 AND version = $10`,
		string(c.Status), c.AssignedTo, c.Version, c.SLADueAt, c.Escalated,
		closeReasonEnc, reopenReasonEnc, c.UpdatedAt, c.ID, priorVersion,
	)
	if err != nil {
		return fmt.Errorf("postgres: updating case: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrStaleVersion
	}
	return nil
}

func (t *Tx) AddCaseNote(ctx context.Context, n domain.CaseNote) error {
	bodyEnc, err := t.enc.Encrypt(n.Body)
	if err != nil {
		return fmt.Errorf("postgres: encrypting case note body: %w", err)
	}
	_, err = t.tx.Exec(ctx,
		`INSERT INTO case_notes (id, case_id, author_id, body_enc, created_at) VALUES ($1,$2,$3,$4,$5)`,
		n.ID, n.CaseID, n.AuthorID, bodyEnc, n.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: inserting case note: %w", err)
	}
	return nil
}

func (t *Tx) RecordAccess(ctx context.Context, actorSubject, resourceType, resourceID string, at time.Time) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO case_note_access_log (actor_subject, resource_type, resource_id, accessed_at) VALUES ($1,$2,$3,$4)`,
		actorSubject, resourceType, resourceID, at,
	)
	if err != nil {
		return fmt.Errorf("postgres: recording access: %w", err)
	}
	return nil
}

func (t *Tx) UpsertCommunicationPreferences(ctx context.Context, p domain.CommunicationPreferences) error {
	var channel *string
	if p.PreferredChannel != nil {
		s := string(*p.PreferredChannel)
		channel = &s
	}
	_, err := t.tx.Exec(ctx,
		`INSERT INTO communication_preferences (party_id, preferred_channel, email_opt_in, sms_opt_in, phone_opt_in, mail_opt_in, do_not_contact, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (party_id) DO UPDATE SET
			preferred_channel = EXCLUDED.preferred_channel,
			email_opt_in = EXCLUDED.email_opt_in,
			sms_opt_in = EXCLUDED.sms_opt_in,
			phone_opt_in = EXCLUDED.phone_opt_in,
			mail_opt_in = EXCLUDED.mail_opt_in,
			do_not_contact = EXCLUDED.do_not_contact,
			updated_at = EXCLUDED.updated_at`,
		p.PartyID, channel, p.EmailOptIn, p.SMSOptIn, p.PhoneOptIn, p.MailOptIn, p.DoNotContact, p.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: upserting communication preferences: %w", err)
	}
	return nil
}

func (t *Tx) AssignRelationshipManager(ctx context.Context, a domain.RelationshipManagerAssignment) error {
	var rmID string
	if a.RelationshipManagerID != nil {
		rmID = *a.RelationshipManagerID
	}
	var assignedAt time.Time
	if a.AssignedAt != nil {
		assignedAt = *a.AssignedAt
	}
	_, err := t.tx.Exec(ctx,
		`INSERT INTO relationship_manager_assignments (party_id, relationship_manager_id, assigned_at) VALUES ($1,$2,$3)`,
		a.PartyID, rmID, assignedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: inserting rm assignment: %w", err)
	}
	return nil
}

func (t *Tx) LinkLoanAccountToParty(ctx context.Context, loanAccountID, partyID string) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO loan_account_links (loan_account_id, party_id) VALUES ($1,$2)
		 ON CONFLICT (loan_account_id) DO NOTHING`,
		loanAccountID, partyID,
	)
	if err != nil {
		return fmt.Errorf("postgres: linking loan account to party: %w", err)
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

func encryptOptional(enc pii.Encryptor, s *string) (*string, error) {
	if s == nil {
		return nil, nil
	}
	ciphertext, err := enc.Encrypt(*s)
	if err != nil {
		return nil, err
	}
	return &ciphertext, nil
}
