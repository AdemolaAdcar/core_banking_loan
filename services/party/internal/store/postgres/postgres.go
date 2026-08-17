// Package postgres is the only place in this service that imports pgx or
// touches SQL directly, and the only place a x-pii:true field is ever
// decrypted back into plaintext (on read) or encrypted (on write) — see
// internal/pii.Encryptor. It implements internal/store.Store /
// internal/store.Tx, so internal/service never depends on this package
// or on pgx at all; it depends on the store interfaces, satisfied here
// or by a fake in tests.
//
// PII columns come in two parts: an encrypted column (*_enc, produced by
// pii.Encryptor — the actual confidentiality control) and, for fields
// the dedup engine needs to look up by exact match, a deterministic
// SHA-256 hash column (*_hash — an index key, not a secret; see
// migrations/0001_init.up.sql for the full rationale). Every dedup query
// here filters on the indexed hash columns first and only decrypts the
// bounded set of rows that survive that filter — never a full-table
// decrypt-and-compare.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/pii"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/store"
)

const dobLayout = "2006-01-02"

type Store struct {
	pool *pgxpool.Pool
	enc  pii.Encryptor
}

func New(pool *pgxpool.Pool, enc pii.Encryptor) *Store {
	return &Store{pool: pool, enc: enc}
}

func hashValue(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func dobHash(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return hashValue(t.UTC().Format(dobLayout))
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

func (s *Store) decryptParty(ctx context.Context, row partyRow) (domain.Party, error) {
	firstName, err := s.enc.Decrypt(row.FirstNameEnc)
	if err != nil {
		return domain.Party{}, fmt.Errorf("postgres: decrypting first name: %w", err)
	}
	lastName, err := s.enc.Decrypt(row.LastNameEnc)
	if err != nil {
		return domain.Party{}, fmt.Errorf("postgres: decrypting last name: %w", err)
	}
	dobStr, err := s.enc.Decrypt(row.DateOfBirthEnc)
	if err != nil {
		return domain.Party{}, fmt.Errorf("postgres: decrypting date of birth: %w", err)
	}
	dob, err := time.Parse(time.RFC3339, dobStr)
	if err != nil {
		return domain.Party{}, fmt.Errorf("postgres: parsing decrypted date of birth: %w", err)
	}
	ssn, err := s.enc.Decrypt(row.SSNEnc)
	if err != nil {
		return domain.Party{}, fmt.Errorf("postgres: decrypting ssn: %w", err)
	}
	email, err := s.enc.Decrypt(row.EmailEnc)
	if err != nil {
		return domain.Party{}, fmt.Errorf("postgres: decrypting email: %w", err)
	}
	phone, err := s.enc.Decrypt(row.PhoneEnc)
	if err != nil {
		return domain.Party{}, fmt.Errorf("postgres: decrypting phone: %w", err)
	}
	return domain.Party{
		ID:              row.ID,
		Status:          domain.PartyStatus(row.Status),
		KYCStatus:       domain.KYCStatus(row.KYCStatus),
		FirstName:       firstName,
		LastName:        lastName,
		DateOfBirth:     dob,
		SSN:             ssn,
		Email:           email,
		Phone:           phone,
		Tombstoned:      row.Tombstoned,
		TombstoneReason: row.TombstoneReason,
		TombstonedBy:    row.TombstonedBy,
		TombstonedAt:    row.TombstonedAt,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}, nil
}

type partyRow struct {
	ID              string
	Status          string
	KYCStatus       string
	FirstNameEnc    string
	LastNameEnc     string
	DateOfBirthEnc  string
	SSNEnc          string
	SSNHash         string
	EmailEnc        string
	PhoneEnc        string
	Tombstoned      bool
	TombstoneReason string
	TombstonedBy    string
	TombstonedAt    *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

const partyColumns = `id, status, kyc_status, first_name_enc, last_name_enc, date_of_birth_enc,
	ssn_enc, ssn_hash, email_enc, phone_enc, tombstoned, tombstone_reason, tombstoned_by, tombstoned_at,
	created_at, updated_at`

func scanPartyRow(row pgx.Row) (partyRow, error) {
	var r partyRow
	var tombstoneReason, tombstonedBy *string
	err := row.Scan(
		&r.ID, &r.Status, &r.KYCStatus, &r.FirstNameEnc, &r.LastNameEnc, &r.DateOfBirthEnc,
		&r.SSNEnc, &r.SSNHash, &r.EmailEnc, &r.PhoneEnc, &r.Tombstoned, &tombstoneReason, &tombstonedBy, &r.TombstonedAt,
		&r.CreatedAt, &r.UpdatedAt,
	)
	if tombstoneReason != nil {
		r.TombstoneReason = *tombstoneReason
	}
	if tombstonedBy != nil {
		r.TombstonedBy = *tombstonedBy
	}
	return r, err
}

func (s *Store) GetParty(ctx context.Context, partyID string) (domain.Party, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+partyColumns+" FROM parties WHERE id = $1", partyID)
	r, err := scanPartyRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Party{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Party{}, fmt.Errorf("postgres: querying party: %w", err)
	}
	return s.decryptParty(ctx, r)
}

// ListDedupCandidates builds one indexed OR query from whichever filter
// fields are non-empty, and never falls back to a full-table scan: a
// filter with nothing set returns no candidates rather than every party
// in the table.
func (s *Store) ListDedupCandidates(ctx context.Context, filter store.DedupCandidateFilter) ([]domain.MatchCandidate, error) {
	var clauses []string
	var args []any

	if filter.SSNHash != "" {
		args = append(args, filter.SSNHash)
		clauses = append(clauses, fmt.Sprintf("ssn_hash = $%d", len(args)))
	}
	if filter.Email != "" {
		args = append(args, hashValue(filter.Email))
		clauses = append(clauses, fmt.Sprintf("email_hash = $%d", len(args)))
	}
	if filter.Phone != "" {
		args = append(args, hashValue(filter.Phone))
		clauses = append(clauses, fmt.Sprintf("phone_hash = $%d", len(args)))
	}
	if !filter.DateOfBirth.IsZero() {
		args = append(args, dobHash(filter.DateOfBirth))
		clauses = append(clauses, fmt.Sprintf("dob_hash = $%d", len(args)))
	}
	if len(clauses) == 0 {
		return nil, nil
	}

	query := "SELECT " + partyColumns + " FROM parties WHERE " + strings.Join(clauses, " OR ")
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying dedup candidates: %w", err)
	}
	defer rows.Close()

	var out []domain.MatchCandidate
	for rows.Next() {
		r, err := scanPartyRow(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scanning dedup candidate: %w", err)
		}
		p, err := s.decryptParty(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, domain.MatchCandidate{
			PartyID:             p.ID,
			NormalizedFirstName: domain.NormalizeName(p.FirstName),
			NormalizedLastName:  domain.NormalizeName(p.LastName),
			DateOfBirth:         p.DateOfBirth,
			SSNHash:             r.SSNHash,
			Email:               domain.NormalizeEmail(p.Email),
			Phone:               domain.NormalizePhone(p.Phone),
			Tombstoned:          p.Tombstoned,
		})
	}
	return out, rows.Err()
}

func (s *Store) ListIdentityDocuments(ctx context.Context, partyID string) ([]domain.IdentityDocument, error) {
	rows, err := s.pool.Query(ctx, documentColumns+" FROM identity_documents WHERE party_id = $1 ORDER BY document_type, version", partyID)
	if err != nil {
		return nil, fmt.Errorf("postgres: querying identity documents: %w", err)
	}
	defer rows.Close()

	var out []domain.IdentityDocument
	for rows.Next() {
		d, err := s.scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetIdentityDocument(ctx context.Context, partyID, documentID string) (domain.IdentityDocument, error) {
	row := s.pool.QueryRow(ctx, documentColumns+" FROM identity_documents WHERE party_id = $1 AND id = $2", partyID, documentID)
	d, err := s.scanDocument(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.IdentityDocument{}, store.ErrNotFound
	}
	return d, err
}

const documentColumns = `SELECT id, party_id, document_type, version, supersedes_document_id,
	document_number_enc, issuing_authority, expires_at, created_at`

func (s *Store) scanDocument(row pgx.Row) (domain.IdentityDocument, error) {
	var d domain.IdentityDocument
	var documentType string
	var docNumberEnc string
	var supersedes *string
	var issuingAuthority string
	err := row.Scan(&d.ID, &d.PartyID, &documentType, &d.Version, &supersedes, &docNumberEnc, &issuingAuthority, &d.ExpiresAt, &d.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.IdentityDocument{}, pgx.ErrNoRows
		}
		return domain.IdentityDocument{}, fmt.Errorf("postgres: scanning identity document: %w", err)
	}
	d.DocumentType = domain.DocumentType(documentType)
	d.SupersedesDocumentID = supersedes
	d.IssuingAuthority = issuingAuthority
	docNumber, err := s.enc.Decrypt(docNumberEnc)
	if err != nil {
		return domain.IdentityDocument{}, fmt.Errorf("postgres: decrypting document number: %w", err)
	}
	d.DocumentNumber = docNumber
	return d, nil
}

func (s *Store) MaxDocumentVersion(ctx context.Context, partyID string, docType domain.DocumentType) (int, *string, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT id, version FROM identity_documents WHERE party_id = $1 AND document_type = $2 ORDER BY version DESC LIMIT 1",
		partyID, string(docType))
	var id string
	var version int
	err := row.Scan(&id, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("postgres: querying max document version: %w", err)
	}
	return version, &id, nil
}

// ListUnpublished and MarkPublished implement outbox.Reader — the
// interface internal/relay's Kafka publisher polls against. They run
// outside any request-path transaction: the relay is a separate
// process/goroutine from the request path that wrote these rows (see
// internal/relay's package doc comment for the at-least-once delivery
// tradeoff that implies).
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
