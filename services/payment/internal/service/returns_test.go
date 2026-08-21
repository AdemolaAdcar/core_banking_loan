package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/accountclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
)

func TestReturnInboundPayment_Succeeds_ReversesLedgerThenReturnsViaRail(t *testing.T) {
	svc, st, rail, account := newTestService()
	t0 := time.Unix(1000, 0)
	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReceived, RailReference: "in-1", Amount: usd(500), Rail: "sandbox", OccurredAt: t0})
	if _, err := svc.ReceiveInboundPayments(context.Background(), "sandbox"); err != nil {
		t.Fatalf("seeding receipt: %v", err)
	}

	sub, err := svc.ReturnInboundPayment(context.Background(), ReturnInboundPaymentInput{
		InstructionID: "in-1", IdempotencyKey: "ops-return-1", ReasonCode: "UNAUTHORIZED",
	})
	if err != nil {
		t.Fatalf("ReturnInboundPayment: %v", err)
	}
	if sub.RailReference == "" {
		t.Fatalf("expected a rail reference for the origination submission")
	}
	if len(account.ReverseCalls) != 1 || account.ReverseCalls[0].RepaymentID != "in-1" {
		t.Fatalf("expected exactly one ReverseRepayment call for in-1, got %+v", account.ReverseCalls)
	}

	reloaded, err := st.GetPaymentInstruction(context.Background(), "in-1")
	if err != nil {
		t.Fatalf("GetPaymentInstruction: %v", err)
	}
	if reloaded.Status != domain.StatusReturned {
		t.Fatalf("expected Returned, got %s", reloaded.Status)
	}
}

func TestReturnInboundPayment_Payoff_Refused(t *testing.T) {
	svc, _, rail, account := newTestService()
	t0 := time.Unix(1000, 0)
	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReceived, RailReference: "in-1", Amount: usd(50000), Rail: "sandbox", OccurredAt: t0})
	account.NextNotifyResult = &accountclient.ReceiveRepaymentNotificationResult{Kind: accountclient.KindPayoff, ID: "payoff-1", Status: "Closed"}
	if _, err := svc.ReceiveInboundPayments(context.Background(), "sandbox"); err != nil {
		t.Fatalf("seeding receipt: %v", err)
	}

	_, err := svc.ReturnInboundPayment(context.Background(), ReturnInboundPaymentInput{InstructionID: "in-1", IdempotencyKey: "ops-return-1", ReasonCode: "UNAUTHORIZED"})
	if !errors.Is(err, ErrNoCompliantReversalPath) {
		t.Fatalf("expected ErrNoCompliantReversalPath, got %v", err)
	}
	if len(account.ReverseCalls) != 0 {
		t.Fatalf("expected NO ReverseRepayment call against a Payoff, got %+v", account.ReverseCalls)
	}
}

func TestReturnInboundPayment_UnknownInstruction_NotFound(t *testing.T) {
	svc, _, _, _ := newTestService()
	_, err := svc.ReturnInboundPayment(context.Background(), ReturnInboundPaymentInput{InstructionID: "no-such-instruction", IdempotencyKey: "ops-return-1", ReasonCode: "UNAUTHORIZED"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestReturnInboundPayment_RailFailure_LedgerAlreadyCorrected(t *testing.T) {
	svc, st, rail, account := newTestService()
	t0 := time.Unix(1000, 0)
	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReceived, RailReference: "in-1", Amount: usd(500), Rail: "sandbox", OccurredAt: t0})
	if _, err := svc.ReceiveInboundPayments(context.Background(), "sandbox"); err != nil {
		t.Fatalf("seeding receipt: %v", err)
	}
	rail.SetNextReturnErr(railclient.ErrRailUnavailable)

	_, err := svc.ReturnInboundPayment(context.Background(), ReturnInboundPaymentInput{InstructionID: "in-1", IdempotencyKey: "ops-return-1", ReasonCode: "UNAUTHORIZED"})
	if !errors.Is(err, railclient.ErrRailUnavailable) {
		t.Fatalf("expected ErrRailUnavailable to propagate, got %v", err)
	}
	// The GL-side reversal call already happened and is durably correct
	// regardless of the rail submission failing -- see this method's own
	// doc comment on ordering.
	if len(account.ReverseCalls) != 1 {
		t.Fatalf("expected the ReverseRepayment call to have already happened, got %+v", account.ReverseCalls)
	}
	// The PaymentInstruction itself is NOT saved as Returned yet, since
	// the rail submission (the other half of this operation) failed --
	// a retry with the same IdempotencyKey is expected to complete it.
	reloaded, err := st.GetPaymentInstruction(context.Background(), "in-1")
	if err != nil {
		t.Fatalf("GetPaymentInstruction: %v", err)
	}
	if reloaded.Status != domain.StatusExecuted {
		t.Fatalf("expected the instruction to remain Executed pending a successful retry, got %s", reloaded.Status)
	}
}
