package ach

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
)

func usd(amount int64) domain.Money { return domain.Money{Amount: amount, Currency: "USD"} }

func testConfig() Config {
	return Config{
		OriginRoutingNumber: "021000021", OriginName: "CORE BANKING CO", CompanyID: "1234567890",
		CompanyName: "CORE BANKING", DestinationRoutingNumber: "011000015", DestinationName: "ACH OPERATOR",
	}
}

func testPayouts() InMemoryPayoutDirectory {
	return InMemoryPayoutDirectory{
		"party-1": {RoutingNumber: "111000025", AccountNumber: "1234567890", AccountName: "JOHN BORROWER", AccountType: Checking},
	}
}

func TestInitiate_UnknownParty_RailRejected(t *testing.T) {
	a := New(testConfig(), testPayouts())
	_, err := a.Initiate(context.Background(), railclient.InitiateInput{InstructionID: "instr-1", PartyID: "no-such-party", Amount: usd(1000)})
	if !errors.Is(err, railclient.ErrRailRejected) {
		t.Fatalf("expected ErrRailRejected, got %v", err)
	}
}

func TestInitiate_ThenConfirm_StaysPendingUntilCutAndSettled(t *testing.T) {
	a := New(testConfig(), testPayouts())
	sub, err := a.Initiate(context.Background(), railclient.InitiateInput{InstructionID: "instr-1", PartyID: "party-1", Amount: usd(150000)})
	if err != nil {
		t.Fatalf("Initiate: %v", err)
	}

	outcome, err := a.Confirm(context.Background(), sub.RailReference)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if outcome.Status != railclient.OutcomePending {
		t.Fatalf("expected Pending before any batch is even cut, got %s", outcome.Status)
	}

	batchID, file, err := a.CutBatch(context.Background(), time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("CutBatch: %v", err)
	}
	if batchID == "" || len(file) == 0 {
		t.Fatalf("expected a non-empty batch ID and NACHA file")
	}

	// Still Pending -- cutting the batch only transmits it, it does not
	// settle it. This is the whole point of this adapter: no real-time
	// confirmation exists on ACH.
	outcome, err = a.Confirm(context.Background(), sub.RailReference)
	if err != nil {
		t.Fatalf("Confirm after cut: %v", err)
	}
	if outcome.Status != railclient.OutcomePending {
		t.Fatalf("expected Pending after a cut but before settlement, got %s", outcome.Status)
	}

	applied, unmatched, err := a.ApplySettlementFile(context.Background(), []SettlementResult{{TraceNumber: sub.RailReference, Outcome: railclient.OutcomeExecuted}})
	if err != nil {
		t.Fatalf("ApplySettlementFile: %v", err)
	}
	if applied != 1 || len(unmatched) != 0 {
		t.Fatalf("expected 1 applied, 0 unmatched, got applied=%d unmatched=%v", applied, unmatched)
	}

	outcome, err = a.Confirm(context.Background(), sub.RailReference)
	if err != nil {
		t.Fatalf("Confirm after settlement: %v", err)
	}
	if outcome.Status != railclient.OutcomeExecuted {
		t.Fatalf("expected Executed after settlement, got %s", outcome.Status)
	}
}

func TestApplySettlementFile_ReturnedEntry_CarriesFailureReason(t *testing.T) {
	a := New(testConfig(), testPayouts())
	sub, err := a.Initiate(context.Background(), railclient.InitiateInput{InstructionID: "instr-1", PartyID: "party-1", Amount: usd(1000)})
	if err != nil {
		t.Fatalf("Initiate: %v", err)
	}
	if _, _, err := a.CutBatch(context.Background(), time.Now()); err != nil {
		t.Fatalf("CutBatch: %v", err)
	}
	if _, _, err := a.ApplySettlementFile(context.Background(), []SettlementResult{{TraceNumber: sub.RailReference, Outcome: railclient.OutcomeReturned, ReasonCode: "R01"}}); err != nil {
		t.Fatalf("ApplySettlementFile: %v", err)
	}
	outcome, err := a.Confirm(context.Background(), sub.RailReference)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if outcome.Status != railclient.OutcomeReturned || outcome.FailureReason == nil || *outcome.FailureReason != domain.ReasonPaymentReturned {
		t.Fatalf("expected Returned/PAYMENT_RETURNED, got %+v", outcome)
	}
}

func TestApplySettlementFile_UnknownTraceNumber_ReportedUnmatched(t *testing.T) {
	a := New(testConfig(), testPayouts())
	applied, unmatched, err := a.ApplySettlementFile(context.Background(), []SettlementResult{{TraceNumber: "no-such-trace", Outcome: railclient.OutcomeExecuted}})
	if err != nil {
		t.Fatalf("ApplySettlementFile: %v", err)
	}
	if applied != 0 || len(unmatched) != 1 || unmatched[0] != "no-such-trace" {
		t.Fatalf("expected 0 applied, 1 unmatched, got applied=%d unmatched=%v", applied, unmatched)
	}
}

func TestCutBatch_NothingToCut_Errors(t *testing.T) {
	a := New(testConfig(), testPayouts())
	if _, _, err := a.CutBatch(context.Background(), time.Now()); err == nil {
		t.Fatalf("expected an error when cutting an empty batch")
	}
}

func TestCutBatch_ProducesWellFormedNACHAFile(t *testing.T) {
	a := New(testConfig(), testPayouts())
	if _, err := a.Initiate(context.Background(), railclient.InitiateInput{InstructionID: "instr-1", PartyID: "party-1", Amount: usd(150000)}); err != nil {
		t.Fatalf("Initiate: %v", err)
	}
	_, file, err := a.CutBatch(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("CutBatch: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(file), "\n"), "\n")
	if len(lines)%10 != 0 {
		t.Fatalf("expected the file to be block-padded to a multiple of 10 records, got %d records", len(lines))
	}
	for i, line := range lines {
		if len(line) != 94 {
			t.Fatalf("record %d is %d characters, expected exactly 94", i, len(line))
		}
	}
	if lines[0][0] != '1' {
		t.Fatalf("expected the first record to be a Type 1 File Header, got type %q", string(lines[0][0]))
	}
	if lines[1][0] != '5' {
		t.Fatalf("expected the second record to be a Type 5 Batch Header, got type %q", string(lines[1][0]))
	}
	if lines[2][0] != '6' {
		t.Fatalf("expected the third record to be a Type 6 Entry Detail, got type %q", string(lines[2][0]))
	}
}

func TestIngestIncomingBatch_ThenReceiveInbound(t *testing.T) {
	a := New(testConfig(), testPayouts())
	loanRef := "loan-1"
	now := time.Now().UTC()
	n := a.IngestIncomingBatch(context.Background(), []IncomingCredit{
		{RailReference: "in-1", LoanAccountRef: &loanRef, Amount: usd(500), ReceivedAt: now},
	})
	if n != 1 {
		t.Fatalf("expected 1 ingested, got %d", n)
	}
	events, err := a.ReceiveInbound(context.Background(), now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("ReceiveInbound: %v", err)
	}
	if len(events) != 1 || events[0].Kind != railclient.InboundReceived || events[0].RailReference != "in-1" {
		t.Fatalf("expected 1 Received event for in-1, got %+v", events)
	}
}

func TestIngestReturnReport_MatchedAndUnmatched(t *testing.T) {
	a := New(testConfig(), testPayouts())
	now := time.Now().UTC()
	a.IngestIncomingBatch(context.Background(), []IncomingCredit{{RailReference: "in-1", Amount: usd(500), ReceivedAt: now}})

	applied, unmatched := a.IngestReturnReport(context.Background(), []IncomingReturnNotice{
		{OriginalRailReference: "in-1", ReasonCode: "R01", OccurredAt: now.Add(time.Hour)},
		{OriginalRailReference: "no-such-credit", ReasonCode: "R01", OccurredAt: now.Add(time.Hour)},
	})
	if applied != 1 {
		t.Fatalf("expected 1 applied, got %d", applied)
	}
	if len(unmatched) != 1 || unmatched[0] != "no-such-credit" {
		t.Fatalf("expected 1 unmatched (no-such-credit), got %v", unmatched)
	}

	events, err := a.ReceiveInbound(context.Background(), now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("ReceiveInbound: %v", err)
	}
	var sawReturn bool
	for _, e := range events {
		if e.Kind == railclient.InboundReturned && e.OriginalRailReference == "in-1" {
			sawReturn = true
		}
	}
	if !sawReturn {
		t.Fatalf("expected a Returned event referencing in-1, got %+v", events)
	}
}

// TestReturnPayment_OutsideReturnWindow_Rejected is the concrete
// demonstration of this rail's own return-window limitation: a real ACH
// return can only be originated by the receiving bank within a short
// window after settlement (see ErrReturnWindowExpired's doc comment).
func TestReturnPayment_OutsideReturnWindow_Rejected(t *testing.T) {
	cfg := testConfig()
	cfg.ReturnWindow = time.Hour
	a := New(cfg, testPayouts())
	longAgo := time.Now().Add(-2 * time.Hour)
	a.IngestIncomingBatch(context.Background(), []IncomingCredit{{RailReference: "in-1", Amount: usd(500), ReceivedAt: longAgo}})

	_, err := a.ReturnPayment(context.Background(), railclient.ReturnPaymentInput{IdempotencyKey: "ret-1", OriginalRailReference: "in-1", Amount: usd(500), ReasonCode: "UNAUTHORIZED"})
	if !errors.Is(err, ErrReturnWindowExpired) {
		t.Fatalf("expected ErrReturnWindowExpired, got %v", err)
	}
}

func TestReturnPayment_WithinWindow_SucceedsAndIsIdempotent(t *testing.T) {
	a := New(testConfig(), testPayouts())
	now := time.Now().UTC()
	a.IngestIncomingBatch(context.Background(), []IncomingCredit{{RailReference: "in-1", Amount: usd(500), ReceivedAt: now}})

	in := railclient.ReturnPaymentInput{IdempotencyKey: "ret-1", OriginalRailReference: "in-1", Amount: usd(500), ReasonCode: "UNAUTHORIZED"}
	first, err := a.ReturnPayment(context.Background(), in)
	if err != nil {
		t.Fatalf("ReturnPayment: %v", err)
	}
	second, err := a.ReturnPayment(context.Background(), in)
	if err != nil {
		t.Fatalf("replay ReturnPayment: %v", err)
	}
	if second.RailReference != first.RailReference {
		t.Fatalf("expected the replay to return the same rail reference, got %s vs %s", second.RailReference, first.RailReference)
	}
}

func TestReturnPayment_UnknownOriginal_NotFound(t *testing.T) {
	a := New(testConfig(), testPayouts())
	_, err := a.ReturnPayment(context.Background(), railclient.ReturnPaymentInput{IdempotencyKey: "ret-1", OriginalRailReference: "no-such-credit", Amount: usd(500), ReasonCode: "UNAUTHORIZED"})
	if !errors.Is(err, railclient.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
