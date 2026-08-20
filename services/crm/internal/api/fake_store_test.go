package api

import (
	"context"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/store"
)

// fakeStore is a minimal in-memory store.Store/store.Tx double for
// exercising the HTTP layer without a live Postgres instance -- the same
// role Party/CIF's api package fake plays for its own handler tests.
type fakeStore struct {
	cases           map[string]domain.ServiceCase
	interactions    []domain.Interaction
	caseNotes       map[string][]domain.CaseNote
	commPrefs       map[string]domain.CommunicationPreferences
	rmAssignments   map[string]domain.RelationshipManagerAssignment
	loanAccountLink map[string]string
	idempotencyKeys map[string][]byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		cases:           map[string]domain.ServiceCase{},
		caseNotes:       map[string][]domain.CaseNote{},
		commPrefs:       map[string]domain.CommunicationPreferences{},
		rmAssignments:   map[string]domain.RelationshipManagerAssignment{},
		loanAccountLink: map[string]string{},
		idempotencyKeys: map[string][]byte{},
	}
}

func (f *fakeStore) WithinTx(_ context.Context, fn func(store.Tx) error) error {
	return fn(f)
}

func (f *fakeStore) GetCase(_ context.Context, caseID string) (domain.ServiceCase, error) {
	c, ok := f.cases[caseID]
	if !ok {
		return domain.ServiceCase{}, store.ErrNotFound
	}
	return c, nil
}

func (f *fakeStore) ListCaseNotes(_ context.Context, caseID string) ([]domain.CaseNote, error) {
	return f.caseNotes[caseID], nil
}

func (f *fakeStore) GetCommunicationPreferences(_ context.Context, partyID string) (domain.CommunicationPreferences, bool, error) {
	p, ok := f.commPrefs[partyID]
	return p, ok, nil
}

func (f *fakeStore) GetRelationshipManagerAssignment(_ context.Context, partyID string) (domain.RelationshipManagerAssignment, error) {
	a, ok := f.rmAssignments[partyID]
	if !ok {
		return domain.RelationshipManagerAssignment{PartyID: partyID}, nil
	}
	return a, nil
}

func (f *fakeStore) ListLoanAccountIDsForParty(_ context.Context, partyID string) ([]string, error) {
	var out []string
	for loanID, pID := range f.loanAccountLink {
		if pID == partyID {
			out = append(out, loanID)
		}
	}
	return out, nil
}

func (f *fakeStore) LatestInteractionPerLoanAccount(_ context.Context, loanAccountIDs []string) (map[string]domain.Interaction, error) {
	want := map[string]bool{}
	for _, id := range loanAccountIDs {
		want[id] = true
	}
	out := map[string]domain.Interaction{}
	for _, i := range f.interactions {
		if !want[i.LoanAccountID] {
			continue
		}
		existing, ok := out[i.LoanAccountID]
		if !ok || i.OccurredAt.After(existing.OccurredAt) {
			out[i.LoanAccountID] = i
		}
	}
	return out, nil
}

func (f *fakeStore) ListRecentInteractionsForLoanAccounts(_ context.Context, loanAccountIDs []string, limit int) ([]domain.Interaction, error) {
	want := map[string]bool{}
	for _, id := range loanAccountIDs {
		want[id] = true
	}
	var out []domain.Interaction
	for _, i := range f.interactions {
		if want[i.LoanAccountID] {
			out = append(out, i)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) ListOpenCasesForParty(_ context.Context, partyID string) ([]domain.ServiceCase, error) {
	var out []domain.ServiceCase
	for _, c := range f.cases {
		if c.PartyID == partyID && c.Status != domain.CaseStatusClosed {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeStore) ListCasesPastSLA(_ context.Context, now time.Time, limit int) ([]domain.ServiceCase, error) {
	var out []domain.ServiceCase
	for _, c := range f.cases {
		if c.IsPastSLA(now) {
			out = append(out, c)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) GetIdempotentResponse(_ context.Context, key string) (bool, []byte, error) {
	b, ok := f.idempotencyKeys[key]
	if !ok {
		return false, nil, nil
	}
	return true, b, nil
}

func (f *fakeStore) ListUnpublished(_ context.Context, _ int) ([]outbox.Entry, error) {
	return nil, nil
}
func (f *fakeStore) MarkPublished(_ context.Context, _ []string) error { return nil }

func (f *fakeStore) CreateInteraction(_ context.Context, i domain.Interaction) error {
	f.interactions = append(f.interactions, i)
	return nil
}

func (f *fakeStore) CreateCase(_ context.Context, c domain.ServiceCase) error {
	f.cases[c.ID] = c
	return nil
}

func (f *fakeStore) UpdateCaseConditional(_ context.Context, c domain.ServiceCase, priorVersion int) error {
	existing, ok := f.cases[c.ID]
	if !ok || existing.Version != priorVersion {
		return store.ErrStaleVersion
	}
	f.cases[c.ID] = c
	return nil
}

func (f *fakeStore) AddCaseNote(_ context.Context, n domain.CaseNote) error {
	f.caseNotes[n.CaseID] = append(f.caseNotes[n.CaseID], n)
	return nil
}

func (f *fakeStore) RecordAccess(_ context.Context, _, _, _ string, _ time.Time) error { return nil }

func (f *fakeStore) UpsertCommunicationPreferences(_ context.Context, p domain.CommunicationPreferences) error {
	f.commPrefs[p.PartyID] = p
	return nil
}

func (f *fakeStore) AssignRelationshipManager(_ context.Context, a domain.RelationshipManagerAssignment) error {
	f.rmAssignments[a.PartyID] = a
	return nil
}

func (f *fakeStore) LinkLoanAccountToParty(_ context.Context, loanAccountID, partyID string) error {
	if _, exists := f.loanAccountLink[loanAccountID]; !exists {
		f.loanAccountLink[loanAccountID] = partyID
	}
	return nil
}

func (f *fakeStore) SaveIdempotentResponse(_ context.Context, key, _ string, responseJSON []byte) error {
	f.idempotencyKeys[key] = responseJSON
	return nil
}

func (f *fakeStore) InsertOutboxEntry(_ context.Context, _ outbox.Entry) error { return nil }
