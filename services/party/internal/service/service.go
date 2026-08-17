// Package service orchestrates the Party/CIF business logic: it runs the
// dedup engine (internal/domain), decides find-or-create outcomes, and
// writes through internal/store's transactional Store interface so every
// business write and its outbox entry commit atomically. This package
// works exclusively with plaintext domain.Party/domain.IdentityDocument
// values — PII encryption happens at the persistence boundary, inside
// internal/store/postgres, not here. That keeps this package's own unit
// tests free of any encryption concern and lets them run against a fake
// store with no real ciphertext in play at all.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/events"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/store"
)

type Service struct {
	store store.Store
	now   func() time.Time // overridable in tests
	newID func() string    // overridable in tests
}

func New(s store.Store) *Service {
	return &Service{
		store: s,
		now:   func() time.Time { return time.Now().UTC() },
		newID: newUUIDv4,
	}
}

// hashSSN produces the deterministic SSN comparison key the dedup engine
// uses (domain.MatchCandidate.SSNHash / domain.Applicant.SSNHash). This
// is a plain SHA-256, not a slow password-hash (bcrypt/argon2/scrypt) —
// deliberately: dedup needs a fast, exact-match lookup key, not a
// credential hash resistant to brute force. The SSN's actual
// confidentiality is protected by pii.Encryptor on the full value at
// rest; this hash exists only to let the dedup engine and its SQL query
// compare SSNs without ever touching plaintext outside the request that
// received it.
func hashSSN(ssn string) string {
	digits := make([]byte, 0, len(ssn))
	for i := 0; i < len(ssn); i++ {
		if c := ssn[i]; c >= '0' && c <= '9' {
			digits = append(digits, c)
		}
	}
	sum := sha256.Sum256(digits)
	return hex.EncodeToString(sum[:])
}

// FindOrCreateInput mirrors FindOrCreatePartyRequest in the OpenAPI spec.
type FindOrCreateInput struct {
	IdempotencyKey string
	FirstName      string
	LastName       string
	DateOfBirth    time.Time
	SSN            string
	Email          string
	Phone          string
}

// FindOrCreateOutput mirrors FindOrCreatePartyResponse.
type FindOrCreateOutput struct {
	Party    domain.Party
	Created  bool
	Decision domain.Decision
}

// FindOrCreateParty is the onboarding entry point: runs the applicant
// through the dedup engine, and either returns an existing party
// (Created=false) or creates a new one (Created=true) — publishing
// party.created transactionally with the write in the latter case.
// Every candidate the dedup engine considered, matched or not, is
// recorded to the audit log before this method returns, regardless of
// outcome.
func (s *Service) FindOrCreateParty(ctx context.Context, in FindOrCreateInput) (FindOrCreateOutput, error) {
	applicant := domain.Applicant{
		NormalizedFirstName: domain.NormalizeName(in.FirstName),
		NormalizedLastName:  domain.NormalizeName(in.LastName),
		DateOfBirth:         in.DateOfBirth,
		SSNHash:             hashSSN(in.SSN),
		Email:               domain.NormalizeEmail(in.Email),
		Phone:               domain.NormalizePhone(in.Phone),
	}

	candidates, err := s.store.ListDedupCandidates(ctx, store.DedupCandidateFilter{
		SSNHash:             applicant.SSNHash,
		Email:               applicant.Email,
		Phone:               applicant.Phone,
		NormalizedFirstName: applicant.NormalizedFirstName,
		NormalizedLastName:  applicant.NormalizedLastName,
		DateOfBirth:         applicant.DateOfBirth,
	})
	if err != nil {
		return FindOrCreateOutput{}, fmt.Errorf("service: listing dedup candidates: %w", err)
	}

	decision := domain.Decide(domain.EvaluateAll(applicant, candidates))

	var out FindOrCreateOutput
	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		for _, r := range decision.AllCandidates {
			if err := tx.RecordDedupAttempt(ctx, in.IdempotencyKey, r); err != nil {
				return fmt.Errorf("recording dedup attempt for rule %s: %w", r.RuleID, err)
			}
		}

		if decision.Matched {
			p, err := s.store.GetParty(ctx, decision.MatchedPartyID)
			if err != nil {
				return fmt.Errorf("loading matched party %s: %w", decision.MatchedPartyID, err)
			}
			out = FindOrCreateOutput{Party: p, Created: false, Decision: decision}
			return nil
		}

		now := s.now()
		p := domain.Party{
			ID:          s.newID(),
			Status:      domain.PartyStatusActive,
			KYCStatus:   domain.KYCStatusUnverified,
			FirstName:   in.FirstName,
			LastName:    in.LastName,
			DateOfBirth: in.DateOfBirth,
			SSN:         in.SSN,
			Email:       in.Email,
			Phone:       in.Phone,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := tx.CreateParty(ctx, p); err != nil {
			return fmt.Errorf("creating party: %w", err)
		}

		entry, err := outbox.NewEntry(s.newID(), events.TopicPartyCreated, events.PartyCreatedPayload{
			PartyID: p.ID, Status: string(p.Status), KYCStatus: string(p.KYCStatus), CreatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("building party.created outbox entry: %w", err)
		}
		if err := tx.InsertOutboxEntry(ctx, entry); err != nil {
			return fmt.Errorf("inserting party.created outbox entry: %w", err)
		}

		out = FindOrCreateOutput{Party: p, Created: true, Decision: decision}
		return nil
	})
	if err != nil {
		return FindOrCreateOutput{}, err
	}
	return out, nil
}

// UpdatePartyInput mirrors UpdatePartyRequest.
type UpdatePartyInput struct {
	PartyID string
	Email   *string // nil = not supplied, leave unchanged
	Phone   *string
}

// ErrPartyTombstoned is returned when an update is attempted against a
// tombstoned party — tombstoning is one-directional; a tombstoned party
// is never quietly reactivated by an unrelated update call.
var ErrPartyTombstoned = fmt.Errorf("service: party is tombstoned")

func (s *Service) UpdateParty(ctx context.Context, in UpdatePartyInput) (domain.Party, error) {
	var out domain.Party
	err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		p, err := s.store.GetParty(ctx, in.PartyID)
		if err != nil {
			return fmt.Errorf("loading party %s: %w", in.PartyID, err)
		}
		if p.Tombstoned {
			return ErrPartyTombstoned
		}

		var changed []string
		if in.Email != nil && *in.Email != p.Email {
			p.Email = *in.Email
			changed = append(changed, "email")
		}
		if in.Phone != nil && *in.Phone != p.Phone {
			p.Phone = *in.Phone
			changed = append(changed, "phone")
		}
		if len(changed) == 0 {
			out = p
			return nil
		}

		now := s.now()
		p.UpdatedAt = now
		if err := tx.UpdateParty(ctx, p, changed); err != nil {
			return fmt.Errorf("updating party: %w", err)
		}

		entry, err := outbox.NewEntry(s.newID(), events.TopicPartyUpdated, events.PartyUpdatedPayload{
			PartyID: p.ID, ChangedFields: changed, UpdatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("building party.updated outbox entry: %w", err)
		}
		if err := tx.InsertOutboxEntry(ctx, entry); err != nil {
			return fmt.Errorf("inserting party.updated outbox entry: %w", err)
		}

		out = p
		return nil
	})
	return out, err
}

// TombstonePartyInput mirrors TombstonePartyRequest.
type TombstonePartyInput struct {
	PartyID string
	Reason  string
	Actor   string
}

func (s *Service) TombstoneParty(ctx context.Context, in TombstonePartyInput) (domain.Party, error) {
	var out domain.Party
	err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		p, err := s.store.GetParty(ctx, in.PartyID)
		if err != nil {
			return fmt.Errorf("loading party %s: %w", in.PartyID, err)
		}
		now := s.now()
		if p.Tombstoned {
			// Idempotent: tombstoning an already-tombstoned party is a
			// no-op success, not an error -- a retried request must not
			// fail just because it already succeeded once.
			out = p
			return nil
		}

		if err := tx.TombstoneParty(ctx, p.ID, in.Reason, in.Actor, now); err != nil {
			return fmt.Errorf("tombstoning party: %w", err)
		}
		p.Tombstoned = true
		p.TombstoneReason = in.Reason
		p.TombstonedBy = in.Actor
		p.TombstonedAt = &now
		p.UpdatedAt = now

		entry, err := outbox.NewEntry(s.newID(), events.TopicPartyTombstoned, events.PartyTombstonedPayload{
			PartyID: p.ID, Reason: in.Reason, Actor: in.Actor, TombstonedAt: now,
		})
		if err != nil {
			return fmt.Errorf("building party.tombstoned outbox entry: %w", err)
		}
		if err := tx.InsertOutboxEntry(ctx, entry); err != nil {
			return fmt.Errorf("inserting party.tombstoned outbox entry: %w", err)
		}

		out = p
		return nil
	})
	return out, err
}

// AddIdentityDocumentInput mirrors AddIdentityDocumentRequest.
type AddIdentityDocumentInput struct {
	PartyID          string
	DocumentType     domain.DocumentType
	DocumentNumber   string
	IssuingAuthority string
	ExpiresAt        *time.Time
}

// AddIdentityDocument records a new document version. If a document of
// the same type already exists for this party, the new row's Version is
// prior-max+1 and SupersedesDocumentID points at the prior version — the
// prior row is never edited or deleted.
func (s *Service) AddIdentityDocument(ctx context.Context, in AddIdentityDocumentInput) (domain.IdentityDocument, error) {
	var out domain.IdentityDocument
	err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		maxVersion, priorID, err := s.store.MaxDocumentVersion(ctx, in.PartyID, in.DocumentType)
		if err != nil {
			return fmt.Errorf("looking up prior document version: %w", err)
		}

		d := domain.IdentityDocument{
			ID:                   s.newID(),
			PartyID:              in.PartyID,
			DocumentType:         in.DocumentType,
			Version:              maxVersion + 1,
			SupersedesDocumentID: priorID,
			DocumentNumber:       in.DocumentNumber,
			IssuingAuthority:     in.IssuingAuthority,
			ExpiresAt:            in.ExpiresAt,
			CreatedAt:            s.now(),
		}
		if err := tx.AddIdentityDocument(ctx, d); err != nil {
			return fmt.Errorf("adding identity document: %w", err)
		}
		out = d
		return nil
	})
	return out, err
}
