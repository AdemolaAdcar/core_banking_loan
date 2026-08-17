package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/pii"
)

// Tx implements internal/store.Tx: every method here runs inside the
// single pgx.Tx opened by Store.WithinTx, so a business write and its
// outbox entry (and, for findOrCreateParty, every dedup_audit row) commit
// or roll back together.
type Tx struct {
	tx  pgx.Tx
	enc pii.Encryptor
}

func (t *Tx) CreateParty(ctx context.Context, p domain.Party) error {
	firstNameEnc, err := t.enc.Encrypt(p.FirstName)
	if err != nil {
		return fmt.Errorf("postgres: encrypting first name: %w", err)
	}
	lastNameEnc, err := t.enc.Encrypt(p.LastName)
	if err != nil {
		return fmt.Errorf("postgres: encrypting last name: %w", err)
	}
	dobEnc, err := t.enc.Encrypt(p.DateOfBirth.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("postgres: encrypting date of birth: %w", err)
	}
	ssnEnc, err := t.enc.Encrypt(p.SSN)
	if err != nil {
		return fmt.Errorf("postgres: encrypting ssn: %w", err)
	}
	emailEnc, err := t.enc.Encrypt(p.Email)
	if err != nil {
		return fmt.Errorf("postgres: encrypting email: %w", err)
	}
	phoneEnc, err := t.enc.Encrypt(p.Phone)
	if err != nil {
		return fmt.Errorf("postgres: encrypting phone: %w", err)
	}

	_, err = t.tx.Exec(ctx, `
		INSERT INTO parties (
			id, status, kyc_status, first_name_enc, last_name_enc, date_of_birth_enc, dob_hash,
			ssn_enc, ssn_hash, ssn_last4, email_enc, email_hash, phone_enc, phone_hash,
			tombstoned, tombstone_reason, tombstoned_by, tombstoned_at, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		p.ID, string(p.Status), string(p.KYCStatus), firstNameEnc, lastNameEnc, dobEnc, dobHash(p.DateOfBirth),
		ssnEnc, hashValue(onlyDigitsForHash(p.SSN)), p.SSNLast4(), emailEnc, hashValue(domain.NormalizeEmail(p.Email)),
		phoneEnc, hashValue(domain.NormalizePhone(p.Phone)),
		p.Tombstoned, nullableString(p.TombstoneReason), nullableString(p.TombstonedBy), p.TombstonedAt,
		p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: inserting party: %w", err)
	}
	return nil
}

// UpdateParty writes the party's current mutable fields (email, phone —
// the only fields UpdateParty ever changes, per internal/service).
// changedFields is not used in the SQL itself (p already reflects the
// post-change state); it exists on the interface for callers that want
// it for their own logging, not because this implementation needs it.
func (t *Tx) UpdateParty(ctx context.Context, p domain.Party, _ []string) error {
	emailEnc, err := t.enc.Encrypt(p.Email)
	if err != nil {
		return fmt.Errorf("postgres: encrypting email: %w", err)
	}
	phoneEnc, err := t.enc.Encrypt(p.Phone)
	if err != nil {
		return fmt.Errorf("postgres: encrypting phone: %w", err)
	}
	_, err = t.tx.Exec(ctx, `
		UPDATE parties SET email_enc = $1, email_hash = $2, phone_enc = $3, phone_hash = $4, updated_at = $5
		WHERE id = $6`,
		emailEnc, hashValue(domain.NormalizeEmail(p.Email)), phoneEnc, hashValue(domain.NormalizePhone(p.Phone)), p.UpdatedAt, p.ID,
	)
	if err != nil {
		return fmt.Errorf("postgres: updating party: %w", err)
	}
	return nil
}

func (t *Tx) TombstoneParty(ctx context.Context, partyID, reason, actor string, at time.Time) error {
	_, err := t.tx.Exec(ctx, `
		UPDATE parties SET tombstoned = TRUE, tombstone_reason = $1, tombstoned_by = $2, tombstoned_at = $3, updated_at = $3
		WHERE id = $4`,
		reason, actor, at, partyID,
	)
	if err != nil {
		return fmt.Errorf("postgres: tombstoning party: %w", err)
	}
	return nil
}

func (t *Tx) AddIdentityDocument(ctx context.Context, d domain.IdentityDocument) error {
	docNumberEnc, err := t.enc.Encrypt(d.DocumentNumber)
	if err != nil {
		return fmt.Errorf("postgres: encrypting document number: %w", err)
	}
	_, err = t.tx.Exec(ctx, `
		INSERT INTO identity_documents (
			id, party_id, document_type, version, supersedes_document_id,
			document_number_enc, document_number_last4, issuing_authority, expires_at, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		d.ID, d.PartyID, string(d.DocumentType), d.Version, d.SupersedesDocumentID,
		docNumberEnc, d.DocumentNumberLast4(), d.IssuingAuthority, d.ExpiresAt, d.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: inserting identity document: %w", err)
	}
	return nil
}

func (t *Tx) RecordDedupAttempt(ctx context.Context, applicantRequestID string, result domain.MatchResult) error {
	matchedFieldsJSON, err := json.Marshal(result.MatchedFields)
	if err != nil {
		return fmt.Errorf("postgres: marshaling matched fields: %w", err)
	}
	_, err = t.tx.Exec(ctx, `
		INSERT INTO dedup_audit (applicant_request_id, matched_party_id, rule_id, confidence, matched_fields)
		VALUES ($1,$2,$3,$4,$5)`,
		applicantRequestID, result.PartyID, result.RuleID, result.Confidence, matchedFieldsJSON,
	)
	if err != nil {
		return fmt.Errorf("postgres: inserting dedup audit row: %w", err)
	}
	return nil
}

func (t *Tx) SaveIdempotentResponse(ctx context.Context, idempotencyKey, requestHash string, responseJSON []byte) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO idempotency_keys (idempotency_key, request_hash, response_json)
		VALUES ($1,$2,$3)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		idempotencyKey, requestHash, responseJSON,
	)
	if err != nil {
		return fmt.Errorf("postgres: saving idempotent response: %w", err)
	}
	return nil
}

func (t *Tx) InsertOutboxEntry(ctx context.Context, e outbox.Entry) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO outbox (id, topic, payload_json, created_at)
		VALUES ($1,$2,$3,$4)`,
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

// onlyDigitsForHash mirrors internal/service.hashSSN's digit-stripping so
// the SSN hash written here is comparable, byte-for-byte, against the
// hash internal/service computes for an incoming applicant.
func onlyDigitsForHash(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= '0' && c <= '9' {
			out = append(out, c)
		}
	}
	return string(out)
}
