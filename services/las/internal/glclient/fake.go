package glclient

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
)

// Fake is an in-memory Client double for internal/service's unit tests —
// it never performs I/O. It records every PostJournalEntry call (so
// tests can assert "exactly one call, with this exact rule code and
// shape") and lets tests preset both the posted-lines statement a
// GetStatementOfAccount call returns and an error to inject on the next
// PostJournalEntry call (to test how a caller reacts to GL rejecting or
// being unavailable).
type Fake struct {
	mu sync.Mutex

	Calls      []PostJournalEntryInput
	byKey      map[string]JournalEntry
	NextErr    error
	Statements map[string][]domain.StatementLine

	idCounter int
}

func NewFake() *Fake {
	return &Fake{byKey: map[string]JournalEntry{}, Statements: map[string][]domain.StatementLine{}}
}

func (f *Fake) PostJournalEntry(_ context.Context, in PostJournalEntryInput) (JournalEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.Calls = append(f.Calls, in)

	if existing, ok := f.byKey[in.IdempotencyKey]; ok {
		return existing, nil // idempotent replay, same as GL's real behavior
	}

	if f.NextErr != nil {
		err := f.NextErr
		f.NextErr = nil
		return JournalEntry{}, err
	}

	f.idCounter++
	amount, lines := f.linesFor(in)
	entry := JournalEntry{
		JournalEntryID: fmt.Sprintf("je-fake-%d", f.idCounter), SourceEventID: in.IdempotencyKey,
		PostingRuleCode: string(in.PostingRuleCode), PostingRuleVersion: "1.0.0", LoanAccountID: in.LoanAccountID,
		Lines: lines, Balanced: true, Immutable: true, PostedAt: time.Now().UTC(), PeriodID: time.Now().UTC().Format("2006-01"),
	}
	f.byKey[in.IdempotencyKey] = entry
	_ = amount
	return entry, nil
}

// linesFor synthesizes a plausible line set purely for test-double
// bookkeeping (so Fake.Statements can be exercised end to end without a
// test manually pre-seeding it) — it does not replicate GL's actual
// posting-rule catalog, since which GL accounts a rule touches is
// GLPostingAPI's exclusive concern, not this service's.
func (f *Fake) linesFor(in PostJournalEntryInput) (domain.Money, []JournalEntryLine) {
	switch {
	case in.Amount != nil:
		return *in.Amount, []JournalEntryLine{
			{GLAccount: "TEST-DEBIT", Direction: domain.Debit, Amount: *in.Amount},
			{GLAccount: "TEST-CREDIT", Direction: domain.Credit, Amount: *in.Amount},
		}
	case in.Allocation != nil:
		total, _ := in.Allocation.FeeAmount.Add(in.Allocation.InterestAmount)
		total, _ = total.Add(in.Allocation.PrincipalAmount)
		return total, []JournalEntryLine{{GLAccount: "TEST-DEBIT", Direction: domain.Debit, Amount: total}}
	case in.Capitalization != nil:
		total, _ := in.Capitalization.InterestAmount.Add(in.Capitalization.FeeAmount)
		return total, []JournalEntryLine{{GLAccount: "TEST-DEBIT", Direction: domain.Debit, Amount: total}}
	default:
		return domain.Money{}, nil
	}
}

func (f *Fake) GetStatementOfAccount(_ context.Context, loanAccountID string) ([]domain.StatementLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Statements[loanAccountID], nil
}

// CallCount returns how many PostJournalEntry calls were made carrying
// the given rule code — the assertion "every domain intent results in
// exactly one call, using the posting rule named in the design note"
// tests against this directly.
func (f *Fake) CallCountForRule(rule string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if string(c.PostingRuleCode) == rule {
			n++
		}
	}
	return n
}
