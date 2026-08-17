package service

import (
	"context"
	"testing"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/domain"
)

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func sequentialIDs(prefix string) func() string {
	n := 0
	return func() string {
		n++
		return prefix + "-" + itoa(n)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func newTestService(fs *fakeStore) *Service {
	s := New(fs)
	s.now = fixedClock(time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC))
	s.newID = sequentialIDs("id")
	return s
}

func TestFindOrCreateParty_NoCandidates_CreatesNewPartyAndPublishesOutbox(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)

	out, err := s.FindOrCreateParty(context.Background(), FindOrCreateInput{
		IdempotencyKey: "req-1",
		FirstName:      "Jane",
		LastName:       "Doe",
		DateOfBirth:    time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC),
		SSN:            "123-45-6789",
		Email:          "jane@example.com",
		Phone:          "512-555-1234",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Created {
		t.Fatalf("expected Created=true, got false")
	}
	if out.Party.ID == "" {
		t.Fatalf("expected a generated party ID")
	}
	if len(fs.outboxEntries) != 1 {
		t.Fatalf("expected exactly 1 outbox entry, got %d", len(fs.outboxEntries))
	}
	if fs.outboxEntries[0].Topic != "party.created" {
		t.Fatalf("expected topic party.created, got %s", fs.outboxEntries[0].Topic)
	}
	if _, ok := fs.parties[out.Party.ID]; !ok {
		t.Fatalf("expected party to be persisted")
	}
}

func TestFindOrCreateParty_SSNExactMatch_ReturnsExistingParty_NoWrite(t *testing.T) {
	fs := newFakeStore()
	existing := domain.Party{
		ID: "existing-1", Status: domain.PartyStatusActive, KYCStatus: domain.KYCStatusVerified,
		FirstName: "Jane", LastName: "Doe", DateOfBirth: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	fs.parties[existing.ID] = existing
	fs.dedupCandidates = []domain.MatchCandidate{
		{
			PartyID:             existing.ID,
			NormalizedFirstName: domain.NormalizeName("Jane"),
			NormalizedLastName:  domain.NormalizeName("Doe"),
			DateOfBirth:         existing.DateOfBirth,
			SSNHash:             hashSSN("123-45-6789"),
		},
	}
	s := newTestService(fs)

	out, err := s.FindOrCreateParty(context.Background(), FindOrCreateInput{
		IdempotencyKey: "req-2",
		FirstName:      "Jane",
		LastName:       "Doe",
		DateOfBirth:    existing.DateOfBirth,
		SSN:            "123-45-6789",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Created {
		t.Fatalf("expected Created=false (matched existing party)")
	}
	if out.Party.ID != existing.ID {
		t.Fatalf("expected matched party %s, got %s", existing.ID, out.Party.ID)
	}
	if len(fs.outboxEntries) != 0 {
		t.Fatalf("expected no outbox entries for a matched (non-created) party, got %d", len(fs.outboxEntries))
	}
	if len(fs.parties) != 1 {
		t.Fatalf("expected no new party row written, got %d parties", len(fs.parties))
	}
}

func TestFindOrCreateParty_RecordsEveryCandidateConsidered_NotOnlyTheWinner(t *testing.T) {
	fs := newFakeStore()
	fs.parties["p1"] = domain.Party{ID: "p1"}
	fs.parties["p2"] = domain.Party{ID: "p2"}
	dob := time.Date(1985, 6, 15, 0, 0, 0, 0, time.UTC)
	fs.dedupCandidates = []domain.MatchCandidate{
		{ // SSN exact -- wins
			PartyID: "p1", SSNHash: hashSSN("999-99-9999"),
			NormalizedFirstName: "alice", NormalizedLastName: "smith", DateOfBirth: dob,
		},
		{ // email-only, weaker signal, still clears AuditFloor -- must still be logged
			PartyID: "p2", Email: "alice@example.com",
			NormalizedFirstName: "bob", NormalizedLastName: "jones",
		},
	}
	s := newTestService(fs)

	out, err := s.FindOrCreateParty(context.Background(), FindOrCreateInput{
		IdempotencyKey: "req-3",
		FirstName:      "Alice",
		LastName:       "Smith",
		DateOfBirth:    dob,
		SSN:            "999-99-9999",
		Email:          "alice@example.com",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Created || out.Party.ID != "p1" {
		t.Fatalf("expected match on p1, got %+v", out)
	}
	if len(fs.dedupAttempts) != 2 {
		t.Fatalf("expected both candidates to be recorded to the audit log, got %d", len(fs.dedupAttempts))
	}
}

func TestFindOrCreateParty_AmbiguousTopTwo_CreatesNewParty_StillAudited(t *testing.T) {
	fs := newFakeStore()
	dob := time.Date(1985, 6, 15, 0, 0, 0, 0, time.UTC)
	fs.parties["p1"] = domain.Party{ID: "p1"}
	fs.parties["p2"] = domain.Party{ID: "p2"}
	fs.dedupCandidates = []domain.MatchCandidate{
		{PartyID: "p1", NormalizedFirstName: "alice", NormalizedLastName: "smith", DateOfBirth: dob},
		{PartyID: "p2", NormalizedFirstName: "alice", NormalizedLastName: "smith", DateOfBirth: dob},
	}
	s := newTestService(fs)

	out, err := s.FindOrCreateParty(context.Background(), FindOrCreateInput{
		IdempotencyKey: "req-4",
		FirstName:      "Alice",
		LastName:       "Smith",
		DateOfBirth:    dob,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Created {
		t.Fatalf("expected ambiguous match to conservatively create a new party, got matched")
	}
	if len(fs.dedupAttempts) != 2 {
		t.Fatalf("expected both ambiguous candidates recorded to audit log, got %d", len(fs.dedupAttempts))
	}
}

func TestUpdateParty_TombstonedParty_Refused(t *testing.T) {
	fs := newFakeStore()
	now := time.Now().UTC()
	fs.parties["p1"] = domain.Party{ID: "p1", Tombstoned: true, TombstonedAt: &now}
	s := newTestService(fs)

	newEmail := "new@example.com"
	_, err := s.UpdateParty(context.Background(), UpdatePartyInput{PartyID: "p1", Email: &newEmail})
	if err != ErrPartyTombstoned {
		t.Fatalf("expected ErrPartyTombstoned, got %v", err)
	}
	if len(fs.outboxEntries) != 0 {
		t.Fatalf("expected no outbox entry published for a refused update")
	}
}

func TestUpdateParty_NoActualChange_IsNoOp_NoWriteNoEvent(t *testing.T) {
	fs := newFakeStore()
	fs.parties["p1"] = domain.Party{ID: "p1", Email: "same@example.com", Phone: "5125551234"}
	s := newTestService(fs)

	sameEmail := "same@example.com"
	out, err := s.UpdateParty(context.Background(), UpdatePartyInput{PartyID: "p1", Email: &sameEmail})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Email != "same@example.com" {
		t.Fatalf("expected unchanged party returned")
	}
	if len(fs.outboxEntries) != 0 {
		t.Fatalf("expected no outbox entry for a no-op update, got %d", len(fs.outboxEntries))
	}
}

func TestUpdateParty_EmailChanged_PublishesChangedFieldNamesOnly(t *testing.T) {
	fs := newFakeStore()
	fs.parties["p1"] = domain.Party{ID: "p1", Email: "old@example.com"}
	s := newTestService(fs)

	newEmail := "new@example.com"
	out, err := s.UpdateParty(context.Background(), UpdatePartyInput{PartyID: "p1", Email: &newEmail})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Email != newEmail {
		t.Fatalf("expected email updated in returned party")
	}
	if len(fs.outboxEntries) != 1 || fs.outboxEntries[0].Topic != "party.updated" {
		t.Fatalf("expected exactly one party.updated outbox entry, got %+v", fs.outboxEntries)
	}
}

func TestTombstoneParty_FirstCall_WritesAndPublishes(t *testing.T) {
	fs := newFakeStore()
	fs.parties["p1"] = domain.Party{ID: "p1"}
	s := newTestService(fs)

	out, err := s.TombstoneParty(context.Background(), TombstonePartyInput{PartyID: "p1", Reason: "duplicate", Actor: "analyst@bank.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Tombstoned {
		t.Fatalf("expected party to be tombstoned")
	}
	if len(fs.outboxEntries) != 1 || fs.outboxEntries[0].Topic != "party.tombstoned" {
		t.Fatalf("expected exactly one party.tombstoned outbox entry, got %+v", fs.outboxEntries)
	}
}

func TestTombstoneParty_AlreadyTombstoned_IsIdempotent_NoDuplicateEvent(t *testing.T) {
	fs := newFakeStore()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fs.parties["p1"] = domain.Party{ID: "p1", Tombstoned: true, TombstoneReason: "duplicate", TombstonedBy: "analyst@bank.com", TombstonedAt: &at}
	s := newTestService(fs)

	out, err := s.TombstoneParty(context.Background(), TombstonePartyInput{PartyID: "p1", Reason: "duplicate", Actor: "analyst@bank.com"})
	if err != nil {
		t.Fatalf("expected idempotent success on retry, got error: %v", err)
	}
	if !out.Tombstoned {
		t.Fatalf("expected party to remain tombstoned")
	}
	if len(fs.outboxEntries) != 0 {
		t.Fatalf("expected no NEW outbox entry on a repeated tombstone call, got %d", len(fs.outboxEntries))
	}
}

func TestAddIdentityDocument_FirstDocumentOfType_IsVersion1_NoSupersedes(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)

	d, err := s.AddIdentityDocument(context.Background(), AddIdentityDocumentInput{
		PartyID: "p1", DocumentType: domain.DocumentTypeDriversLicense, DocumentNumber: "D1234567",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Version != 1 {
		t.Fatalf("expected version 1, got %d", d.Version)
	}
	if d.SupersedesDocumentID != nil {
		t.Fatalf("expected no supersedes pointer on first document, got %v", *d.SupersedesDocumentID)
	}
}

func TestAddIdentityDocument_SecondDocumentOfSameType_IncrementsVersionAndSupersedes(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)

	first, err := s.AddIdentityDocument(context.Background(), AddIdentityDocumentInput{
		PartyID: "p1", DocumentType: domain.DocumentTypePassport, DocumentNumber: "X1111111",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	second, err := s.AddIdentityDocument(context.Background(), AddIdentityDocumentInput{
		PartyID: "p1", DocumentType: domain.DocumentTypePassport, DocumentNumber: "X2222222",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if second.Version != 2 {
		t.Fatalf("expected version 2, got %d", second.Version)
	}
	if second.SupersedesDocumentID == nil || *second.SupersedesDocumentID != first.ID {
		t.Fatalf("expected second document to supersede first (%s), got %v", first.ID, second.SupersedesDocumentID)
	}
	if len(fs.documents["p1"]) != 2 {
		t.Fatalf("expected both document versions retained (no delete/overwrite), got %d", len(fs.documents["p1"]))
	}
}

func TestAddIdentityDocument_DifferentTypesVersionIndependently(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)

	license, err := s.AddIdentityDocument(context.Background(), AddIdentityDocumentInput{
		PartyID: "p1", DocumentType: domain.DocumentTypeDriversLicense, DocumentNumber: "D1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	passport, err := s.AddIdentityDocument(context.Background(), AddIdentityDocumentInput{
		PartyID: "p1", DocumentType: domain.DocumentTypePassport, DocumentNumber: "P1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if license.Version != 1 || passport.Version != 1 {
		t.Fatalf("expected independent version-1 for each document type, got license=%d passport=%d", license.Version, passport.Version)
	}
}
