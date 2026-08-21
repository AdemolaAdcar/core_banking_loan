package service

import (
	"context"
	"errors"
	"testing"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
)

func TestBookLoanAccount_Succeeds(t *testing.T) {
	svc, st, gl := newTestService()
	a := mustBook(t, svc, "approval-1")

	if a.Status != domain.StatusApproved {
		t.Fatalf("expected Approved, got %s", a.Status)
	}
	if a.CurrentTermVersion.Version != 1 {
		t.Fatalf("expected term version 1, got %d", a.CurrentTermVersion.Version)
	}
	if len(gl.Calls) != 0 {
		t.Fatalf("expected zero GLPostingAPI calls from booking, got %d", len(gl.Calls))
	}
	if len(st.outboxEntries) != 1 || st.outboxEntries[0].Topic != "loan.account.booked" {
		t.Fatalf("expected exactly one loan.account.booked outbox entry, got %+v", st.outboxEntries)
	}
}

func TestBookLoanAccount_IdempotentReplay(t *testing.T) {
	svc, st, _ := newTestService()
	first := mustBook(t, svc, "approval-1")

	second, err := svc.BookLoanAccount(context.Background(), BookLoanAccountInput{
		ApprovalReferenceID: "approval-1", PartyID: "party-1", BookedBy: "officer-1",
		Terms: domain.TermSet{PrincipalAmount: usd(999), AnnualInterestRateBps: 1, TermMonths: 1, DayCountConvention: "ACTUAL_365"},
	})
	if err != nil {
		t.Fatalf("expected idempotent replay to succeed, got %v", err)
	}
	if second.LoanAccountID != first.LoanAccountID {
		t.Fatalf("expected the same account to be returned on replay")
	}
	if len(st.accounts) != 1 {
		t.Fatalf("expected exactly one account to have been created, got %d", len(st.accounts))
	}
}

func TestBookLoanAccount_InvalidTerms_Rejected(t *testing.T) {
	cases := []struct {
		name  string
		terms domain.TermSet
	}{
		{"zero principal", domain.TermSet{PrincipalAmount: usd(0), AnnualInterestRateBps: 1000, TermMonths: 12, DayCountConvention: "ACTUAL_365"}},
		{"zero rate", domain.TermSet{PrincipalAmount: usd(1000), AnnualInterestRateBps: 0, TermMonths: 12, DayCountConvention: "ACTUAL_365"}},
		{"zero term", domain.TermSet{PrincipalAmount: usd(1000), AnnualInterestRateBps: 1000, TermMonths: 0, DayCountConvention: "ACTUAL_365"}},
		{"unrecognized day count", domain.TermSet{PrincipalAmount: usd(1000), AnnualInterestRateBps: 1000, TermMonths: 12, DayCountConvention: "BOGUS"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _ := newTestService()
			_, err := svc.BookLoanAccount(context.Background(), BookLoanAccountInput{
				ApprovalReferenceID: "approval-1", PartyID: "party-1", BookedBy: "officer-1", Terms: tc.terms,
			})
			if !errors.Is(err, ErrMalformedTerms) {
				t.Fatalf("expected ErrMalformedTerms, got %v", err)
			}
		})
	}
}

func TestBookLoanAccount_MissingRequiredFields_Rejected(t *testing.T) {
	validTerms := domain.TermSet{PrincipalAmount: usd(1000), AnnualInterestRateBps: 1000, TermMonths: 12, DayCountConvention: "ACTUAL_365"}
	cases := []BookLoanAccountInput{
		{ApprovalReferenceID: "", PartyID: "party-1", BookedBy: "officer-1", Terms: validTerms},
		{ApprovalReferenceID: "approval-1", PartyID: "", BookedBy: "officer-1", Terms: validTerms},
		{ApprovalReferenceID: "approval-1", PartyID: "party-1", BookedBy: "", Terms: validTerms},
	}
	for _, in := range cases {
		svc, _, _ := newTestService()
		if _, err := svc.BookLoanAccount(context.Background(), in); !errors.Is(err, ErrMalformedTerms) {
			t.Fatalf("expected ErrMalformedTerms for input %+v, got %v", in, err)
		}
	}
}
