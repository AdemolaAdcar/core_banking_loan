package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
)

func usd(amount int64) domain.Money { return domain.Money{Amount: amount, Currency: "USD"} }

func TestInitiate_DefaultsToImmediateExecuted(t *testing.T) {
	sb := New()
	sub, err := sb.Initiate(context.Background(), railclient.InitiateInput{InstructionID: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(1000)})
	if err != nil {
		t.Fatalf("Initiate: %v", err)
	}
	outcome, err := sb.Confirm(context.Background(), sub.RailReference)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if outcome.Status != railclient.OutcomeExecuted {
		t.Fatalf("expected the deterministic default (Executed), got %s", outcome.Status)
	}
}

func TestInitiate_IdempotentReplay(t *testing.T) {
	sb := New()
	in := railclient.InitiateInput{InstructionID: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(1000)}
	first, err := sb.Initiate(context.Background(), in)
	if err != nil {
		t.Fatalf("first Initiate: %v", err)
	}
	second, err := sb.Initiate(context.Background(), in)
	if err != nil {
		t.Fatalf("replay Initiate: %v", err)
	}
	if second.RailReference != first.RailReference {
		t.Fatalf("expected the replay to return the SAME rail reference, got %s vs %s", second.RailReference, first.RailReference)
	}
}

func TestInitiate_SameKeyDifferentPayload_Rejected(t *testing.T) {
	sb := New()
	if _, err := sb.Initiate(context.Background(), railclient.InitiateInput{InstructionID: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(1000)}); err != nil {
		t.Fatalf("first Initiate: %v", err)
	}
	_, err := sb.Initiate(context.Background(), railclient.InitiateInput{InstructionID: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(9999)})
	if !errors.Is(err, railclient.ErrDuplicateInstruction) {
		t.Fatalf("expected ErrDuplicateInstruction, got %v", err)
	}
}

func TestSetOutcome_ArmsConfirmBeforeInitiate(t *testing.T) {
	sb := New()
	reason := domain.ReasonRailRejected
	sb.SetOutcome("instr-1", railclient.Outcome{Status: railclient.OutcomeFailed, FailureReason: &reason})

	sub, err := sb.Initiate(context.Background(), railclient.InitiateInput{InstructionID: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(1000)})
	if err != nil {
		t.Fatalf("Initiate: %v", err)
	}
	outcome, err := sb.Confirm(context.Background(), sub.RailReference)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if outcome.Status != railclient.OutcomeFailed || outcome.FailureReason == nil || *outcome.FailureReason != domain.ReasonRailRejected {
		t.Fatalf("expected the armed Failed/RAIL_REJECTED outcome, got %+v", outcome)
	}
}

func TestConfirm_UnknownRailReference_NotFound(t *testing.T) {
	sb := New()
	_, err := sb.Confirm(context.Background(), "no-such-reference")
	if !errors.Is(err, railclient.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestReceiveInbound_FiltersBySinceAndSorts(t *testing.T) {
	sb := New()
	t0 := time.Unix(0, 0)
	sb.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReceived, RailReference: "in-2", Amount: usd(200), OccurredAt: t0.Add(2 * time.Hour)})
	sb.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReceived, RailReference: "in-1", Amount: usd(100), OccurredAt: t0.Add(1 * time.Hour)})
	sb.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReceived, RailReference: "in-0", Amount: usd(50), OccurredAt: t0})

	events, err := sb.ReceiveInbound(context.Background(), t0)
	if err != nil {
		t.Fatalf("ReceiveInbound: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events strictly after `since`, got %d", len(events))
	}
	if events[0].RailReference != "in-1" || events[1].RailReference != "in-2" {
		t.Fatalf("expected events sorted by OccurredAt ascending, got %+v", events)
	}
}

func TestReturnPayment_IdempotentReplay(t *testing.T) {
	sb := New()
	in := railclient.ReturnPaymentInput{IdempotencyKey: "ret-1", OriginalRailReference: "in-1", Amount: usd(100), ReasonCode: "NSF"}
	first, err := sb.ReturnPayment(context.Background(), in)
	if err != nil {
		t.Fatalf("first ReturnPayment: %v", err)
	}
	second, err := sb.ReturnPayment(context.Background(), in)
	if err != nil {
		t.Fatalf("replay ReturnPayment: %v", err)
	}
	if second.RailReference != first.RailReference {
		t.Fatalf("expected the replay to return the SAME rail reference, got %s vs %s", second.RailReference, first.RailReference)
	}
}

func TestErrorInjection(t *testing.T) {
	sb := New()
	sb.SetNextInitiateErr(errors.New("boom"))
	if _, err := sb.Initiate(context.Background(), railclient.InitiateInput{InstructionID: "instr-1"}); err == nil {
		t.Fatalf("expected the armed Initiate error")
	}
	// Armed error is consumed -- the next call must succeed normally.
	if _, err := sb.Initiate(context.Background(), railclient.InitiateInput{InstructionID: "instr-1"}); err != nil {
		t.Fatalf("expected the second Initiate to succeed once the armed error is consumed, got %v", err)
	}
}
