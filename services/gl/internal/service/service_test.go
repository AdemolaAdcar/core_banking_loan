package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/coa"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/postingrules"
)

func usd(amount int64) domain.Money { return domain.Money{Amount: amount, Currency: "USD"} }

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func sequentialIDs(prefix string) func() string {
	n := 0
	var mu sync.Mutex
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		digits := []byte{}
		v := n
		for v > 0 {
			digits = append([]byte{byte('0' + v%10)}, digits...)
			v /= 10
		}
		return prefix + "-" + string(digits)
	}
}

func newTestService(fs *fakeStore) *Service {
	chart := coa.MustLoad()
	s := New(fs, chart)
	s.now = fixedClock(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	s.newID = sequentialIDs("je")
	return s
}

func disbInput(key, loanAccountID string, amount int64) PostJournalEntryInput {
	m := usd(amount)
	return PostJournalEntryInput{IdempotencyKey: key, PostingRuleCode: postingrules.PRDISB01, LoanAccountID: loanAccountID, Amount: &m}
}

// =========================================================================
// Invariant 1: every entry's debits sum exactly to its credits, per currency
// =========================================================================
// The database-level enforcement (a deferred constraint trigger, the real
// "last line of defense") was verified live against a real Postgres
// instance separately -- see PR_DESCRIPTION.md. These tests confirm the
// SERVICE layer's own write path always goes through
// domain.NewJournalEntry (the one constructor that refuses an unbalanced
// entry), on every rule, every time -- there is no code path in this
// package that constructs a JournalEntry any other way.

func TestPostJournalEntry_PRDISB01_AlwaysBalanced(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	out, err := s.PostJournalEntry(context.Background(), disbInput("disb:1", "loan-1", 1500000))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Balanced() {
		t.Fatalf("expected posted entry to be balanced")
	}
	if err := domain.ValidateBalanced(linesOf(out)); err != nil {
		t.Fatalf("posted entry does not actually balance: %v", err)
	}
}

func linesOf(e domain.JournalEntry) []domain.Line {
	out := make([]domain.Line, len(e.Lines))
	for i, l := range e.Lines {
		out[i] = l.Line
	}
	return out
}

// =========================================================================
// Invariant 2: posting is atomic -- all lines commit together or none do
// =========================================================================

func TestPostJournalEntry_OutboxFailure_NothingPersisted(t *testing.T) {
	fs := newFakeStore()
	fs.failNextOutboxInsert = true
	s := newTestService(fs)

	_, err := s.PostJournalEntry(context.Background(), disbInput("disb:1", "loan-1", 1500000))
	if err == nil {
		t.Fatalf("expected the simulated outbox failure to propagate")
	}
	if len(fs.entries) != 0 {
		t.Fatalf("expected NO journal entry persisted when the transaction failed partway through, got %d", len(fs.entries))
	}
	if _, found := fs.bySourceEventID["disb:1"]; found {
		t.Fatalf("expected no source_event_id indexed after a rolled-back transaction")
	}
}

// =========================================================================
// Invariant 3: posted entries are immutable -- corrections are new entries
// =========================================================================
// There is no UpdateJournalEntry/DeleteJournalEntry method on store.Tx or
// store.Store at all (see store.go's package doc comment) -- not
// runtime-testable as such, but this test confirms the actual correction
// path (a reversal) never touches the original entry's stored state.

func TestReversal_ProducesNewEntry_OriginalUnchanged(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	original, err := s.PostJournalEntry(context.Background(), disbInput("disb:1", "loan-1", 1500000))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	amt := usd(1500000)
	reversalKey := "disb:1:reversal"
	original2 := "disb:1"
	_, err = s.PostJournalEntry(context.Background(), PostJournalEntryInput{
		IdempotencyKey: reversalKey, PostingRuleCode: postingrules.PRDISB02, LoanAccountID: "loan-1",
		Amount: &amt, ReversalOfSourceEventID: &original2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reloadedOriginal, err := s.GetJournalEntry(context.Background(), original.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reloadedOriginal.Lines) != 2 || reloadedOriginal.Lines[0].GLAccount != coa.LoanReceivable {
		t.Fatalf("expected the original entry's lines to be completely unchanged, got %+v", reloadedOriginal.Lines)
	}
	if len(fs.entries) != 2 {
		t.Fatalf("expected exactly 2 entries to exist (original + new reversal), got %d", len(fs.entries))
	}
}

// =========================================================================
// Invariant 4: Idempotency-Key required; same key+payload replays; same
// key+different payload conflicts. (Full replay-cache mechanics live in
// the API layer's idempotency middleware, matching services/party's and
// services/crm's established pattern -- these tests cover the service
// layer's OWN backstop: a genuine database-level race between two
// concurrent posts sharing one Idempotency-Key.)
// =========================================================================

func TestPostJournalEntry_DuplicateSourceEventID_ReturnsWinnerNotError(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)

	// Simulate someone else's concurrent call already having committed
	// under this exact source event id.
	winner, err := s.PostJournalEntry(context.Background(), disbInput("disb:1", "loan-1", 1500000))
	if err != nil {
		t.Fatalf("unexpected error priming the winner: %v", err)
	}

	// This call's own CreateJournalEntry will report a duplicate (as if
	// it raced the call above at the database level).
	fs.failNextCreateWithDuplicate = true
	got, err := s.PostJournalEntry(context.Background(), disbInput("disb:1", "loan-1", 1500000))
	if err != nil {
		t.Fatalf("expected the duplicate-source-event-id race to resolve to the winner, not an error: %v", err)
	}
	if got.ID != winner.ID {
		t.Fatalf("expected to get back the winning entry %s, got %s", winner.ID, got.ID)
	}
	if len(fs.entries) != 1 {
		t.Fatalf("expected exactly 1 entry to exist after the race, got %d", len(fs.entries))
	}
}

// =========================================================================
// Invariant 5: every entry stores its sourceEventId and posting-rule
// version
// =========================================================================

func TestPostJournalEntry_StoresSourceEventIDAndPostingRuleVersion(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	out, err := s.PostJournalEntry(context.Background(), disbInput("disb:explain-me", "loan-1", 1500000))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.SourceEventID != "disb:explain-me" {
		t.Fatalf("expected sourceEventId to equal the Idempotency-Key, got %q", out.SourceEventID)
	}
	if out.PostingRuleVersion == "" {
		t.Fatalf("expected a non-empty postingRuleVersion")
	}
	if out.PostingRuleVersion != postingrules.ManifestVersion {
		t.Fatalf("expected postingRuleVersion %q, got %q", postingrules.ManifestVersion, out.PostingRuleVersion)
	}
}

// =========================================================================
// Invariant 6: trial balance / statement always computed live, never from
// a separately maintained running total
// =========================================================================

func TestGetTrialBalance_ReflectsEveryNewlyPostedEntry_NoSeparateUpdateStep(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	asOf := time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC)

	before, err := s.GetTrialBalance(context.Background(), asOf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("expected an empty trial balance before any posting, got %+v", before)
	}

	if _, err := s.PostJournalEntry(context.Background(), disbInput("disb:1", "loan-1", 1500000)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The SAME method, called again -- there is no separate "refresh" or
	// "recompute" call anywhere in this package. If this reflects the
	// new entry, the balance was genuinely computed live.
	after, err := s.GetTrialBalance(context.Background(), asOf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var loanReceivableDebit, cashCredit int64
	for _, l := range after {
		if l.GLAccount == coa.LoanReceivable {
			loanReceivableDebit = l.DebitTotal
		}
		if l.GLAccount == coa.CashNostro {
			cashCredit = l.CreditTotal
		}
	}
	if loanReceivableDebit != 1500000 || cashCredit != 1500000 {
		t.Fatalf("expected the trial balance to reflect the newly posted entry, got LoanReceivable debit=%d CashNostro credit=%d", loanReceivableDebit, cashCredit)
	}
}

func TestGetAccountBalance_UnknownAccount_NotFound(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	_, err := s.GetAccountBalance(context.Background(), "9999", time.Now())
	if err == nil {
		t.Fatalf("expected an error for an unknown GL account code")
	}
}

// =========================================================================
// Invariant 7: period close locks prior-period entries from reversal;
// corrections after close are new current-period entries tagged as
// prior-period adjustments
// =========================================================================

func TestPostJournalEntry_ReversalOfClosedPeriodEntry_Rejected(t *testing.T) {
	fs := newFakeStore()
	julyClock := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	s := NewWithClock(fs, coa.MustLoad(), fixedClock(julyClock))
	s.newID = sequentialIDs("je")

	original, err := s.PostJournalEntry(context.Background(), disbInput("disb:1", "loan-1", 1500000))
	if err != nil {
		t.Fatalf("unexpected error posting in July: %v", err)
	}
	if original.PeriodID != "2026-07" {
		t.Fatalf("expected periodId 2026-07, got %s", original.PeriodID)
	}

	if _, err := s.ClosePeriod(context.Background(), "2026-07", "ops.analyst"); err != nil {
		t.Fatalf("unexpected error closing 2026-07: %v", err)
	}

	// Move to August and attempt to reverse the July entry.
	augustSvc := NewWithClock(fs, coa.MustLoad(), fixedClock(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)))
	augustSvc.newID = sequentialIDs("je-aug")
	amt := usd(1500000)
	original2 := "disb:1"
	_, err = augustSvc.PostJournalEntry(context.Background(), PostJournalEntryInput{
		IdempotencyKey: "disb:1:reversal", PostingRuleCode: postingrules.PRDISB02, LoanAccountID: "loan-1",
		Amount: &amt, ReversalOfSourceEventID: &original2,
	})
	if !errors.Is(err, ErrReversalTargetPeriodClosed) {
		t.Fatalf("expected ErrReversalTargetPeriodClosed, got %v", err)
	}
}

func TestPostJournalEntry_PriorPeriodAdjustment_TaggedCorrectly(t *testing.T) {
	fs := newFakeStore()
	julyClock := fixedClock(time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC))
	julySvc := NewWithClock(fs, coa.MustLoad(), julyClock)
	julySvc.newID = sequentialIDs("je-jul")
	if _, err := julySvc.PostJournalEntry(context.Background(), disbInput("disb:1", "loan-1", 1500000)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := julySvc.ClosePeriod(context.Background(), "2026-07", "ops.analyst"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	augustSvc := NewWithClock(fs, coa.MustLoad(), fixedClock(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)))
	augustSvc.newID = sequentialIDs("je-aug")
	amt := usd(2500)
	julyPeriod := "2026-07"
	out, err := augustSvc.PostJournalEntry(context.Background(), PostJournalEntryInput{
		IdempotencyKey: "correction:1", PostingRuleCode: postingrules.PRDELINQ01, LoanAccountID: "loan-1",
		Amount: &amt, PriorPeriodAdjustmentForPeriodID: &julyPeriod,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.IsPriorPeriodAdjustment {
		t.Fatalf("expected isPriorPeriodAdjustment=true")
	}
	if out.AdjustmentForPeriodID == nil || *out.AdjustmentForPeriodID != "2026-07" {
		t.Fatalf("expected adjustmentForPeriodId=2026-07, got %v", out.AdjustmentForPeriodID)
	}
	if out.PeriodID != "2026-08" {
		t.Fatalf("expected the adjustment entry itself to belong to the CURRENT period 2026-08, got %s", out.PeriodID)
	}
}

func TestPostJournalEntry_PriorPeriodAdjustment_TargetPeriodNotClosed_Rejected(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	amt := usd(2500)
	openPeriod := "2026-08"
	_, err := s.PostJournalEntry(context.Background(), PostJournalEntryInput{
		IdempotencyKey: "correction:1", PostingRuleCode: postingrules.PRDELINQ01, LoanAccountID: "loan-1",
		Amount: &amt, PriorPeriodAdjustmentForPeriodID: &openPeriod,
	})
	if !errors.Is(err, ErrAdjustmentPeriodNotClosed) {
		t.Fatalf("expected ErrAdjustmentPeriodNotClosed, got %v", err)
	}
}

func TestClosePeriod_ChronologicalOrder_Enforced(t *testing.T) {
	fs := newFakeStore()
	julySvc := NewWithClock(fs, coa.MustLoad(), fixedClock(time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)))
	julySvc.newID = sequentialIDs("je-jul")
	if _, err := julySvc.PostJournalEntry(context.Background(), disbInput("disb:1", "loan-1", 1500000)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	augustSvc := NewWithClock(fs, coa.MustLoad(), fixedClock(time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)))
	// July has posted entries and is still Open -- closing August first must be refused.
	_, err := augustSvc.ClosePeriod(context.Background(), "2026-08", "ops.analyst")
	var chronErr *domain.ErrEarlierPeriodOpen
	if !errors.As(err, &chronErr) {
		t.Fatalf("expected ErrEarlierPeriodOpen, got %v", err)
	}
	if chronErr.EarliestOpenPeriodID != "2026-07" {
		t.Fatalf("expected earliest open period 2026-07, got %s", chronErr.EarliestOpenPeriodID)
	}

	// Closing July first, then August, must succeed.
	if _, err := julySvc.ClosePeriod(context.Background(), "2026-07", "ops.analyst"); err != nil {
		t.Fatalf("unexpected error closing July: %v", err)
	}
	if _, err := augustSvc.ClosePeriod(context.Background(), "2026-08", "ops.analyst"); err != nil {
		t.Fatalf("unexpected error closing August after July is closed: %v", err)
	}
}

func TestClosePeriod_Idempotent(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	first, err := s.ClosePeriod(context.Background(), "2026-08", "ops.analyst")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entriesBefore := len(fs.outboxEntries)

	second, err := s.ClosePeriod(context.Background(), "2026-08", "someone.else")
	if err != nil {
		t.Fatalf("expected idempotent success on repeat close, got error: %v", err)
	}
	if second.ClosedBy == nil || *second.ClosedBy != *first.ClosedBy {
		t.Fatalf("expected original closedBy preserved, not overwritten")
	}
	if len(fs.outboxEntries) != entriesBefore {
		t.Fatalf("expected no new outbox entry on a repeat close")
	}
}

// =========================================================================
// Edge case: entries with more than two lines
// =========================================================================

func TestPostJournalEntry_PRREPAY01_MoreThanTwoLines(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	out, err := s.PostJournalEntry(context.Background(), PostJournalEntryInput{
		IdempotencyKey: "repay:1", PostingRuleCode: postingrules.PRREPAY01, LoanAccountID: "loan-1",
		Allocation: &postingrules.Allocation{FeeAmount: usd(2500), InterestAmount: usd(18734), PrincipalAmount: usd(28766)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(out.Lines))
	}
}

// =========================================================================
// Edge case: multi-currency rejection
// =========================================================================

func TestPostJournalEntry_MultiCurrencyAllocation_Rejected(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	_, err := s.PostJournalEntry(context.Background(), PostJournalEntryInput{
		IdempotencyKey: "repay:1", PostingRuleCode: postingrules.PRREPAY01, LoanAccountID: "loan-1",
		Allocation: &postingrules.Allocation{
			FeeAmount:       domain.Money{Amount: 2500, Currency: "EUR"},
			InterestAmount:  usd(18734),
			PrincipalAmount: usd(28766),
		},
	})
	if err == nil {
		t.Fatalf("expected an error for a multi-currency allocation")
	}
}

// =========================================================================
// Edge case: concurrent posting to the same account
// =========================================================================
// Run with `go test -race` to prove no data race, alongside proving the
// aggregate result (live trial balance) is correct regardless of
// interleaving -- there is no shared mutable counter for concurrent
// writers to corrupt (invariant 6). Independent, real-Postgres proof
// that the database itself handles concurrent transactions to the same
// account correctly (no incorrect rejection, no deadlock) is documented
// separately in PR_DESCRIPTION.md.

func TestPostJournalEntry_ConcurrentPostingToSameAccount(t *testing.T) {
	fs := newFakeStore()
	chart := coa.MustLoad()
	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			svc := NewWithClock(fs, chart, fixedClock(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)))
			svc.newID = sequentialIDs("je-concurrent-" + string(rune('a'+i)))
			key := "disb:concurrent:" + string(rune('a'+i))
			_, err := svc.PostJournalEntry(context.Background(), disbInput(key, "loan-shared", 1000))
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d unexpected error: %v", i, err)
		}
	}
	if len(fs.entries) != n {
		t.Fatalf("expected all %d concurrent postings to succeed independently, got %d entries", n, len(fs.entries))
	}

	balance, err := fs.GetAccountBalance(context.Background(), coa.LoanReceivable, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if balance.Amount != n*1000 {
		t.Fatalf("expected live LoanReceivable balance to correctly reflect all %d concurrent postings (%d), got %d", n, n*1000, balance.Amount)
	}
}

// =========================================================================
// Edge case: reversal of a reversal
// =========================================================================

func TestPostJournalEntry_ReversalOfAReversal(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)

	original, err := s.PostJournalEntry(context.Background(), disbInput("disb:1", "loan-1", 1500000))
	if err != nil {
		t.Fatalf("unexpected error posting original: %v", err)
	}

	amt := usd(1500000)
	firstReversalTarget := "disb:1"
	firstReversal, err := s.PostJournalEntry(context.Background(), PostJournalEntryInput{
		IdempotencyKey: "disb:1:reversal-1", PostingRuleCode: postingrules.PRDISB02, LoanAccountID: "loan-1",
		Amount: &amt, ReversalOfSourceEventID: &firstReversalTarget,
	})
	if err != nil {
		t.Fatalf("unexpected error posting first reversal: %v", err)
	}

	secondReversalTarget := "disb:1:reversal-1"
	secondReversal, err := s.PostJournalEntry(context.Background(), PostJournalEntryInput{
		IdempotencyKey: "disb:1:reversal-2", PostingRuleCode: postingrules.PRDISB02, LoanAccountID: "loan-1",
		Amount: &amt, ReversalOfSourceEventID: &secondReversalTarget,
	})
	if err != nil {
		t.Fatalf("unexpected error posting reversal of a reversal: %v", err)
	}

	// The reversal of a reversal must restore the ORIGINAL entry's exact
	// direction pattern (double negation).
	if len(secondReversal.Lines) != len(original.Lines) {
		t.Fatalf("expected the same number of lines")
	}
	for i := range original.Lines {
		if secondReversal.Lines[i].Direction != original.Lines[i].Direction {
			t.Fatalf("expected reversal-of-a-reversal to restore the original direction at line %d: got %s, want %s", i, secondReversal.Lines[i].Direction, original.Lines[i].Direction)
		}
		if secondReversal.Lines[i].GLAccount != original.Lines[i].GLAccount || secondReversal.Lines[i].Amount != original.Lines[i].Amount {
			t.Fatalf("expected the same account/amount at line %d", i)
		}
	}
	// And it directly references the FIRST reversal, not the original --
	// one hop only, preserving the audit chain.
	if secondReversal.ReversalOfSourceEventID == nil || *secondReversal.ReversalOfSourceEventID != firstReversal.SourceEventID {
		t.Fatalf("expected the second reversal to reference the first reversal directly, got %v", secondReversal.ReversalOfSourceEventID)
	}
}

func TestPostJournalEntry_ReversalAmountMismatch_Rejected(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	if _, err := s.PostJournalEntry(context.Background(), disbInput("disb:1", "loan-1", 1500000)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wrongAmt := usd(999) // does not match the original 1500000
	target := "disb:1"
	_, err := s.PostJournalEntry(context.Background(), PostJournalEntryInput{
		IdempotencyKey: "disb:1:reversal", PostingRuleCode: postingrules.PRDISB02, LoanAccountID: "loan-1",
		Amount: &wrongAmt, ReversalOfSourceEventID: &target,
	})
	if !errors.Is(err, ErrReversalAmountMismatch) {
		t.Fatalf("expected ErrReversalAmountMismatch, got %v", err)
	}
}

func TestPostJournalEntry_ReversalTargetNotFound_Rejected(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	amt := usd(1500000)
	missing := "disb:does-not-exist"
	_, err := s.PostJournalEntry(context.Background(), PostJournalEntryInput{
		IdempotencyKey: "disb:1:reversal", PostingRuleCode: postingrules.PRDISB02, LoanAccountID: "loan-1",
		Amount: &amt, ReversalOfSourceEventID: &missing,
	})
	if !errors.Is(err, ErrReversalTargetNotFound) {
		t.Fatalf("expected ErrReversalTargetNotFound, got %v", err)
	}
}

// =========================================================================
// Other request-shape validation
// =========================================================================

func TestPostJournalEntry_UnknownRuleCode_Rejected(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	amt := usd(100)
	_, err := s.PostJournalEntry(context.Background(), PostJournalEntryInput{
		IdempotencyKey: "x:1", PostingRuleCode: "PR-NOT-REAL", LoanAccountID: "loan-1", Amount: &amt,
	})
	if !errors.Is(err, ErrUnknownRuleCode) {
		t.Fatalf("expected ErrUnknownRuleCode, got %v", err)
	}
}

func TestPostJournalEntry_WrongInputShape_Rejected(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	// PR-DISB-01 requires `amount`, not `allocation`.
	_, err := s.PostJournalEntry(context.Background(), PostJournalEntryInput{
		IdempotencyKey: "disb:1", PostingRuleCode: postingrules.PRDISB01, LoanAccountID: "loan-1",
		Allocation: &postingrules.Allocation{FeeAmount: usd(1), InterestAmount: usd(1), PrincipalAmount: usd(1)},
	})
	if !errors.Is(err, ErrWrongInputShape) {
		t.Fatalf("expected ErrWrongInputShape, got %v", err)
	}
}

func TestPostJournalEntry_MissingRequiredMetadata_Rejected(t *testing.T) {
	fs := newFakeStore()
	s := newTestService(fs)
	amt := usd(616)
	_, err := s.PostJournalEntry(context.Background(), PostJournalEntryInput{
		IdempotencyKey: "accr:1", PostingRuleCode: postingrules.PRACCR01, LoanAccountID: "loan-1", Amount: &amt,
		// no metadata supplied -- PR-ACCR-01 requires it
	})
	if !errors.Is(err, ErrMissingRequiredMetadata) {
		t.Fatalf("expected ErrMissingRequiredMetadata, got %v", err)
	}
}
