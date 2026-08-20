package postingrules

import (
	"testing"

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/coa"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/domain"
)

func usd(amount int64) domain.Money { return domain.Money{Amount: amount, Currency: "USD"} }

func assertLine(t *testing.T, l domain.Line, account string, dir domain.Direction, amount domain.Money) {
	t.Helper()
	if l.GLAccount != account {
		t.Errorf("expected account %s, got %s", account, l.GLAccount)
	}
	if l.Direction != dir {
		t.Errorf("expected direction %s, got %s", dir, l.Direction)
	}
	if l.Amount != amount {
		t.Errorf("expected amount %+v, got %+v", amount, l.Amount)
	}
}

func assertBalances(t *testing.T, lines []domain.Line) {
	t.Helper()
	if err := domain.ValidateBalanced(lines); err != nil {
		t.Fatalf("expected lines to balance: %v", err)
	}
}

// --- PR-DISB-01 ---

func TestPRDISB01Lines(t *testing.T) {
	lines := PRDISB01Lines(usd(1500000))
	assertBalances(t, lines)
	assertLine(t, lines[0], coa.LoanReceivable, domain.Debit, usd(1500000))
	assertLine(t, lines[1], coa.CashNostro, domain.Credit, usd(1500000))
}

// --- PR-ACCR-01 ---

func TestPRACCR01Lines(t *testing.T) {
	lines := PRACCR01Lines(usd(616))
	assertBalances(t, lines)
	assertLine(t, lines[0], coa.InterestReceivable, domain.Debit, usd(616))
	assertLine(t, lines[1], coa.InterestIncome, domain.Credit, usd(616))
}

// --- PR-DELINQ-01 ---

func TestPRDELINQ01Lines(t *testing.T) {
	lines := PRDELINQ01Lines(usd(2500))
	assertBalances(t, lines)
	assertLine(t, lines[0], coa.FeeReceivable, domain.Debit, usd(2500))
	assertLine(t, lines[1], coa.FeeIncome, domain.Credit, usd(2500))
}

// --- PR-CHGOFF-02 (recovery, NOT a reversal) ---

func TestPRCHGOFF02Lines(t *testing.T) {
	lines := PRCHGOFF02Lines(usd(15000))
	assertBalances(t, lines)
	assertLine(t, lines[0], coa.CashNostro, domain.Debit, usd(15000))
	assertLine(t, lines[1], coa.RecoveryIncome, domain.Credit, usd(15000))
}

func TestPRCHGOFF02_IsNotARegisteredReversalRule(t *testing.T) {
	if IsReversalRule(PRCHGOFF02) {
		t.Fatalf("PR-CHGOFF-02 must not be classified as a reversal rule -- a recovery does not undo the write-off decision")
	}
}

// --- PR-REPAY-01: entries with more than two lines ---

func TestPRREPAY01Lines_FullAllocation_ProducesFourLines(t *testing.T) {
	a := Allocation{FeeAmount: usd(2500), InterestAmount: usd(18734), PrincipalAmount: usd(28766)}
	lines, err := PRREPAY01Lines(a)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (1 debit + 3 credits), got %d", len(lines))
	}
	assertBalances(t, lines)
	assertLine(t, lines[0], coa.CashNostro, domain.Debit, usd(50000)) // 2500+18734+28766
	assertLine(t, lines[1], coa.FeeReceivable, domain.Credit, usd(2500))
	assertLine(t, lines[2], coa.InterestReceivable, domain.Credit, usd(18734))
	assertLine(t, lines[3], coa.LoanReceivable, domain.Credit, usd(28766))
}

func TestPRREPAY01Lines_ZeroCategoryOmitted(t *testing.T) {
	a := Allocation{FeeAmount: usd(0), InterestAmount: usd(18734), PrincipalAmount: usd(28766)}
	lines, err := PRREPAY01Lines(a)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (zero-fee category omitted), got %d", len(lines))
	}
	assertBalances(t, lines)
	for _, l := range lines {
		if l.GLAccount == coa.FeeReceivable {
			t.Fatalf("expected no FeeReceivable line when feeAmount is 0")
		}
	}
}

// --- PR-PAYOFF-01 ---

func TestPRPAYOFF01Lines(t *testing.T) {
	a := Allocation{FeeAmount: usd(0), InterestAmount: usd(18734), PrincipalAmount: usd(1481266)}
	lines, err := PRPAYOFF01Lines(a)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertBalances(t, lines)
	assertLine(t, lines[0], coa.CashNostro, domain.Debit, usd(1500000))
	assertLine(t, lines[1], coa.InterestReceivable, domain.Credit, usd(18734))
	assertLine(t, lines[2], coa.LoanReceivable, domain.Credit, usd(1481266))
}

// --- PR-CHGOFF-01: debits AllowanceForLoanLosses, more than two lines ---

func TestPRCHGOFF01Lines(t *testing.T) {
	a := Allocation{FeeAmount: usd(0), InterestAmount: usd(21450), PrincipalAmount: usd(942100)}
	lines, err := PRCHGOFF01Lines(a)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertBalances(t, lines)
	assertLine(t, lines[0], coa.AllowanceForLoanLosses, domain.Debit, usd(963550))
	assertLine(t, lines[1], coa.InterestReceivable, domain.Credit, usd(21450))
	assertLine(t, lines[2], coa.LoanReceivable, domain.Credit, usd(942100))
}

// --- PR-MOD-01: capitalization shape ---

func TestPRMOD01Lines_BothCategories(t *testing.T) {
	c := Capitalization{InterestAmount: usd(8420), FeeAmount: usd(5000)}
	lines, err := PRMOD01Lines(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertBalances(t, lines)
	assertLine(t, lines[0], coa.LoanReceivable, domain.Debit, usd(13420))
	assertLine(t, lines[1], coa.InterestReceivable, domain.Credit, usd(8420))
	assertLine(t, lines[2], coa.FeeReceivable, domain.Credit, usd(5000))
}

func TestPRMOD01Lines_TouchesNoIncomeAccount(t *testing.T) {
	c := Capitalization{InterestAmount: usd(8420), FeeAmount: usd(5000)}
	lines, err := PRMOD01Lines(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, l := range lines {
		if l.GLAccount == coa.InterestIncome || l.GLAccount == coa.FeeIncome {
			t.Fatalf("PR-MOD-01 must never touch an Income account, got a line on %s", l.GLAccount)
		}
	}
}

func TestPRMOD01Lines_OnlyInterestCategory_FeeLineOmitted(t *testing.T) {
	c := Capitalization{InterestAmount: usd(8420), FeeAmount: usd(0)}
	lines, err := PRMOD01Lines(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (fee category omitted), got %d", len(lines))
	}
}

// --- Rule catalog / classification consistency ---

func TestShapeFor_MatchesSpecMapping(t *testing.T) {
	amountRules := []RuleCode{PRDISB01, PRDISB02, PRACCR01, PRDELINQ01, PRDELINQ02, PRCHGOFF02}
	for _, r := range amountRules {
		shape, err := ShapeFor(r)
		if err != nil || shape != ShapeAmount {
			t.Errorf("expected %s to require 'amount', got %v (err=%v)", r, shape, err)
		}
	}
	allocationRules := []RuleCode{PRREPAY01, PRREPAY02, PRPAYOFF01, PRCHGOFF01}
	for _, r := range allocationRules {
		shape, err := ShapeFor(r)
		if err != nil || shape != ShapeAllocation {
			t.Errorf("expected %s to require 'allocation', got %v (err=%v)", r, shape, err)
		}
	}
	shape, err := ShapeFor(PRMOD01)
	if err != nil || shape != ShapeCapitalization {
		t.Errorf("expected PR-MOD-01 to require 'capitalization', got %v (err=%v)", shape, err)
	}
}

func TestIsKnownRuleCode(t *testing.T) {
	if !IsKnownRuleCode("PR-DISB-01") {
		t.Fatalf("expected PR-DISB-01 to be known")
	}
	if IsKnownRuleCode("PR-NOT-A-REAL-RULE") {
		t.Fatalf("expected an unknown rule code to be rejected")
	}
}

func TestRequiredMetadataKeys(t *testing.T) {
	if got := RequiredMetadataKeys(PRACCR01); len(got) != 5 {
		t.Fatalf("expected 5 required metadata keys for PR-ACCR-01, got %v", got)
	}
	if got := RequiredMetadataKeys(PRDELINQ02); len(got) != 2 {
		t.Fatalf("expected 2 required metadata keys for PR-DELINQ-02, got %v", got)
	}
	if got := RequiredMetadataKeys(PRCHGOFF01); len(got) != 2 {
		t.Fatalf("expected 2 required metadata keys for PR-CHGOFF-01, got %v", got)
	}
	if got := RequiredMetadataKeys(PRDISB01); got != nil {
		t.Fatalf("expected no required metadata keys for PR-DISB-01, got %v", got)
	}
}

func TestAllRuleCodes_MatchesElevenApprovedRules(t *testing.T) {
	if len(AllRuleCodes) != 11 {
		t.Fatalf("expected exactly 11 approved posting rules, got %d", len(AllRuleCodes))
	}
}
