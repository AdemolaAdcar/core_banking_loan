package domain

import (
	"testing"
	"time"
)

func line(account string, dir Direction, amount int64) StatementLine {
	return StatementLine{GLAccount: account, Direction: dir, Amount: Money{Amount: amount, Currency: "USD"}, PostedAt: time.Unix(0, 0)}
}

// TestRebuildFromLines_Sequence proves balance-projection correctness
// against a realistic sequence of postings: disbursement, one day's
// accrual, a late fee, then a repayment whose waterfall clears the fee
// and interest and partially clears principal.
func TestRebuildFromLines_Sequence(t *testing.T) {
	lines := []StatementLine{
		// PR-DISB-01: Dr LoanReceivable 100000 / Cr CashNostro 100000
		line(GLAccountLoanReceivable, Debit, 100000),
		line("1010", Credit, 100000),
		// PR-ACCR-01: Dr InterestReceivable 41 / Cr InterestIncome 41
		line(GLAccountInterestReceivable, Debit, 41),
		line("4100", Credit, 41),
		// PR-DELINQ-01: Dr FeeReceivable 2500 / Cr FeeIncome 2500
		line(GLAccountFeeReceivable, Debit, 2500),
		line("4200", Credit, 2500),
		// PR-REPAY-01: Dr CashNostro 3041 / Cr FeeReceivable 2500, InterestReceivable 41, LoanReceivable 500
		line("1010", Debit, 3041),
		line(GLAccountFeeReceivable, Credit, 2500),
		line(GLAccountInterestReceivable, Credit, 41),
		line(GLAccountLoanReceivable, Credit, 500),
	}

	p, err := RebuildFromLines("loan-1", lines, time.Unix(1000, 0))
	if err != nil {
		t.Fatalf("RebuildFromLines: %v", err)
	}
	if p.OutstandingPrincipal.Amount != 99500 {
		t.Fatalf("expected outstanding principal 99500 (100000 - 500), got %d", p.OutstandingPrincipal.Amount)
	}
	if p.InterestReceivable.Amount != 0 {
		t.Fatalf("expected interest receivable 0 (41 accrued, 41 repaid), got %d", p.InterestReceivable.Amount)
	}
	if p.FeeReceivable.Amount != 0 {
		t.Fatalf("expected fee receivable 0 (2500 assessed, 2500 repaid), got %d", p.FeeReceivable.Amount)
	}
	if p.LineCount != 6 {
		t.Fatalf("expected 6 lines counted (only the 3 receivable-category lines from each of disb/accr/delinq/repay = 1+1+1+3), got %d", p.LineCount)
	}
	total, err := p.TotalOutstanding()
	if err != nil {
		t.Fatalf("TotalOutstanding: %v", err)
	}
	if total.Amount != 99500 {
		t.Fatalf("expected total outstanding 99500, got %d", total.Amount)
	}
}

// TestRebuildFromLines_IgnoresNonReceivableAccounts confirms CashNostro/
// Income/Allowance lines never contribute to the projection -- only the
// three receivable categories a borrower actually owes against.
func TestRebuildFromLines_IgnoresNonReceivableAccounts(t *testing.T) {
	lines := []StatementLine{
		line("1010", Debit, 999999),  // CashNostro
		line("4100", Credit, 999999), // InterestIncome
		line("1900", Debit, 999999),  // AllowanceForLoanLosses
		line("4300", Credit, 999999), // RecoveryIncome
	}
	p, err := RebuildFromLines("loan-1", lines, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("RebuildFromLines: %v", err)
	}
	if p.OutstandingPrincipal.Amount != 0 || p.InterestReceivable.Amount != 0 || p.FeeReceivable.Amount != 0 {
		t.Fatalf("expected zero projection when no receivable-category line is present, got %+v", p)
	}
	if p.LineCount != 0 {
		t.Fatalf("expected 0 lines counted, got %d", p.LineCount)
	}
}

// TestRebuildFromLines_EmptyIsZero -- a freshly booked, undisbursed
// account has no posted lines at all; the projection must be a valid
// zero value, not an error.
func TestRebuildFromLines_EmptyIsZero(t *testing.T) {
	p, err := RebuildFromLines("loan-1", nil, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("RebuildFromLines: %v", err)
	}
	if p.OutstandingPrincipal.Amount != 0 || p.InterestReceivable.Amount != 0 || p.FeeReceivable.Amount != 0 {
		t.Fatalf("expected zero projection for an account with no posted lines, got %+v", p)
	}
}

// TestRebuildFromLines_AlwaysRecomputesFromFullHistory -- calling
// RebuildFromLines twice with the SAME full history must produce
// identical results, proving it never depends on a previous call's
// output (no incremental-delta path exists to drift).
func TestRebuildFromLines_AlwaysRecomputesFromFullHistory(t *testing.T) {
	lines := []StatementLine{
		line(GLAccountLoanReceivable, Debit, 50000),
		line("1010", Credit, 50000),
	}
	p1, err := RebuildFromLines("loan-1", lines, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	p2, err := RebuildFromLines("loan-1", lines, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	if p1.OutstandingPrincipal != p2.OutstandingPrincipal {
		t.Fatalf("expected identical results from identical input: %+v vs %+v", p1, p2)
	}
}

func TestRebuildFromLines_MultiCurrencyRejected(t *testing.T) {
	lines := []StatementLine{
		line(GLAccountLoanReceivable, Debit, 100),
		{GLAccount: GLAccountLoanReceivable, Direction: Debit, Amount: Money{Amount: 100, Currency: "EUR"}, PostedAt: time.Unix(0, 0)},
	}
	_, err := RebuildFromLines("loan-1", lines, time.Unix(0, 0))
	if err == nil {
		t.Fatalf("expected a multi-currency statement to be rejected")
	}
}
