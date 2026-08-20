package domain

import (
	"errors"
	"testing"
	"time"
)

func usd(amount int64) Money { return Money{Amount: amount, Currency: "USD"} }

// --- Invariant 1: every entry's debits sum exactly to its credits, per currency ---

func TestValidateBalanced_TwoLine_Balanced(t *testing.T) {
	lines := []Line{
		{GLAccount: "1200", Direction: Debit, Amount: usd(1500000)},
		{GLAccount: "1010", Direction: Credit, Amount: usd(1500000)},
	}
	if err := ValidateBalanced(lines); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateBalanced_Unbalanced_Rejected(t *testing.T) {
	lines := []Line{
		{GLAccount: "1200", Direction: Debit, Amount: usd(1500000)},
		{GLAccount: "1010", Direction: Credit, Amount: usd(1499999)},
	}
	err := ValidateBalanced(lines)
	var unbalanced *ErrUnbalanced
	if !errors.As(err, &unbalanced) {
		t.Fatalf("expected ErrUnbalanced, got %v", err)
	}
	if unbalanced.DebitTotal != 1500000 || unbalanced.CreditTotal != 1499999 {
		t.Fatalf("unexpected totals: %+v", unbalanced)
	}
}

// --- Edge case: entries with more than two lines ---

func TestValidateBalanced_MoreThanTwoLines_Balanced(t *testing.T) {
	// Mirrors PR-REPAY-01's shape: 1 debit, 3 credits.
	lines := []Line{
		{GLAccount: "1010", Direction: Debit, Amount: usd(50000)},
		{GLAccount: "1400", Direction: Credit, Amount: usd(2500)},
		{GLAccount: "1300", Direction: Credit, Amount: usd(18734)},
		{GLAccount: "1200", Direction: Credit, Amount: usd(28766)},
	}
	if err := ValidateBalanced(lines); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateBalanced_MoreThanTwoLines_Unbalanced_Rejected(t *testing.T) {
	lines := []Line{
		{GLAccount: "1010", Direction: Debit, Amount: usd(50000)},
		{GLAccount: "1400", Direction: Credit, Amount: usd(2500)},
		{GLAccount: "1300", Direction: Credit, Amount: usd(18734)},
		{GLAccount: "1200", Direction: Credit, Amount: usd(28765)}, // off by 1
	}
	var unbalanced *ErrUnbalanced
	if err := ValidateBalanced(lines); !errors.As(err, &unbalanced) {
		t.Fatalf("expected ErrUnbalanced, got %v", err)
	}
}

func TestValidateBalanced_FewerThanTwoLines_Rejected(t *testing.T) {
	lines := []Line{{GLAccount: "1200", Direction: Debit, Amount: usd(100)}}
	if err := ValidateBalanced(lines); err == nil {
		t.Fatalf("expected an error for a single-line entry")
	}
}

// --- Edge case: multi-currency rejection ---

func TestValidateBalanced_MultiCurrency_Rejected(t *testing.T) {
	lines := []Line{
		{GLAccount: "1200", Direction: Debit, Amount: Money{Amount: 1000, Currency: "USD"}},
		{GLAccount: "1010", Direction: Credit, Amount: Money{Amount: 1000, Currency: "EUR"}},
	}
	var multiCur *ErrMultiCurrency
	err := ValidateBalanced(lines)
	if !errors.As(err, &multiCur) {
		t.Fatalf("expected ErrMultiCurrency, got %v", err)
	}
}

func TestValidateBalanced_ZeroOrNegativeAmount_Rejected(t *testing.T) {
	lines := []Line{
		{GLAccount: "1200", Direction: Debit, Amount: usd(0)},
		{GLAccount: "1010", Direction: Credit, Amount: usd(0)},
	}
	if err := ValidateBalanced(lines); err == nil {
		t.Fatalf("expected an error for zero-amount lines")
	}
}

// --- NewJournalEntry: the only constructor, always balanced by construction ---

func TestNewJournalEntry_Balanced_Succeeds(t *testing.T) {
	lines := []JournalEntryLine{
		{Line: Line{GLAccount: "1200", Direction: Debit, Amount: usd(1500000)}, RunningBalanceAfter: usd(1500000)},
		{Line: Line{GLAccount: "1010", Direction: Credit, Amount: usd(1500000)}, RunningBalanceAfter: usd(-1500000)},
	}
	postedAt := time.Date(2026, 8, 16, 9, 14, 0, 0, time.UTC)
	entry, err := NewJournalEntry("je-1", "disb:abc", "PR-DISB-01", "1.0.0", "loan-1", lines, postedAt, false, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !entry.Balanced() || !entry.Immutable() {
		t.Fatalf("expected Balanced()=true and Immutable()=true")
	}
	if entry.PeriodID != "2026-08" {
		t.Fatalf("expected periodId 2026-08, got %s", entry.PeriodID)
	}
}

func TestNewJournalEntry_Unbalanced_Refused(t *testing.T) {
	lines := []JournalEntryLine{
		{Line: Line{GLAccount: "1200", Direction: Debit, Amount: usd(100)}},
		{Line: Line{GLAccount: "1010", Direction: Credit, Amount: usd(99)}},
	}
	_, err := NewJournalEntry("je-1", "disb:abc", "PR-DISB-01", "1.0.0", "loan-1", lines, time.Now(), false, nil, nil, nil)
	var unbalanced *ErrUnbalanced
	if !errors.As(err, &unbalanced) {
		t.Fatalf("expected ErrUnbalanced, got %v", err)
	}
}

// --- Reversal of a reversal ---

func TestMirrorForReversal_FlipsEveryDirection(t *testing.T) {
	original := []Line{
		{GLAccount: "1200", Direction: Debit, Amount: usd(1500000)},
		{GLAccount: "1010", Direction: Credit, Amount: usd(1500000)},
	}
	reversal := MirrorForReversal(original)
	if reversal[0].Direction != Credit || reversal[1].Direction != Debit {
		t.Fatalf("expected directions flipped, got %+v", reversal)
	}
	if reversal[0].Amount != original[0].Amount || reversal[0].GLAccount != original[0].GLAccount {
		t.Fatalf("expected same account/amount, only direction flipped: %+v vs %+v", reversal[0], original[0])
	}
}

func TestMirrorForReversal_ReversalOfAReversal_RestoresOriginalDirections(t *testing.T) {
	original := []Line{
		{GLAccount: "1200", Direction: Debit, Amount: usd(1500000)},
		{GLAccount: "1010", Direction: Credit, Amount: usd(1500000)},
	}
	reversalOfOriginal := MirrorForReversal(original)           // A -> B
	reversalOfReversal := MirrorForReversal(reversalOfOriginal) // B -> C

	for i := range original {
		if reversalOfReversal[i].Direction != original[i].Direction {
			t.Fatalf("expected reversal-of-a-reversal to restore the original direction at line %d: got %s, want %s", i, reversalOfReversal[i].Direction, original[i].Direction)
		}
		if reversalOfReversal[i].Amount != original[i].Amount || reversalOfReversal[i].GLAccount != original[i].GLAccount {
			t.Fatalf("expected same account/amount preserved through double reversal at line %d", i)
		}
	}
	// And still balances -- reversing already-balanced lines twice must
	// still produce a balanced entry.
	if err := ValidateBalanced(reversalOfReversal); err != nil {
		t.Fatalf("expected reversal-of-a-reversal lines to still balance: %v", err)
	}
}

func TestOpposite(t *testing.T) {
	if Debit.Opposite() != Credit {
		t.Fatalf("expected Debit.Opposite() == Credit")
	}
	if Credit.Opposite() != Debit {
		t.Fatalf("expected Credit.Opposite() == Debit")
	}
}

func TestPeriodID(t *testing.T) {
	got := PeriodID(time.Date(2026, 1, 5, 23, 59, 0, 0, time.UTC))
	if got != "2026-01" {
		t.Fatalf("expected 2026-01, got %s", got)
	}
}
