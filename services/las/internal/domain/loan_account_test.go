package domain

import (
	"errors"
	"testing"
	"time"
)

func testAccount(status Status) LoanAccount {
	return LoanAccount{LoanAccountID: "loan-1", Status: status, CreatedAt: time.Unix(0, 0), UpdatedAt: time.Unix(0, 0)}
}

// TestValidTransitions exercises every transition the ground rules and
// the approved spec together define as legal.
func TestValidTransitions(t *testing.T) {
	cases := []struct {
		name string
		from Status
		to   Status
	}{
		{"book -> disbursement requested", StatusApproved, StatusPendingDisbursement},
		{"funding confirmed", StatusPendingDisbursement, StatusDisbursed},
		{"disbursement reversed", StatusDisbursed, StatusApproved},
		{"payoff settlement", StatusDisbursed, StatusClosed},
		{"charge-off", StatusDisbursed, StatusChargedOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := testAccount(tc.from)
			next, err := a.TransitionTo(tc.to, time.Unix(100, 0))
			if err != nil {
				t.Fatalf("expected %s -> %s to be valid, got error: %v", tc.from, tc.to, err)
			}
			if next.Status != tc.to {
				t.Fatalf("expected status %s, got %s", tc.to, next.Status)
			}
			if !next.UpdatedAt.Equal(time.Unix(100, 0)) {
				t.Fatalf("expected UpdatedAt to advance")
			}
			// receiver must not be mutated
			if a.Status != tc.from {
				t.Fatalf("TransitionTo must not mutate the receiver, got %s", a.Status)
			}
		})
	}
}

// TestInvalidTransitions exercises every OTHER pair among the five
// statuses, confirming each is rejected with ErrInvalidTransition — the
// ground rule's "any other transition is invalid and must be rejected
// with a 409, not silently allowed."
func TestInvalidTransitions(t *testing.T) {
	all := []Status{StatusApproved, StatusPendingDisbursement, StatusDisbursed, StatusClosed, StatusChargedOff}
	valid := map[[2]Status]bool{
		{StatusApproved, StatusPendingDisbursement}:  true,
		{StatusPendingDisbursement, StatusDisbursed}: true,
		{StatusDisbursed, StatusApproved}:            true,
		{StatusDisbursed, StatusClosed}:              true,
		{StatusDisbursed, StatusChargedOff}:          true,
	}
	tested := 0
	for _, from := range all {
		for _, to := range all {
			if from == to {
				continue
			}
			if valid[[2]Status{from, to}] {
				continue
			}
			tested++
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				a := testAccount(from)
				_, err := a.TransitionTo(to, time.Now())
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
	// 5 statuses x 5 = 25 pairs, minus 5 self-pairs, minus 5 valid pairs = 15 invalid pairs.
	if tested != 15 {
		t.Fatalf("expected to have tested 15 invalid transition pairs, tested %d -- the exhaustive matrix changed without this test being updated", tested)
	}
}

// TestNoCureFromChargedOff is a targeted regression test for the
// specific conflict resolved with the requester before implementation:
// the ground rules' literal "Delinquent -> Active" cure has NO analogue
// in the approved spec's ChargedOff status -- there is no transition
// out of ChargedOff at all.
func TestNoCureFromChargedOff(t *testing.T) {
	for _, to := range []Status{StatusApproved, StatusPendingDisbursement, StatusDisbursed, StatusClosed} {
		if CanTransition(StatusChargedOff, to) {
			t.Fatalf("ChargedOff must be terminal -- found an unexpected allowed transition to %s", to)
		}
	}
}

func TestClosedIsTerminal(t *testing.T) {
	for _, to := range []Status{StatusApproved, StatusPendingDisbursement, StatusDisbursed, StatusChargedOff} {
		if CanTransition(StatusClosed, to) {
			t.Fatalf("Closed must be terminal -- found an unexpected allowed transition to %s", to)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	for _, s := range []Status{StatusApproved, StatusPendingDisbursement, StatusDisbursed} {
		if testAccount(s).IsTerminal() {
			t.Fatalf("status %s must not be terminal", s)
		}
	}
	for _, s := range []Status{StatusClosed, StatusChargedOff} {
		if !testAccount(s).IsTerminal() {
			t.Fatalf("status %s must be terminal", s)
		}
	}
}

func TestNewLoanAccountStartsApproved(t *testing.T) {
	terms := TermSet{PrincipalAmount: Money{Amount: 100000, Currency: "USD"}, AnnualInterestRateBps: 1000, TermMonths: 12, DayCountConvention: "ACTUAL_365"}
	a := NewLoanAccount("loan-1", "approval-1", "party-1", "officer-1", terms, time.Unix(0, 0))
	if a.Status != StatusApproved {
		t.Fatalf("expected new loan account to start Approved, got %s", a.Status)
	}
	if a.CurrentTermVersion.Version != 1 {
		t.Fatalf("expected initial term version 1, got %d", a.CurrentTermVersion.Version)
	}
	if a.DPDBucket != DPDCurrent || a.DPD != 0 || a.NonAccrualFlag {
		t.Fatalf("expected a freshly booked account to have no delinquency overlay")
	}
}
