package service

import (
	"context"
	"testing"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
)

func TestRunReconciliationSweep_ConfirmsExecuted(t *testing.T) {
	svc, st, _, _ := newTestService()
	out, err := svc.InitiateDisbursement(context.Background(), InitiateDisbursementInput{
		IdempotencyKey: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(1000),
	})
	if err != nil {
		t.Fatalf("InitiateDisbursement: %v", err)
	}

	summary, err := svc.RunReconciliationSweep(context.Background())
	if err != nil {
		t.Fatalf("RunReconciliationSweep: %v", err)
	}
	if summary.Checked != 1 || summary.Confirmed != 1 {
		t.Fatalf("expected checked=1 confirmed=1, got %+v", summary)
	}

	reloaded, err := svc.GetPaymentInstruction(context.Background(), out.InstructionID)
	if err != nil {
		t.Fatalf("GetPaymentInstruction: %v", err)
	}
	if reloaded.Status != domain.StatusExecuted {
		t.Fatalf("expected Executed, got %s", reloaded.Status)
	}

	if len(st.outboxEntries) != 1 || st.outboxEntries[0].Topic != "payment.disbursement.confirmed" {
		t.Fatalf("expected exactly one payment.disbursement.confirmed outbox entry, got %+v", st.outboxEntries)
	}
}

func TestRunReconciliationSweep_StillPending_NoTransition(t *testing.T) {
	svc, _, rail, _ := newTestService()
	rail.SetOutcome("instr-1", railclient.Outcome{Status: railclient.OutcomePending})
	out, err := svc.InitiateDisbursement(context.Background(), InitiateDisbursementInput{
		IdempotencyKey: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(1000),
	})
	if err != nil {
		t.Fatalf("InitiateDisbursement: %v", err)
	}

	summary, err := svc.RunReconciliationSweep(context.Background())
	if err != nil {
		t.Fatalf("RunReconciliationSweep: %v", err)
	}
	if summary.StillPending != 1 || summary.Confirmed != 0 {
		t.Fatalf("expected pending=1 confirmed=0, got %+v", summary)
	}
	reloaded, err := svc.GetPaymentInstruction(context.Background(), out.InstructionID)
	if err != nil {
		t.Fatalf("GetPaymentInstruction: %v", err)
	}
	if reloaded.Status != domain.StatusSubmitted {
		t.Fatalf("expected Submitted (unchanged), got %s", reloaded.Status)
	}
}

func TestRunReconciliationSweep_Failed_PublishesFailedEvent(t *testing.T) {
	svc, st, rail, _ := newTestService()
	reason := domain.ReasonRailRejected
	rail.SetOutcome("instr-1", railclient.Outcome{Status: railclient.OutcomeFailed, FailureReason: &reason})
	if _, err := svc.InitiateDisbursement(context.Background(), InitiateDisbursementInput{
		IdempotencyKey: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(1000),
	}); err != nil {
		t.Fatalf("InitiateDisbursement: %v", err)
	}

	summary, err := svc.RunReconciliationSweep(context.Background())
	if err != nil {
		t.Fatalf("RunReconciliationSweep: %v", err)
	}
	if summary.Failed != 1 {
		t.Fatalf("expected failed=1, got %+v", summary)
	}
	reloaded, err := svc.GetPaymentInstruction(context.Background(), "instr-1")
	if err != nil {
		t.Fatalf("GetPaymentInstruction: %v", err)
	}
	if reloaded.Status != domain.StatusFailed || reloaded.FailureReason == nil || *reloaded.FailureReason != domain.ReasonRailRejected {
		t.Fatalf("expected Failed/RAIL_REJECTED, got %+v", reloaded)
	}
	if len(st.outboxEntries) != 1 || st.outboxEntries[0].Topic != "payment.disbursement.failed" {
		t.Fatalf("expected exactly one payment.disbursement.failed outbox entry, got %+v", st.outboxEntries)
	}
}

// TestRunReconciliationSweep_RailForgotReference_FiledAsUnmatchedException
// covers the sweep's own defensive path: a rail reference THIS SERVICE
// stored is no longer recognized by the rail itself.
func TestRunReconciliationSweep_RailForgotReference_FiledAsUnmatchedException(t *testing.T) {
	svc, st, rail, _ := newTestService()
	if _, err := svc.InitiateDisbursement(context.Background(), InitiateDisbursementInput{
		IdempotencyKey: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(1000),
	}); err != nil {
		t.Fatalf("InitiateDisbursement: %v", err)
	}
	rail.SetNextConfirmErr(railclient.ErrNotFound)

	summary, err := svc.RunReconciliationSweep(context.Background())
	if err != nil {
		t.Fatalf("RunReconciliationSweep: %v", err)
	}
	if summary.Unmatched != 1 {
		t.Fatalf("expected unmatched=1, got %+v", summary)
	}
	if len(st.exceptions) != 1 || st.exceptions[0].Kind != domain.ExceptionUnmatchedConfirmation {
		t.Fatalf("expected exactly one UNMATCHED_CONFIRMATION exception, got %+v", st.exceptions)
	}
}

// TestProcessConfirmation_UnmatchedRailReference is this role's literal
// ground rule: "An unmatched confirmation is logged as an exception,
// never posted speculatively."
func TestProcessConfirmation_UnmatchedRailReference_FiledAsExceptionNotPosted(t *testing.T) {
	svc, st, _, _ := newTestService()
	err := svc.ProcessConfirmation(context.Background(), "unknown-rail-ref", railclient.Outcome{Status: railclient.OutcomeExecuted})
	if err != nil {
		t.Fatalf("ProcessConfirmation: %v", err)
	}
	if len(st.exceptions) != 1 || st.exceptions[0].Kind != domain.ExceptionUnmatchedConfirmation {
		t.Fatalf("expected exactly one UNMATCHED_CONFIRMATION exception, got %+v", st.exceptions)
	}
	if len(st.outboxEntries) != 0 {
		t.Fatalf("expected ZERO events published for an unmatched confirmation -- never posted speculatively, got %d", len(st.outboxEntries))
	}
	if len(st.instructions) != 0 {
		t.Fatalf("expected NO PaymentInstruction to have been fabricated, got %+v", st.instructions)
	}
}

// TestApplyOutcome_ConflictingTerminalStatus_FiledAsException proves a
// SECOND, contradicting terminal outcome for an already-resolved
// instruction is treated as a data-integrity anomaly, not silently
// overwritten.
func TestApplyOutcome_ConflictingTerminalStatus_FiledAsException(t *testing.T) {
	svc, st, _, _ := newTestService()
	out, err := svc.InitiateDisbursement(context.Background(), InitiateDisbursementInput{
		IdempotencyKey: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(1000),
	})
	if err != nil {
		t.Fatalf("InitiateDisbursement: %v", err)
	}
	// First confirmation: applies normally (Submitted -> Executed).
	if err := svc.ProcessConfirmation(context.Background(), *out.RailReference, railclient.Outcome{Status: railclient.OutcomeExecuted}); err != nil {
		t.Fatalf("first ProcessConfirmation: %v", err)
	}
	// Second, CONTRADICTING confirmation for the same reference: Executed
	// cannot transition to Failed (only to Returned) -- an anomaly.
	if err := svc.ProcessConfirmation(context.Background(), *out.RailReference, railclient.Outcome{Status: railclient.OutcomeFailed}); err != nil {
		t.Fatalf("second ProcessConfirmation: %v", err)
	}

	reloaded, err := svc.GetPaymentInstruction(context.Background(), out.InstructionID)
	if err != nil {
		t.Fatalf("GetPaymentInstruction: %v", err)
	}
	if reloaded.Status != domain.StatusExecuted {
		t.Fatalf("expected the ORIGINAL Executed status to remain untouched, got %s", reloaded.Status)
	}
	if len(st.exceptions) != 1 {
		t.Fatalf("expected exactly one reconciliation exception for the conflicting report, got %+v", st.exceptions)
	}
	if len(st.outboxEntries) != 1 {
		t.Fatalf("expected only the FIRST confirmation to have published an event, got %d", len(st.outboxEntries))
	}
}

func TestApplyOutcome_SameOutcomeTwice_IdempotentNoOp(t *testing.T) {
	svc, st, _, _ := newTestService()
	out, err := svc.InitiateDisbursement(context.Background(), InitiateDisbursementInput{
		IdempotencyKey: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(1000),
	})
	if err != nil {
		t.Fatalf("InitiateDisbursement: %v", err)
	}
	if err := svc.ProcessConfirmation(context.Background(), *out.RailReference, railclient.Outcome{Status: railclient.OutcomeExecuted}); err != nil {
		t.Fatalf("first ProcessConfirmation: %v", err)
	}
	if err := svc.ProcessConfirmation(context.Background(), *out.RailReference, railclient.Outcome{Status: railclient.OutcomeExecuted}); err != nil {
		t.Fatalf("re-delivered ProcessConfirmation: %v", err)
	}
	if len(st.outboxEntries) != 1 {
		t.Fatalf("expected a re-delivered, identical confirmation NOT to publish a second event, got %d", len(st.outboxEntries))
	}
	if len(st.exceptions) != 0 {
		t.Fatalf("expected no exception for a routine re-delivered identical confirmation, got %+v", st.exceptions)
	}
}
