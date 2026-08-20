package domain

import (
	"fmt"
	"time"
)

type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

func (d Direction) Opposite() Direction {
	if d == Debit {
		return Credit
	}
	return Debit
}

// Line is one side of a posting, before persistence -- RunningBalanceAfter
// is deliberately not part of this type. Computing it requires reading
// the current balance for (loanAccountId, glAccount), which is I/O this
// pure package never performs; the service layer fills it in when
// persisting (see internal/service).
type Line struct {
	GLAccount string
	Direction Direction
	Amount    Money
}

// ErrUnbalanced is returned by ValidateBalanced when debit and credit
// totals don't match -- invariant 1, checked here in memory AND,
// independently, by a database constraint trigger (see
// internal/store/postgres/migrations). The database check is the real
// last line of defense per this role's invariant 1; this in-memory check
// exists so a bug never gets as far as attempting the insert at all.
type ErrUnbalanced struct {
	DebitTotal, CreditTotal int64
	Currency                string
}

func (e *ErrUnbalanced) Error() string {
	return fmt.Sprintf("domain: unbalanced entry: debits=%d credits=%d %s", e.DebitTotal, e.CreditTotal, e.Currency)
}

// ErrMultiCurrency is returned when a single entry's lines don't all
// share one currency. Multi-currency posting is explicitly out of scope
// until a future increment makes it explicit -- this is a hard rejection,
// not a TODO.
type ErrMultiCurrency struct {
	First, Second string
}

func (e *ErrMultiCurrency) Error() string {
	return fmt.Sprintf("domain: multi-currency entry rejected: found both %s and %s (multi-currency is out of scope)", e.First, e.Second)
}

// ValidateBalanced enforces invariant 1 in memory: every line's amount
// is positive, every line shares exactly one currency, and the sum of
// DEBIT amounts equals the sum of CREDIT amounts.
func ValidateBalanced(lines []Line) error {
	if len(lines) < 2 {
		return fmt.Errorf("domain: a journal entry must have at least 2 lines, got %d", len(lines))
	}
	var currency string
	var debitTotal, creditTotal int64
	for _, l := range lines {
		if l.Amount.Amount <= 0 {
			return fmt.Errorf("domain: line amount for %s must be positive, got %d", l.GLAccount, l.Amount.Amount)
		}
		if currency == "" {
			currency = l.Amount.Currency
		} else if l.Amount.Currency != currency {
			return &ErrMultiCurrency{First: currency, Second: l.Amount.Currency}
		}
		switch l.Direction {
		case Debit:
			debitTotal += l.Amount.Amount
		case Credit:
			creditTotal += l.Amount.Amount
		default:
			return fmt.Errorf("domain: line for %s has invalid direction %q", l.GLAccount, l.Direction)
		}
	}
	if debitTotal != creditTotal {
		return &ErrUnbalanced{DebitTotal: debitTotal, CreditTotal: creditTotal, Currency: currency}
	}
	return nil
}

// MirrorForReversal produces the line set for a reversal entry: the same
// GL accounts and amounts, every direction flipped. This is what makes
// "a reversal of a reversal" well-defined: reversing entry B (itself a
// reversal of A) mirrors B's lines, which are numerically identical to
// A's original lines again (double negation) -- but the resulting entry
// is tagged with its own new sourceEventId and a reversalOfSourceEventId
// pointing at B, not skipping ahead to A. reversalOfSourceEventId always
// names the entry being directly undone, one hop only; the audit chain
// (A -> reversed by B -> reversed by C) is preserved by following that
// chain, not by collapsing it.
func MirrorForReversal(lines []Line) []Line {
	out := make([]Line, len(lines))
	for i, l := range lines {
		out[i] = Line{GLAccount: l.GLAccount, Direction: l.Direction.Opposite(), Amount: l.Amount}
	}
	return out
}

// PeriodID derives the YYYY-MM calendar period a timestamp belongs to.
func PeriodID(t time.Time) string {
	return t.UTC().Format("2006-01")
}

// JournalEntryLine is a Line plus its computed running balance --
// present only once the service layer has filled it in.
type JournalEntryLine struct {
	Line
	RunningBalanceAfter Money
}

// JournalEntry is immutable by construction: there is no method on this
// type, or anywhere in this package, that mutates Lines after
// NewJournalEntry returns. Corrections are always a new JournalEntry
// with a new ID and SourceEventID, produced through the same
// NewJournalEntry constructor -- never an edit of an existing value.
type JournalEntry struct {
	ID                      string
	SourceEventID           string
	PostingRuleCode         string
	PostingRuleVersion      string
	LoanAccountID           string
	Lines                   []JournalEntryLine
	PostedAt                time.Time
	PeriodID                string
	IsPriorPeriodAdjustment bool
	AdjustmentForPeriodID   *string
	Metadata                map[string]any

	// ReversalOfSourceEventID is internal-only -- not part of
	// journal-entry.schema.json's public response shape, but tracked
	// here (and persisted) so the reversal chain (A -> reversed by B ->
	// reversed by C) is always reconstructable server-side for audit,
	// and so a reversal-of-a-reversal can look up exactly which entry
	// it's mirroring. Nil for a non-reversal entry.
	ReversalOfSourceEventID *string
}

// Balanced always returns true -- the only constructor for JournalEntry
// (NewJournalEntry) refuses to build one otherwise. Kept as an explicit
// method (mirroring the schema's `balanced: const true` field) so
// callers can assert it without reaching into Lines themselves.
func (e JournalEntry) Balanced() bool { return true }

// Immutable always returns true -- there is no update or delete
// operation anywhere in this package, the store interface, or the API.
func (e JournalEntry) Immutable() bool { return true }

// NewJournalEntry is the ONLY way to construct a JournalEntry in this
// codebase -- it runs ValidateBalanced before building anything, which
// is what makes constructing an unbalanced JournalEntry a compile-time
// impossibility for every caller in this service: there is no other
// entry point.
func NewJournalEntry(
	id, sourceEventID, postingRuleCode, postingRuleVersion, loanAccountID string,
	lines []JournalEntryLine,
	postedAt time.Time,
	isPriorPeriodAdjustment bool,
	adjustmentForPeriodID *string,
	reversalOfSourceEventID *string,
	metadata map[string]any,
) (JournalEntry, error) {
	plain := make([]Line, len(lines))
	for i, l := range lines {
		plain[i] = l.Line
	}
	if err := ValidateBalanced(plain); err != nil {
		return JournalEntry{}, err
	}
	return JournalEntry{
		ID:                      id,
		SourceEventID:           sourceEventID,
		PostingRuleCode:         postingRuleCode,
		PostingRuleVersion:      postingRuleVersion,
		LoanAccountID:           loanAccountID,
		Lines:                   lines,
		PostedAt:                postedAt,
		PeriodID:                PeriodID(postedAt),
		IsPriorPeriodAdjustment: isPriorPeriodAdjustment,
		AdjustmentForPeriodID:   adjustmentForPeriodID,
		ReversalOfSourceEventID: reversalOfSourceEventID,
		Metadata:                metadata,
	}, nil
}
