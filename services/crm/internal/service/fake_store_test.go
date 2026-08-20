package service

import (
	"context"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/outbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/store"
)

// fakeStore is an in-memory, non-concurrent-safe store.Store/store.Tx
// double used only in tests -- enough to verify the SERVICE layer's
// orchestration logic without a live Postgres instance.
type fakeStore struct {
	cases           map[string]domain.ServiceCase
	interactions    []domain.Interaction
	caseNotes       map[string][]domain.CaseNote
	commPrefs       map[string]domain.CommunicationPreferences
	rmAssignments   map[string]domain.RelationshipManagerAssignment
	loanAccountLink map[string]string // loanAccountID -> partyID
	outboxEntries   []outbox.Entry
	accessLog       []accessLogEntry
}

type accessLogEntry struct {
	ActorSubject, ResourceType, ResourceID string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		cases:           map[string]domain.ServiceCase{},
		caseNotes:       map[string][]domain.CaseNote{},
		commPrefs:       map[string]domain.CommunicationPreferences{},
		rmAssignments:   map[string]domain.RelationshipManagerAssignment{},
		loanAccountLink: map[string]string{},
	}
}

func (f *fakeStore) WithinTx(ctx context.Context, fn func(store.Tx) error) error {
	// Snapshot for rollback-on-error semantics.
	snapCases := cloneCases(f.cases)
	snapNotes := cloneNotes(f.caseNotes)
	snapPrefs := clonePrefs(f.commPrefs)
	snapRM := cloneRM(f.rmAssignments)
	snapLinks := cloneLinks(f.loanAccountLink)
	snapOutbox := append([]outbox.Entry(nil), f.outboxEntries...)
	snapAccess := append([]accessLogEntry(nil), f.accessLog...)
	snapInteractions := append([]domain.Interaction(nil), f.interactions...)

	if err := fn(f); err != nil {
		f.cases = snapCases
		f.caseNotes = snapNotes
		f.commPrefs = snapPrefs
		f.rmAssignments = snapRM
		f.loanAccountLink = snapLinks
		f.outboxEntries = snapOutbox
		f.accessLog = snapAccess
		f.interactions = snapInteractions
		return err
	}
	return nil
}

func cloneCases(m map[string]domain.ServiceCase) map[string]domain.ServiceCase {
	out := make(map[string]domain.ServiceCase, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
func cloneNotes(m map[string][]domain.CaseNote) map[string][]domain.CaseNote {
	out := make(map[string][]domain.CaseNote, len(m))
	for k, v := range m {
		out[k] = append([]domain.CaseNote(nil), v...)
	}
	return out
}
func clonePrefs(m map[string]domain.CommunicationPreferences) map[string]domain.CommunicationPreferences {
	out := make(map[string]domain.CommunicationPreferences, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
func cloneRM(m map[string]domain.RelationshipManagerAssignment) map[string]domain.RelationshipManagerAssignment {
	out := make(map[string]domain.RelationshipManagerAssignment, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
func cloneLinks(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// --- Store read methods ------------------------------------------------

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

func (f *fakeStore) GetIdempotentResponse(_ context.Context, _ string) (bool, []byte, error) {
	return false, nil, nil
}

func (f *fakeStore) ListUnpublished(_ context.Context, limit int) ([]outbox.Entry, error) {
	if len(f.outboxEntries) > limit {
		return f.outboxEntries[:limit], nil
	}
	return f.outboxEntries, nil
}

func (f *fakeStore) MarkPublished(_ context.Context, _ []string) error { return nil }

// --- Tx write methods ----------------------------------------------------

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

func (f *fakeStore) RecordAccess(_ context.Context, actorSubject, resourceType, resourceID string, _ time.Time) error {
	f.accessLog = append(f.accessLog, accessLogEntry{actorSubject, resourceType, resourceID})
	return nil
}

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

func (f *fakeStore) SaveIdempotentResponse(_ context.Context, _, _ string, _ []byte) error {
	return nil
}

func (f *fakeStore) InsertOutboxEntry(_ context.Context, e outbox.Entry) error {
	f.outboxEntries = append(f.outboxEntries, e)
	return nil
}
