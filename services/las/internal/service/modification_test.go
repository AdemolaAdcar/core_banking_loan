package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/postingrules"
)

func TestApplyModification_BranchA_CapitalizesPrincipalAndPosts(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	disbursed := mustDisburse(t, svc, account.LoanAccountID)

	m, err := svc.ApplyModification(context.Background(), ApplyModificationInput{
		LoanAccountID: account.LoanAccountID, ModificationID: "mod-1", EffectiveDate: time.Now(), ConfirmedBy: "ops-1",
		Capitalization: &domain.Capitalization{InterestAmount: usd(200), FeeAmount: usd(50)},
	})
	if err != nil {
		t.Fatalf("ApplyModification: %v", err)
	}
	if m.CapitalizedAmount == nil || m.CapitalizedAmount.Amount != 250 {
		t.Fatalf("expected capitalized amount 250, got %+v", m.CapitalizedAmount)
	}
	if m.JournalEntryID == nil {
		t.Fatalf("expected a journalEntryId for a Branch A modification")
	}
	if gl.CallCountForRule(string(postingrules.PRMOD01)) != 1 {
		t.Fatalf("expected exactly one PR-MOD-01 call")
	}

	reloaded, err := svc.GetLoanAccount(context.Background(), account.LoanAccountID)
	if err != nil {
		t.Fatalf("GetLoanAccount: %v", err)
	}
	expectedPrincipal := disbursed.CurrentTermVersion.PrincipalAmount.Amount + 250
	if reloaded.CurrentTermVersion.PrincipalAmount.Amount != expectedPrincipal {
		t.Fatalf("expected new term version principal %d, got %d", expectedPrincipal, reloaded.CurrentTermVersion.PrincipalAmount.Amount)
	}
	if reloaded.CurrentTermVersion.Version != 2 {
		t.Fatalf("expected term version 2, got %d", reloaded.CurrentTermVersion.Version)
	}
}

func TestApplyModification_BranchB_NoGLCall(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	callsBefore := len(gl.Calls)

	newRate := 900
	m, err := svc.ApplyModification(context.Background(), ApplyModificationInput{
		LoanAccountID: account.LoanAccountID, ModificationID: "mod-1", EffectiveDate: time.Now(), ConfirmedBy: "ops-1",
		NewAnnualInterestRateBps: &newRate,
	})
	if err != nil {
		t.Fatalf("ApplyModification: %v", err)
	}
	if m.CapitalizedAmount != nil || m.JournalEntryID != nil {
		t.Fatalf("expected a true no-op for a rate-only Branch B modification, got %+v", m)
	}
	if len(gl.Calls) != callsBefore {
		t.Fatalf("expected ZERO additional GLPostingAPI calls for a Branch B modification, got %d new calls", len(gl.Calls)-callsBefore)
	}

	reloaded, err := svc.GetLoanAccount(context.Background(), account.LoanAccountID)
	if err != nil {
		t.Fatalf("GetLoanAccount: %v", err)
	}
	if reloaded.CurrentTermVersion.AnnualInterestRateBps != newRate {
		t.Fatalf("expected new rate %d, got %d", newRate, reloaded.CurrentTermVersion.AnnualInterestRateBps)
	}
}

func TestApplyModification_NonDisbursedAccount_Rejected(t *testing.T) {
	svc, _, _ := newTestService()
	account := mustBook(t, svc, "approval-1") // still Approved

	newRate := 900
	_, err := svc.ApplyModification(context.Background(), ApplyModificationInput{
		LoanAccountID: account.LoanAccountID, ModificationID: "mod-1", EffectiveDate: time.Now(), ConfirmedBy: "ops-1",
		NewAnnualInterestRateBps: &newRate,
	})
	if !errors.Is(err, ErrNotModifiable) {
		t.Fatalf("expected ErrNotModifiable for a non-Disbursed account, got %v", err)
	}
}

func TestApplyModification_NoChangesSpecified_Rejected(t *testing.T) {
	svc, _, _ := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)

	_, err := svc.ApplyModification(context.Background(), ApplyModificationInput{
		LoanAccountID: account.LoanAccountID, ModificationID: "mod-1", EffectiveDate: time.Now(), ConfirmedBy: "ops-1",
	})
	if !errors.Is(err, ErrMalformedTerms) {
		t.Fatalf("expected ErrMalformedTerms when no change is specified, got %v", err)
	}
}
