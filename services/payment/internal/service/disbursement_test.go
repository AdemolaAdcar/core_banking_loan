package service

import (
	"context"
	"errors"
	"testing"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
)

func TestInitiateDisbursement_Succeeds(t *testing.T) {
	svc, st, _, _ := newTestService()
	out, err := svc.InitiateDisbursement(context.Background(), InitiateDisbursementInput{
		IdempotencyKey: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(150000),
	})
	if err != nil {
		t.Fatalf("InitiateDisbursement: %v", err)
	}
	if out.Status != domain.StatusSubmitted {
		t.Fatalf("expected Submitted, got %s", out.Status)
	}
	if out.RailReference == nil {
		t.Fatalf("expected a rail reference to be recorded")
	}
	if len(st.outboxEntries) != 0 {
		t.Fatalf("expected NO outbox event at Submitted time -- only a later confirmed/failed reconciliation publishes one, got %d", len(st.outboxEntries))
	}
}

func TestInitiateDisbursement_IdempotentReplay_DoesNotCallRailTwice(t *testing.T) {
	svc, _, rail, _ := newTestService()
	in := InitiateDisbursementInput{IdempotencyKey: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(150000)}

	first, err := svc.InitiateDisbursement(context.Background(), in)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := svc.InitiateDisbursement(context.Background(), in)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.RailReference == nil || first.RailReference == nil || *second.RailReference != *first.RailReference {
		t.Fatalf("expected the replay to return the SAME instruction, got %+v vs %+v", second, first)
	}

	// Confirm the rail itself only ever saw ONE Initiate call's worth of
	// state -- Confirm still resolves to a single outcome record, not
	// two independent submissions.
	outcome, err := rail.Confirm(context.Background(), *first.RailReference)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if outcome.Status != railclient.OutcomeExecuted {
		t.Fatalf("expected the sandbox's single deterministic outcome, got %s", outcome.Status)
	}
}

func TestInitiateDisbursement_MissingJournalEntry_Rejected(t *testing.T) {
	svc, _, _, _ := newTestService()
	_, err := svc.InitiateDisbursement(context.Background(), InitiateDisbursementInput{
		IdempotencyKey: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", Amount: usd(1000),
	})
	if !errors.Is(err, ErrMissingJournalEntry) {
		t.Fatalf("expected ErrMissingJournalEntry, got %v", err)
	}
}

func TestInitiateDisbursement_RailRejection_NothingPersisted(t *testing.T) {
	svc, st, rail, _ := newTestService()
	rail.SetNextInitiateErr(railclient.ErrRailRejected)

	_, err := svc.InitiateDisbursement(context.Background(), InitiateDisbursementInput{
		IdempotencyKey: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(1000),
	})
	if !errors.Is(err, railclient.ErrRailRejected) {
		t.Fatalf("expected ErrRailRejected to propagate, got %v", err)
	}
	if _, err := st.GetPaymentInstruction(context.Background(), "instr-1"); err == nil {
		t.Fatalf("expected NOTHING to be persisted after a rail rejection")
	}

	// A retry after the rejection is fixed must still work -- proves the
	// failed attempt didn't leave the instruction ID permanently stuck.
	out, err := svc.InitiateDisbursement(context.Background(), InitiateDisbursementInput{
		IdempotencyKey: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(1000),
	})
	if err != nil {
		t.Fatalf("retry after fixing the rail: %v", err)
	}
	if out.Status != domain.StatusSubmitted {
		t.Fatalf("expected Submitted on the successful retry, got %s", out.Status)
	}
}

func TestGetPaymentInstruction_NotFound(t *testing.T) {
	svc, _, _, _ := newTestService()
	_, err := svc.GetPaymentInstruction(context.Background(), "no-such-instruction")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
