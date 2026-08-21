package service

import (
	"context"
	"testing"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/glclient"
)

func newTestService() (*Service, *fakeStore, *glclient.Fake) {
	st := newFakeStore()
	gl := glclient.NewFake()
	return New(st, gl), st, gl
}

func usd(amount int64) domain.Money { return domain.Money{Amount: amount, Currency: "USD"} }

func mustBook(t *testing.T, svc *Service, approvalRef string) domain.LoanAccount {
	t.Helper()
	terms := domain.TermSet{PrincipalAmount: usd(100000), AnnualInterestRateBps: 1200, TermMonths: 24, DayCountConvention: "ACTUAL_365"}
	a, err := svc.BookLoanAccount(context.Background(), BookLoanAccountInput{
		ApprovalReferenceID: approvalRef, PartyID: "party-1", BookedBy: "officer-1", Terms: terms,
	})
	if err != nil {
		t.Fatalf("BookLoanAccount: %v", err)
	}
	return a
}

// mustDisburse books nothing new -- it moves an already-Approved account
// through createDisbursement + confirmed funding, using a deterministic
// "disb:"+loanAccountID disbursement ID callers can reconstruct.
func mustDisburse(t *testing.T, svc *Service, loanAccountID string) domain.LoanAccount {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.CreateDisbursement(ctx, loanAccountID, "disb:"+loanAccountID, "officer-1"); err != nil {
		t.Fatalf("CreateDisbursement: %v", err)
	}
	if _, err := svc.ConfirmDisbursementFunding(ctx, "disb:"+loanAccountID, "instr:"+loanAccountID); err != nil {
		t.Fatalf("ConfirmDisbursementFunding: %v", err)
	}
	a, err := svc.GetLoanAccount(ctx, loanAccountID)
	if err != nil {
		t.Fatalf("GetLoanAccount: %v", err)
	}
	return a
}

// seedOutstanding pre-loads the fake GL client's statement-of-account for
// loanAccountID with one debit line per non-zero receivable category, so
// the NEXT call this test makes that triggers RefreshBalanceProjection
// (GetBalanceProjection/RunDailyAccrual/RecordChargeOff/etc.) sees exactly
// this outstanding balance -- RebuildFromLines always re-fetches live, so
// this only needs to be seeded immediately before the action under test,
// regardless of when the account was actually disbursed.
func seedOutstanding(gl *glclient.Fake, loanAccountID string, principal, interest, fee int64, currency string) {
	var lines []domain.StatementLine
	if principal != 0 {
		lines = append(lines, domain.StatementLine{GLAccount: domain.GLAccountLoanReceivable, Direction: domain.Debit, Amount: domain.Money{Amount: principal, Currency: currency}, PostedAt: time.Unix(0, 0)})
	}
	if interest != 0 {
		lines = append(lines, domain.StatementLine{GLAccount: domain.GLAccountInterestReceivable, Direction: domain.Debit, Amount: domain.Money{Amount: interest, Currency: currency}, PostedAt: time.Unix(0, 0)})
	}
	if fee != 0 {
		lines = append(lines, domain.StatementLine{GLAccount: domain.GLAccountFeeReceivable, Direction: domain.Debit, Amount: domain.Money{Amount: fee, Currency: currency}, PostedAt: time.Unix(0, 0)})
	}
	gl.Statements[loanAccountID] = lines
}
