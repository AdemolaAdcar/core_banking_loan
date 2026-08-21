package domain

import (
	"errors"
	"testing"
	"time"
)

func usd(amount int64) Money { return Money{Amount: amount, Currency: "USD"} }

func TestNewOutboundDisbursement_StartsSubmitted(t *testing.T) {
	p := NewOutboundDisbursement("instr-1", "loan-1", "party-1", "je-1", usd(1000), time.Unix(0, 0))
	if p.Status != StatusSubmitted {
		t.Fatalf("expected Submitted, got %s", p.Status)
	}
	if p.Direction != Outbound || p.Purpose != PurposeDisbursement {
		t.Fatalf("expected OUTBOUND/DISBURSEMENT, got %s/%s", p.Direction, p.Purpose)
	}
	if p.PartyID == nil || *p.PartyID != "party-1" {
		t.Fatalf("expected partyId party-1, got %+v", p.PartyID)
	}
	if p.RailReference != nil {
		t.Fatalf("expected no rail reference before Initiate is ever called, got %+v", p.RailReference)
	}
}

func TestNewInboundReceipt_StartsExecuted(t *testing.T) {
	p := NewInboundReceipt("instr-2", "ACH", "rail-ref-1", usd(500), time.Unix(0, 0))
	if p.Status != StatusExecuted {
		t.Fatalf("expected Executed, got %s", p.Status)
	}
	if p.Direction != Inbound || p.Purpose != PurposeRepayment {
		t.Fatalf("expected INBOUND/REPAYMENT, got %s/%s", p.Direction, p.Purpose)
	}
	if p.RailReference == nil || *p.RailReference != "rail-ref-1" {
		t.Fatalf("expected rail reference rail-ref-1, got %+v", p.RailReference)
	}
}

func TestValidTransitions(t *testing.T) {
	cases := []struct {
		name string
		from Status
		to   Status
	}{
		{"submitted -> executed", StatusSubmitted, StatusExecuted},
		{"submitted -> failed", StatusSubmitted, StatusFailed},
		{"submitted -> returned (no intervening confirmation)", StatusSubmitted, StatusReturned},
		{"executed -> returned (later reversal)", StatusExecuted, StatusReturned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := PaymentInstruction{Status: tc.from, UpdatedAt: time.Unix(0, 0)}
			next, err := p.TransitionTo(tc.to, nil, time.Unix(100, 0))
			if err != nil {
				t.Fatalf("expected %s -> %s to be valid, got error: %v", tc.from, tc.to, err)
			}
			if next.Status != tc.to {
				t.Fatalf("expected status %s, got %s", tc.to, next.Status)
			}
			if !next.UpdatedAt.Equal(time.Unix(100, 0)) {
				t.Fatalf("expected UpdatedAt to advance")
			}
			if p.Status != tc.from {
				t.Fatalf("TransitionTo must not mutate the receiver, got %s", p.Status)
			}
		})
	}
}

// TestInvalidTransitions exhaustively enumerates every OTHER pair among
// the four statuses, confirming each is rejected with
// ErrInvalidTransition.
func TestInvalidTransitions(t *testing.T) {
	all := []Status{StatusSubmitted, StatusExecuted, StatusFailed, StatusReturned}
	valid := map[[2]Status]bool{
		{StatusSubmitted, StatusExecuted}: true,
		{StatusSubmitted, StatusFailed}:   true,
		{StatusSubmitted, StatusReturned}: true,
		{StatusExecuted, StatusReturned}:  true,
	}
	tested := 0
	for _, from := range all {
		for _, to := range all {
			if from == to || valid[[2]Status{from, to}] {
				continue
			}
			tested++
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				p := PaymentInstruction{Status: from}
				_, err := p.TransitionTo(to, nil, time.Now())
				if err == nil {
					t.Fatalf("expected %s -> %s to be rejected", from, to)
				}
				var invalidErr *ErrInvalidTransition
				if !errors.As(err, &invalidErr) {
					t.Fatalf("expected ErrInvalidTransition, got %T: %v", err, err)
				}
				if invalidErr.From != from || invalidErr.To != to {
					t.Fatalf("ErrInvalidTransition carries wrong from/to: got %s->%s", invalidErr.From, invalidErr.To)
				}
			})
		}
	}
	// 4 statuses x 4 = 16 pairs, minus 4 self-pairs, minus 4 valid pairs = 8 invalid pairs.
	if tested != 8 {
		t.Fatalf("expected to have tested 8 invalid transition pairs, tested %d -- the exhaustive matrix changed without this test being updated", tested)
	}
}

func TestTransitionTo_RecordsFailureReasonOnlyForFailedOrReturned(t *testing.T) {
	reason := ReasonRailRejected
	p := PaymentInstruction{Status: StatusSubmitted}

	executed, err := p.TransitionTo(StatusExecuted, &reason, time.Now())
	if err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	if executed.FailureReason != nil {
		t.Fatalf("expected no failureReason on an Executed transition even if one was passed, got %v", executed.FailureReason)
	}

	failed, err := p.TransitionTo(StatusFailed, &reason, time.Now())
	if err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	if failed.FailureReason == nil || *failed.FailureReason != ReasonRailRejected {
		t.Fatalf("expected failureReason RAIL_REJECTED, got %v", failed.FailureReason)
	}
}

func TestIsTerminal(t *testing.T) {
	for _, s := range []Status{StatusFailed, StatusReturned} {
		if !(PaymentInstruction{Status: s}).IsTerminal() {
			t.Fatalf("status %s must be terminal", s)
		}
	}
	for _, s := range []Status{StatusSubmitted, StatusExecuted} {
		if (PaymentInstruction{Status: s}).IsTerminal() {
			t.Fatalf("status %s must not be terminal (Submitted can still move, Executed can still move to Returned)", s)
		}
	}
}

func TestWithRailReference(t *testing.T) {
	p := NewOutboundDisbursement("instr-1", "loan-1", "party-1", "je-1", usd(1000), time.Unix(0, 0))
	next := p.WithRailReference("rail-ref-1", time.Unix(100, 0))
	if next.RailReference == nil || *next.RailReference != "rail-ref-1" {
		t.Fatalf("expected rail reference rail-ref-1, got %+v", next.RailReference)
	}
	if p.RailReference != nil {
		t.Fatalf("WithRailReference must not mutate the receiver")
	}
}
