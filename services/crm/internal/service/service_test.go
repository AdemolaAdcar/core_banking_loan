package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/domain"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func sequentialIDs(prefix string) func() string {
	n := 0
	return func() string {
		n++
		digits := []byte{}
		v := n
		if v == 0 {
			digits = []byte{'0'}
		}
		for v > 0 {
			digits = append([]byte{byte('0' + v%10)}, digits...)
			v /= 10
		}
		return prefix + "-" + string(digits)
	}
}

func newTestService(fs *fakeStore) *Service {
	s := New(fs)
	s.now = fixedClock(time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC))
	s.newID = sequentialIDs("id")
	return s
}

// --- LogInteraction --------------------------------------------------

func TestLogInteraction_WritesRowAndOutboxEntry(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)

	out, err := s.LogInteraction(context.Background(), LogInteractionInput{
		LoanAccountID: "loan-1", EventType: domain.EventAccountDisbursed, OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.ID == "" {
		t.Fatalf("expected a generated interaction ID")
	}
	if len(fs.interactions) != 1 {
		t.Fatalf("expected 1 interaction persisted, got %d", len(fs.interactions))
	}
	if len(fs.outboxEntries) != 1 || fs.outboxEntries[0].Topic != "crm.interaction.logged" {
		t.Fatalf("expected 1 crm.interaction.logged outbox entry, got %+v", fs.outboxEntries)
	}
}

// --- OpenCase ----------------------------------------------------------

func TestOpenCase_CreatesCaseAndLinksLoanAccount(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	loanID := "loan-1"

	out, err := s.OpenCase(context.Background(), OpenCaseInput{
		CaseID: "case-1", PartyID: "party-1", LoanAccountID: &loanID, ReasonCode: domain.ReasonPaymentDispute,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != domain.CaseStatusOpen || out.Version != 1 {
		t.Fatalf("unexpected initial case state: %+v", out)
	}
	if fs.loanAccountLink[loanID] != "party-1" {
		t.Fatalf("expected loan account linked to party-1, got %q", fs.loanAccountLink[loanID])
	}
	if len(fs.outboxEntries) != 1 || fs.outboxEntries[0].Topic != "crm.case.opened" {
		t.Fatalf("expected 1 crm.case.opened outbox entry, got %+v", fs.outboxEntries)
	}
}

// --- UpdateCase: concurrent updates to the same case --------------------

func TestUpdateCase_ConcurrentUpdate_SecondCallerGetsStaleVersionError(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	_, err := s.OpenCase(context.Background(), OpenCaseInput{CaseID: "case-1", PartyID: "party-1", ReasonCode: domain.ReasonGeneralInquiry})
	if err != nil {
		t.Fatalf("unexpected error opening case: %v", err)
	}

	// Both CSRs read the case at version 1.
	inProgress := domain.CaseStatusInProgress
	first, err := s.UpdateCase(context.Background(), UpdateCaseInput{CaseID: "case-1", ExpectedVersion: 1, NewStatus: &inProgress})
	if err != nil {
		t.Fatalf("first update unexpected error: %v", err)
	}
	if first.Version != 2 {
		t.Fatalf("expected version 2 after first update, got %d", first.Version)
	}

	// Second CSR still submits against version 1 (their stale read).
	assignee := "csr.jdoe"
	_, err = s.UpdateCase(context.Background(), UpdateCaseInput{CaseID: "case-1", ExpectedVersion: 1, NewAssignedTo: &assignee})
	if err == nil {
		t.Fatalf("expected the second, stale-version update to fail")
	}
	var staleErr *domain.ErrStaleVersion
	if !errors.As(err, &staleErr) {
		t.Fatalf("expected domain.ErrStaleVersion, got %v", err)
	}

	// Third CSR re-reads (gets version 2) and retries -- succeeds.
	retried, err := s.UpdateCase(context.Background(), UpdateCaseInput{CaseID: "case-1", ExpectedVersion: 2, NewAssignedTo: &assignee})
	if err != nil {
		t.Fatalf("retried update after re-read unexpected error: %v", err)
	}
	if retried.AssignedTo == nil || *retried.AssignedTo != assignee {
		t.Fatalf("expected assignee set after retry")
	}
}

func TestUpdateCase_NoActualChange_NoWriteNoEvent(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	_, err := s.OpenCase(context.Background(), OpenCaseInput{CaseID: "case-1", PartyID: "party-1", ReasonCode: domain.ReasonGeneralInquiry})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entriesBefore := len(fs.outboxEntries)

	sameStatus := domain.CaseStatusOpen
	_, err = s.UpdateCase(context.Background(), UpdateCaseInput{CaseID: "case-1", ExpectedVersion: 1, NewStatus: &sameStatus})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fs.outboxEntries) != entriesBefore {
		t.Fatalf("expected no new outbox entry for a no-op update")
	}
	if fs.cases["case-1"].Version != 1 {
		t.Fatalf("expected version unchanged on no-op update, got %d", fs.cases["case-1"].Version)
	}
}

// --- Reopening a closed case ---------------------------------------------

func TestReopenCase_ClosedCase_SucceedsAndPublishesEvent(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	_, err := s.OpenCase(context.Background(), OpenCaseInput{CaseID: "case-1", PartyID: "party-1", ReasonCode: domain.ReasonHardshipRequest})
	if err != nil {
		t.Fatalf("unexpected error opening: %v", err)
	}
	_, err = s.CloseCase(context.Background(), CloseCaseInput{CaseID: "case-1", Reason: "resolved"})
	if err != nil {
		t.Fatalf("unexpected error closing: %v", err)
	}

	reopened, err := s.ReopenCase(context.Background(), ReopenCaseInput{CaseID: "case-1", Reason: "customer called back"})
	if err != nil {
		t.Fatalf("unexpected error reopening: %v", err)
	}
	if reopened.Status != domain.CaseStatusOpen {
		t.Fatalf("expected status Open after reopen, got %s", reopened.Status)
	}

	var found bool
	for _, e := range fs.outboxEntries {
		if e.Topic == "crm.case.reopened" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a crm.case.reopened outbox entry, got %+v", fs.outboxEntries)
	}
}

func TestReopenCase_NeverClosed_Rejected(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	_, err := s.OpenCase(context.Background(), OpenCaseInput{CaseID: "case-1", PartyID: "party-1", ReasonCode: domain.ReasonGeneralInquiry})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = s.ReopenCase(context.Background(), ReopenCaseInput{CaseID: "case-1", Reason: "mistaken reopen"})
	var transErr *domain.ErrInvalidTransition
	if !errors.As(err, &transErr) {
		t.Fatalf("expected ErrInvalidTransition reopening a never-closed case, got %v", err)
	}
}

// --- CloseCase idempotency -------------------------------------------

func TestCloseCase_AlreadyClosed_IsIdempotentNoOp(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	_, err := s.OpenCase(context.Background(), OpenCaseInput{CaseID: "case-1", PartyID: "party-1", ReasonCode: domain.ReasonGeneralInquiry})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = s.CloseCase(context.Background(), CloseCaseInput{CaseID: "case-1", Reason: "resolved"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entriesAfterFirstClose := len(fs.outboxEntries)

	out, err := s.CloseCase(context.Background(), CloseCaseInput{CaseID: "case-1", Reason: "resolved again"})
	if err != nil {
		t.Fatalf("expected idempotent success on second close, got error: %v", err)
	}
	if out.Status != domain.CaseStatusClosed {
		t.Fatalf("expected case to remain Closed, got %s", out.Status)
	}
	if len(fs.outboxEntries) != entriesAfterFirstClose {
		t.Fatalf("expected no new outbox entry on a repeated close")
	}
}

// --- SLA escalation timing ---------------------------------------------

func TestEvaluateSLABreaches_PastDueCase_EscalatedAndEventPublished(t *testing.T) {
	fs := newFakeStore()
	opened := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(fs)
	s.now = fixedClock(opened)
	s.newID = sequentialIDs("id")

	_, err := s.OpenCase(context.Background(), OpenCaseInput{CaseID: "case-1", PartyID: "party-1", ReasonCode: domain.ReasonPaymentDispute}) // 24h SLA
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Advance the clock past the 24h SLA window.
	s.now = fixedClock(opened.Add(25 * time.Hour))
	n, err := s.EvaluateSLABreaches(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 case escalated, got %d", n)
	}
	if !fs.cases["case-1"].Escalated {
		t.Fatalf("expected case marked escalated in the store")
	}

	var found bool
	for _, e := range fs.outboxEntries {
		if e.Topic == "crm.case.escalated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a crm.case.escalated outbox entry, got %+v", fs.outboxEntries)
	}
}

func TestEvaluateSLABreaches_NotYetDueCase_NotEscalated(t *testing.T) {
	fs := newFakeStore()
	opened := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(fs)
	s.now = fixedClock(opened)
	s.newID = sequentialIDs("id")

	_, err := s.OpenCase(context.Background(), OpenCaseInput{CaseID: "case-1", PartyID: "party-1", ReasonCode: domain.ReasonPaymentDispute}) // 24h SLA
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Still within the 24h window.
	s.now = fixedClock(opened.Add(1 * time.Hour))
	n, err := s.EvaluateSLABreaches(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 cases escalated before the SLA deadline, got %d", n)
	}
}

func TestEvaluateSLABreaches_AlreadyEscalated_NotEscalatedAgain(t *testing.T) {
	fs := newFakeStore()
	opened := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(fs)
	s.now = fixedClock(opened)
	s.newID = sequentialIDs("id")

	_, err := s.OpenCase(context.Background(), OpenCaseInput{CaseID: "case-1", PartyID: "party-1", ReasonCode: domain.ReasonPaymentDispute})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s.now = fixedClock(opened.Add(25 * time.Hour))
	if _, err := s.EvaluateSLABreaches(context.Background()); err != nil {
		t.Fatalf("unexpected error on first sweep: %v", err)
	}
	entriesAfterFirstSweep := len(fs.outboxEntries)

	// A second sweep, further past due, must not escalate (and re-publish) again.
	s.now = fixedClock(opened.Add(72 * time.Hour))
	n, err := s.EvaluateSLABreaches(context.Background())
	if err != nil {
		t.Fatalf("unexpected error on second sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 newly escalated on the second sweep, got %d", n)
	}
	if len(fs.outboxEntries) != entriesAfterFirstSweep {
		t.Fatalf("expected no additional crm.case.escalated entries on the second sweep")
	}
}

func TestEvaluateSLABreaches_ClosedCase_NeverEscalated(t *testing.T) {
	fs := newFakeStore()
	opened := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(fs)
	s.now = fixedClock(opened)
	s.newID = sequentialIDs("id")

	_, err := s.OpenCase(context.Background(), OpenCaseInput{CaseID: "case-1", PartyID: "party-1", ReasonCode: domain.ReasonPaymentDispute})
	if err != nil {
		t.Fatalf("unexpected error opening: %v", err)
	}
	if _, err := s.CloseCase(context.Background(), CloseCaseInput{CaseID: "case-1", Reason: "resolved fast"}); err != nil {
		t.Fatalf("unexpected error closing: %v", err)
	}

	s.now = fixedClock(opened.Add(72 * time.Hour))
	n, err := s.EvaluateSLABreaches(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected a closed case to never be escalated, got %d", n)
	}
}

// --- Case notes: access logging -----------------------------------------

func TestListCaseNotes_LogsAccessWithActorAndTimestamp(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	_, err := s.OpenCase(context.Background(), OpenCaseInput{CaseID: "case-1", PartyID: "party-1", ReasonCode: domain.ReasonGeneralInquiry})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := s.AddCaseNote(context.Background(), AddCaseNoteInput{CaseID: "case-1", AuthorID: "csr.jdoe", Body: "customer called about payment"}); err != nil {
		t.Fatalf("unexpected error adding note: %v", err)
	}

	notes, err := s.ListCaseNotes(context.Background(), "csr.reviewer", "case-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(notes) != 1 || notes[0].Body != "customer called about payment" {
		t.Fatalf("unexpected notes: %+v", notes)
	}
	if len(fs.accessLog) != 1 {
		t.Fatalf("expected exactly 1 access-log entry, got %d", len(fs.accessLog))
	}
	if fs.accessLog[0].ActorSubject != "csr.reviewer" || fs.accessLog[0].ResourceType != "CaseNote" || fs.accessLog[0].ResourceID != "case-1" {
		t.Fatalf("unexpected access-log entry: %+v", fs.accessLog[0])
	}
}

// --- Relationship manager -----------------------------------------------

func TestAssignRelationshipManager_FirstAssignment_WritesAndPublishes(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)

	out, err := s.AssignRelationshipManager(context.Background(), AssignRelationshipManagerInput{PartyID: "party-1", RelationshipManagerID: "rm-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.RelationshipManagerID == nil || *out.RelationshipManagerID != "rm-1" {
		t.Fatalf("unexpected assignment: %+v", out)
	}
	if len(fs.outboxEntries) != 1 || fs.outboxEntries[0].Topic != "crm.relationshipManager.assigned" {
		t.Fatalf("expected 1 crm.relationshipManager.assigned outbox entry, got %+v", fs.outboxEntries)
	}
}

func TestAssignRelationshipManager_SameRMAgain_IsNoOp(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	if _, err := s.AssignRelationshipManager(context.Background(), AssignRelationshipManagerInput{PartyID: "party-1", RelationshipManagerID: "rm-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entriesAfterFirst := len(fs.outboxEntries)

	if _, err := s.AssignRelationshipManager(context.Background(), AssignRelationshipManagerInput{PartyID: "party-1", RelationshipManagerID: "rm-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fs.outboxEntries) != entriesAfterFirst {
		t.Fatalf("expected no new outbox entry for reassigning the same RM")
	}
}

// --- Communication preferences -----------------------------------------

func TestGetCommunicationPreferences_NeverSet_ReturnsConservativeDefault(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)

	prefs, err := s.GetCommunicationPreferences(context.Background(), "party-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prefs.PreferredChannel != nil || prefs.EmailOptIn || prefs.SMSOptIn || prefs.PhoneOptIn || prefs.MailOptIn || prefs.DoNotContact {
		t.Fatalf("expected all-conservative defaults, got %+v", prefs)
	}
}

func TestUpdateCommunicationPreferences_WritesAndPublishes(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	channel := domain.ChannelEmail

	out, err := s.UpdateCommunicationPreferences(context.Background(), UpdateCommunicationPreferencesInput{
		PartyID: "party-1", PreferredChannel: &channel, EmailOptIn: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.PreferredChannel == nil || *out.PreferredChannel != domain.ChannelEmail || !out.EmailOptIn {
		t.Fatalf("unexpected preferences: %+v", out)
	}
	if len(fs.outboxEntries) != 1 || fs.outboxEntries[0].Topic != "crm.communicationPreferences.updated" {
		t.Fatalf("expected 1 crm.communicationPreferences.updated outbox entry, got %+v", fs.outboxEntries)
	}
}

// --- Customer 360 --------------------------------------------------------

func TestGetCustomer360_DerivesLoanAccountStatusFromLatestInteraction_AndLogsAccess(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	loanID := "loan-1"

	if _, err := s.OpenCase(context.Background(), OpenCaseInput{CaseID: "case-1", PartyID: "party-1", LoanAccountID: &loanID, ReasonCode: domain.ReasonGeneralInquiry}); err != nil {
		t.Fatalf("unexpected error opening case: %v", err)
	}
	if _, err := s.LogInteraction(context.Background(), LogInteractionInput{LoanAccountID: loanID, EventType: domain.EventAccountBooked, OccurredAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := s.LogInteraction(context.Background(), LogInteractionInput{LoanAccountID: loanID, EventType: domain.EventAccountDisbursed, OccurredAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c360, err := s.GetCustomer360(context.Background(), "csr.reviewer", "party-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c360.LoanAccountSummaries) != 1 {
		t.Fatalf("expected 1 loan account summary, got %d", len(c360.LoanAccountSummaries))
	}
	if c360.LoanAccountSummaries[0].Status != domain.LoanAccountDisbursed {
		t.Fatalf("expected status Disbursed (the LATER interaction), got %s", c360.LoanAccountSummaries[0].Status)
	}
	if len(c360.OpenCases) != 1 {
		t.Fatalf("expected 1 open case, got %d", len(c360.OpenCases))
	}

	var found bool
	for _, e := range fs.accessLog {
		if e.ResourceType == "Customer360" && e.ResourceID == "party-1" && e.ActorSubject == "csr.reviewer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a Customer360 access-log entry, got %+v", fs.accessLog)
	}
}
