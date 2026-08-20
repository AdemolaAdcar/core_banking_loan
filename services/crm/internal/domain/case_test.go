package domain

import (
	"errors"
	"testing"
	"time"
)

func mustDur(t *testing.T, r ReasonCode) time.Duration {
	t.Helper()
	d, ok := SLADuration[r]
	if !ok {
		t.Fatalf("no SLA duration configured for %s", r)
	}
	return d
}

func TestNewCase_ComputesSLADueAtFromReasonCode(t *testing.T) {
	opened := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := NewCase("case-1", "party-1", nil, nil, ReasonPaymentDispute, opened)
	want := opened.Add(mustDur(t, ReasonPaymentDispute))
	if !c.SLADueAt.Equal(want) {
		t.Fatalf("expected slaDueAt %v, got %v", want, c.SLADueAt)
	}
	if c.Status != CaseStatusOpen || c.Version != 1 || c.Escalated {
		t.Fatalf("unexpected initial state: %+v", c)
	}
}

func TestUpdate_OpenToInProgress_Succeeds(t *testing.T) {
	c := NewCase("case-1", "party-1", nil, nil, ReasonGeneralInquiry, time.Now())
	newStatus := CaseStatusInProgress
	updated, changedFields, err := c.Update(c.Version, &newStatus, nil, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changedFields) != 1 || changedFields[0] != "status" {
		t.Fatalf("expected changedFields=[status], got %v", changedFields)
	}
	if updated.Status != CaseStatusInProgress {
		t.Fatalf("expected InProgress, got %s", updated.Status)
	}
	if updated.Version != c.Version+1 {
		t.Fatalf("expected version to increment, got %d", updated.Version)
	}
}

func TestUpdate_NoActualChange_IsNoOp_VersionUnchanged(t *testing.T) {
	c := NewCase("case-1", "party-1", nil, nil, ReasonGeneralInquiry, time.Now())
	sameStatus := CaseStatusOpen
	updated, changedFields, err := c.Update(c.Version, &sameStatus, nil, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changedFields) != 0 {
		t.Fatalf("expected no changed fields when the supplied status matches the current one, got %v", changedFields)
	}
	if updated.Version != c.Version {
		t.Fatalf("expected version unchanged on a no-op update, got %d vs %d", updated.Version, c.Version)
	}
}

func TestUpdate_DisallowedTransition_ResolvedBackToOpen_Rejected(t *testing.T) {
	c := NewCase("case-1", "party-1", nil, nil, ReasonGeneralInquiry, time.Now())
	inProgress := CaseStatusInProgress
	c, _, err := c.Update(c.Version, &inProgress, nil, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resolved := CaseStatusResolved
	c, _, err = c.Update(c.Version, &resolved, nil, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	backToOpen := CaseStatusOpen
	_, _, err = c.Update(c.Version, &backToOpen, nil, time.Now())
	var transErr *ErrInvalidTransition
	if !errors.As(err, &transErr) {
		t.Fatalf("expected ErrInvalidTransition going Resolved->Open, got %v", err)
	}
}

func TestUpdate_ToOrFromClosed_AlwaysRejected(t *testing.T) {
	c := NewCase("case-1", "party-1", nil, nil, ReasonGeneralInquiry, time.Now())
	c, _, err := c.Close("resolved externally", time.Now())
	if err != nil {
		t.Fatalf("unexpected error closing: %v", err)
	}

	newStatus := CaseStatusOpen
	_, _, err = c.Update(c.Version, &newStatus, nil, time.Now())
	var transErr *ErrInvalidTransition
	if !errors.As(err, &transErr) {
		t.Fatalf("expected ErrInvalidTransition updating a closed case, got %v", err)
	}
}

// --- Concurrent updates to the same case -----------------------------

func TestUpdate_StaleVersion_ConcurrentUpdateRejected(t *testing.T) {
	c := NewCase("case-1", "party-1", nil, nil, ReasonGeneralInquiry, time.Now())

	// Two CSRs both read the case at version 1, then both submit updates.
	newStatus := CaseStatusInProgress
	first, _, err := c.Update(1, &newStatus, nil, time.Now())
	if err != nil {
		t.Fatalf("first update unexpected error: %v", err)
	}
	if first.Version != 2 {
		t.Fatalf("expected version 2 after first update, got %d", first.Version)
	}

	// Second CSR's update still carries expectedVersion=1 (their stale
	// read from before the first CSR's write landed) -- applied against
	// `first`, which represents the case as it now actually is in the
	// store (version 2), this must be rejected.
	assignee := "csr.jdoe"
	_, _, err = first.Update(1, nil, &assignee, time.Now())
	if err == nil {
		t.Fatalf("expected the second, stale-version update to fail")
	}
	var staleErr *ErrStaleVersion
	if !errors.As(err, &staleErr) {
		t.Fatalf("expected ErrStaleVersion, got %v", err)
	}

	// The second CSR re-reads (gets `first`, version 2) and retries --
	// now succeeds.
	retried, _, err := first.Update(2, nil, &assignee, time.Now())
	if err != nil {
		t.Fatalf("retried update after re-read unexpected error: %v", err)
	}
	if retried.AssignedTo == nil || *retried.AssignedTo != assignee {
		t.Fatalf("expected assignee to be set after retry")
	}
}

// --- Reopening a closed case -------------------------------------------

func TestReopen_ClosedCase_Succeeds_ResetsEscalationAndSLA(t *testing.T) {
	opened := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCase("case-1", "party-1", nil, nil, ReasonHardshipRequest, opened)
	c, _, err := c.Close("customer satisfied", opened.Add(time.Hour))
	if err != nil {
		t.Fatalf("unexpected error closing: %v", err)
	}
	// Simulate the case having been escalated before closure.
	c.Escalated = true

	reopenedAt := opened.Add(72 * time.Hour)
	reopened, err := c.Reopen("customer called back with a new complaint", reopenedAt)
	if err != nil {
		t.Fatalf("unexpected error reopening: %v", err)
	}
	if reopened.Status != CaseStatusOpen {
		t.Fatalf("expected status Open after reopen, got %s", reopened.Status)
	}
	if reopened.Escalated {
		t.Fatalf("expected escalated to reset to false on reopen")
	}
	wantSLA := reopenedAt.Add(mustDur(t, ReasonHardshipRequest))
	if !reopened.SLADueAt.Equal(wantSLA) {
		t.Fatalf("expected fresh slaDueAt %v, got %v", wantSLA, reopened.SLADueAt)
	}
	if reopened.ReopenReason == nil || *reopened.ReopenReason == "" {
		t.Fatalf("expected reopen reason to be recorded")
	}
}

func TestReopen_NonClosedCase_Rejected_NotIdempotentNoOp(t *testing.T) {
	c := NewCase("case-1", "party-1", nil, nil, ReasonGeneralInquiry, time.Now())
	// Never closed -- reopening must be a genuine error, not a silent no-op.
	_, err := c.Reopen("mistaken reopen attempt", time.Now())
	var transErr *ErrInvalidTransition
	if !errors.As(err, &transErr) {
		t.Fatalf("expected ErrInvalidTransition reopening a case that was never closed, got %v", err)
	}
}

// --- Idempotent close ----------------------------------------------------

func TestClose_FirstCall_ChangesState(t *testing.T) {
	c := NewCase("case-1", "party-1", nil, nil, ReasonGeneralInquiry, time.Now())
	updated, changed, err := c.Close("resolved", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true on first close")
	}
	if updated.Status != CaseStatusClosed {
		t.Fatalf("expected status Closed, got %s", updated.Status)
	}
}

func TestClose_AlreadyClosed_IsIdempotentNoOp(t *testing.T) {
	c := NewCase("case-1", "party-1", nil, nil, ReasonGeneralInquiry, time.Now())
	c, _, err := c.Close("resolved", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	versionAfterFirstClose := c.Version

	c, changed, err := c.Close("resolved again", time.Now())
	if err != nil {
		t.Fatalf("expected idempotent success on second close, got error: %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false on a repeat close (idempotent no-op)")
	}
	if c.Version != versionAfterFirstClose {
		t.Fatalf("expected version unchanged on idempotent no-op close, got %d vs %d", c.Version, versionAfterFirstClose)
	}
}

// --- SLA escalation timing ------------------------------------------------

func TestIsPastSLA_BeforeDeadline_NotPast(t *testing.T) {
	opened := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCase("case-1", "party-1", nil, nil, ReasonPaymentDispute, opened)
	almostDue := c.SLADueAt.Add(-time.Minute)
	if c.IsPastSLA(almostDue) {
		t.Fatalf("expected not past SLA one minute before the deadline")
	}
}

func TestIsPastSLA_AfterDeadline_IsPast(t *testing.T) {
	opened := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCase("case-1", "party-1", nil, nil, ReasonPaymentDispute, opened)
	justPast := c.SLADueAt.Add(time.Minute)
	if !c.IsPastSLA(justPast) {
		t.Fatalf("expected past SLA one minute after the deadline")
	}
}

func TestIsPastSLA_AlreadyEscalated_NeverReportedAgain(t *testing.T) {
	opened := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCase("case-1", "party-1", nil, nil, ReasonPaymentDispute, opened)
	c.Escalated = true
	longPast := c.SLADueAt.Add(30 * 24 * time.Hour)
	if c.IsPastSLA(longPast) {
		t.Fatalf("expected an already-escalated case to never report past-SLA again")
	}
}

func TestIsPastSLA_ClosedCase_NeverEscalates(t *testing.T) {
	opened := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCase("case-1", "party-1", nil, nil, ReasonPaymentDispute, opened)
	c, _, err := c.Close("resolved", opened.Add(time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	longPast := c.SLADueAt.Add(30 * 24 * time.Hour)
	if c.IsPastSLA(longPast) {
		t.Fatalf("expected a closed case to never be flagged past-SLA, regardless of how much time passed")
	}
}

func TestIsPastSLA_ResolvedCase_NeverEscalates(t *testing.T) {
	opened := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCase("case-1", "party-1", nil, nil, ReasonPaymentDispute, opened)
	resolved := CaseStatusResolved
	c, _, err := c.Update(c.Version, &resolved, nil, opened.Add(time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	longPast := c.SLADueAt.Add(30 * 24 * time.Hour)
	if c.IsPastSLA(longPast) {
		t.Fatalf("expected a resolved (but not yet closed) case to never be flagged past-SLA")
	}
}

func TestReopen_GivesFreshSLAWindow_EvenIfOriginalWasAlreadyOverdue(t *testing.T) {
	opened := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCase("case-1", "party-1", nil, nil, ReasonPaymentDispute, opened) // 24h SLA
	// Close it long after its original SLA would have passed.
	c, _, err := c.Close("resolved late", opened.Add(72*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	reopenedAt := opened.Add(200 * time.Hour)
	reopened, err := c.Reopen("customer disputes resolution", reopenedAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reopened.IsPastSLA(reopenedAt.Add(time.Minute)) {
		t.Fatalf("expected the freshly reopened case to NOT be immediately past its new SLA window")
	}
	if !reopened.IsPastSLA(reopenedAt.Add(25 * time.Hour)) {
		t.Fatalf("expected the reopened case to be past SLA after its own fresh 24h window elapses")
	}
}
