package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/accountclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/store"
)

func TestReceiveInboundPayments_NewReceipt_CallsAccountAPIAndPersists(t *testing.T) {
	svc, st, rail, account := newTestService()
	loanRef := "loan-1"
	t0 := time.Unix(1000, 0)
	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReceived, RailReference: "in-1", LoanAccountRef: &loanRef, Amount: usd(500), Rail: "sandbox", OccurredAt: t0})
	je := "je-repay-1"
	account.NextNotifyResult = &accountclient.ReceiveRepaymentNotificationResult{Kind: accountclient.KindRepayment, ID: "repay-1", Status: "Posted", JournalEntryID: &je}

	summary, err := svc.ReceiveInboundPayments(context.Background(), "sandbox")
	if err != nil {
		t.Fatalf("ReceiveInboundPayments: %v", err)
	}
	if summary.Received != 1 {
		t.Fatalf("expected received=1, got %+v", summary)
	}
	if len(account.NotifyCalls) != 1 || account.NotifyCalls[0].IdempotencyKey != "in-1" {
		t.Fatalf("expected exactly one ReceiveRepaymentNotification call keyed by the rail reference, got %+v", account.NotifyCalls)
	}

	instruction, err := st.GetPaymentInstruction(context.Background(), "in-1")
	if err != nil {
		t.Fatalf("GetPaymentInstruction: %v", err)
	}
	if instruction.Direction != domain.Inbound || instruction.Status != domain.StatusExecuted {
		t.Fatalf("expected an Executed INBOUND instruction, got %+v", instruction)
	}
	if instruction.JournalEntryID == nil || *instruction.JournalEntryID != je {
		t.Fatalf("expected journalEntryId %s, got %+v", je, instruction.JournalEntryID)
	}

	cursor, found, err := st.GetInboundCursor(context.Background(), "sandbox")
	if err != nil {
		t.Fatalf("GetInboundCursor: %v", err)
	}
	if !found || !cursor.Equal(t0) {
		t.Fatalf("expected cursor advanced to %v, got found=%v cursor=%v", t0, found, cursor)
	}

	var sawInboundReceived bool
	for _, e := range st.outboxEntries {
		if e.Topic == "payment.inbound.received" {
			sawInboundReceived = true
		}
	}
	if !sawInboundReceived {
		t.Fatalf("expected a payment.inbound.received outbox entry, got %+v", st.outboxEntries)
	}
}

func TestReceiveInboundPayments_PayoffKind_SetsPurposePayoff(t *testing.T) {
	svc, st, rail, account := newTestService()
	t0 := time.Unix(1000, 0)
	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReceived, RailReference: "in-1", Amount: usd(50000), Rail: "sandbox", OccurredAt: t0})
	account.NextNotifyResult = &accountclient.ReceiveRepaymentNotificationResult{Kind: accountclient.KindPayoff, ID: "payoff-1", Status: "Closed"}

	if _, err := svc.ReceiveInboundPayments(context.Background(), "sandbox"); err != nil {
		t.Fatalf("ReceiveInboundPayments: %v", err)
	}
	instruction, err := st.GetPaymentInstruction(context.Background(), "in-1")
	if err != nil {
		t.Fatalf("GetPaymentInstruction: %v", err)
	}
	if instruction.Purpose != domain.PurposePayoff {
		t.Fatalf("expected Purpose PAYOFF, got %s", instruction.Purpose)
	}
}

func TestReceiveInboundPayments_AlreadyProcessed_SkipsAccountAPIButAdvancesCursor(t *testing.T) {
	svc, st, rail, account := newTestService()
	t0 := time.Unix(1000, 0)

	// Simulate an instruction already persisted in a prior sweep (crash
	// happened after SavePaymentInstruction but before SetInboundCursor
	// committed, or simply a re-delivered event within the same window).
	existing := domain.NewInboundReceipt("in-1", "sandbox", "in-1", usd(500), t0)
	if err := st.WithinTx(context.Background(), func(tx store.Tx) error {
		return tx.SavePaymentInstruction(context.Background(), existing)
	}); err != nil {
		t.Fatalf("seeding existing instruction: %v", err)
	}

	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReceived, RailReference: "in-1", Amount: usd(500), Rail: "sandbox", OccurredAt: t0})

	if _, err := svc.ReceiveInboundPayments(context.Background(), "sandbox"); err != nil {
		t.Fatalf("ReceiveInboundPayments: %v", err)
	}
	if len(account.NotifyCalls) != 0 {
		t.Fatalf("expected NO ReceiveRepaymentNotification call for an already-processed event, got %+v", account.NotifyCalls)
	}
	cursor, found, err := st.GetInboundCursor(context.Background(), "sandbox")
	if err != nil {
		t.Fatalf("GetInboundCursor: %v", err)
	}
	if !found || !cursor.Equal(t0) {
		t.Fatalf("expected the cursor to still advance past the already-processed event, got found=%v cursor=%v", found, cursor)
	}
}

func TestReceiveInboundPayments_AccountAPIFailure_CursorNotAdvanced(t *testing.T) {
	svc, st, rail, account := newTestService()
	t0 := time.Unix(1000, 0)
	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReceived, RailReference: "in-1", Amount: usd(500), Rail: "sandbox", OccurredAt: t0})
	account.NextNotifyErr = accountclient.ErrAccountUnavailable

	_, err := svc.ReceiveInboundPayments(context.Background(), "sandbox")
	if !errors.Is(err, accountclient.ErrAccountUnavailable) {
		t.Fatalf("expected ErrAccountUnavailable to propagate, got %v", err)
	}
	if _, found, _ := st.GetInboundCursor(context.Background(), "sandbox"); found {
		t.Fatalf("expected the cursor NOT to advance past a failed event")
	}
	if _, err := st.GetPaymentInstruction(context.Background(), "in-1"); err == nil {
		t.Fatalf("expected NO PaymentInstruction to be persisted for a failed AccountAPI call")
	}
}

func TestReceiveInboundPayments_Return_TriggersCompensatingReversal(t *testing.T) {
	svc, st, rail, account := newTestService()
	t0 := time.Unix(1000, 0)
	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReceived, RailReference: "in-1", Amount: usd(500), Rail: "sandbox", OccurredAt: t0})
	if _, err := svc.ReceiveInboundPayments(context.Background(), "sandbox"); err != nil {
		t.Fatalf("first sweep: %v", err)
	}

	t1 := t0.Add(time.Hour)
	reason := domain.ReasonPaymentReturned
	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReturned, RailReference: "in-1-ret", OriginalRailReference: "in-1", Amount: usd(500), Rail: "sandbox", FailureReason: &reason, OccurredAt: t1})

	summary, err := svc.ReceiveInboundPayments(context.Background(), "sandbox")
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if summary.Returned != 1 || summary.ReturnedUnmatched != 0 || summary.ReturnedNoCompliantPath != 0 {
		t.Fatalf("expected a clean matched return, got %+v", summary)
	}
	if len(account.ReverseCalls) != 1 || account.ReverseCalls[0].RepaymentID != "in-1" {
		t.Fatalf("expected exactly one ReverseRepayment call for repaymentId in-1, got %+v", account.ReverseCalls)
	}

	reloaded, err := st.GetPaymentInstruction(context.Background(), "in-1")
	if err != nil {
		t.Fatalf("GetPaymentInstruction: %v", err)
	}
	if reloaded.Status != domain.StatusReturned {
		t.Fatalf("expected Returned, got %s", reloaded.Status)
	}
}

func TestReceiveInboundPayments_ReturnOfUnknownOriginal_FiledAsException(t *testing.T) {
	svc, st, rail, account := newTestService()
	t0 := time.Unix(1000, 0)
	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReturned, RailReference: "ret-1", OriginalRailReference: "never-received", Amount: usd(500), Rail: "sandbox", OccurredAt: t0})

	summary, err := svc.ReceiveInboundPayments(context.Background(), "sandbox")
	if err != nil {
		t.Fatalf("ReceiveInboundPayments: %v", err)
	}
	if summary.ReturnedUnmatched != 1 {
		t.Fatalf("expected returnedUnmatched=1, got %+v", summary)
	}
	if len(account.ReverseCalls) != 0 {
		t.Fatalf("expected NO ReverseRepayment call for an unmatched return, got %+v", account.ReverseCalls)
	}
	if len(st.exceptions) != 1 || st.exceptions[0].Kind != domain.ExceptionUnmatchedInboundReturn {
		t.Fatalf("expected exactly one UNMATCHED_INBOUND_RETURN exception, got %+v", st.exceptions)
	}
}

// TestReceiveInboundPayments_ReturnOfPayoff_RefusedWithException is this
// role's OWN "refuse and explain the compliant alternative" clause: a
// returned Payoff payment has no compliant AccountAPI reversal endpoint
// to call (see ErrNoCompliantReversalPath), so this service refuses to
// call ReverseRepayment against it -- but still records the return
// locally and files an exception, never a silent write-off.
func TestReceiveInboundPayments_ReturnOfPayoff_RefusedWithException(t *testing.T) {
	svc, st, rail, account := newTestService()
	t0 := time.Unix(1000, 0)
	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReceived, RailReference: "in-1", Amount: usd(50000), Rail: "sandbox", OccurredAt: t0})
	account.NextNotifyResult = &accountclient.ReceiveRepaymentNotificationResult{Kind: accountclient.KindPayoff, ID: "payoff-1", Status: "Closed"}
	if _, err := svc.ReceiveInboundPayments(context.Background(), "sandbox"); err != nil {
		t.Fatalf("first sweep: %v", err)
	}

	t1 := t0.Add(time.Hour)
	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReturned, RailReference: "in-1-ret", OriginalRailReference: "in-1", Amount: usd(50000), Rail: "sandbox", OccurredAt: t1})

	summary, err := svc.ReceiveInboundPayments(context.Background(), "sandbox")
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if summary.ReturnedNoCompliantPath != 1 {
		t.Fatalf("expected returnedNoCompliantPath=1, got %+v", summary)
	}
	if len(account.ReverseCalls) != 0 {
		t.Fatalf("expected NO ReverseRepayment call against a Payoff -- no compliant endpoint exists, got %+v", account.ReverseCalls)
	}

	var sawException bool
	for _, e := range st.exceptions {
		if e.Kind == domain.ExceptionNoCompliantReversalPath {
			sawException = true
		}
	}
	if !sawException {
		t.Fatalf("expected a NO_COMPLIANT_REVERSAL_PATH exception, got %+v", st.exceptions)
	}

	reloaded, err := st.GetPaymentInstruction(context.Background(), "in-1")
	if err != nil {
		t.Fatalf("GetPaymentInstruction: %v", err)
	}
	if reloaded.Status != domain.StatusReturned {
		t.Fatalf("expected the local record to still accurately reflect Returned (not silently left Executed), got %s", reloaded.Status)
	}
}
