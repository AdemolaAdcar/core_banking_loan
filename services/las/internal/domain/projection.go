package domain

import "time"

// Direction mirrors GLPostingAPI's line direction — used here only to
// interpret lines GL hands back via GetStatementOfAccount, never to
// construct a posting (LAS never picks a GL account or direction to
// debit/credit; that decision belongs entirely to GL's posting-rule
// catalog — see internal/glclient's doc comment).
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

// GL account codes this package reads when rebuilding a
// BalanceProjection from GLPostingAPI's statement-of-account lines.
// Mirrors services/gl/internal/coa's canonical codes for the three
// receivable categories a loan account's outstanding balance is made
// of. LAS reads these to categorize an already-posted line; it never
// selects one to post against.
const (
	GLAccountLoanReceivable     = "1200"
	GLAccountInterestReceivable = "1300"
	GLAccountFeeReceivable      = "1400"
)

// StatementLine is the minimal shape BalanceProjection rebuilds from —
// one posted GL line touching a loan account, exactly what
// glclient.Client.GetStatementOfAccount returns for that account.
type StatementLine struct {
	JournalEntryID string
	GLAccount      string
	Direction      Direction
	Amount         Money
	PostedAt       time.Time
}

// BalanceProjection is a loan account's current outstanding balance,
// split by receivable category. It is READ-OPTIMIZED CACHE, not a
// source of truth: every field here is only ever produced by
// RebuildFromLines summing GL's actual posted entries for the account —
// there is no method anywhere in this package (or internal/service)
// that increments or decrements a BalanceProjection field directly.
// Persisting the result of a rebuild is a cache write for read
// performance; it is never itself the input to a business decision that
// skips re-deriving from GL. If a balance here is ever wrong, it is
// because the line history fed into RebuildFromLines was wrong or
// stale — never because something wrote to this struct independently.
type BalanceProjection struct {
	LoanAccountID        string
	OutstandingPrincipal Money
	InterestReceivable   Money
	FeeReceivable        Money
	LineCount            int
	RebuiltAt            time.Time
}

// TotalOutstanding is the sum of all three receivable categories — what
// the borrower still owes across principal, interest, and fees.
func (p BalanceProjection) TotalOutstanding() (Money, error) {
	sum, err := p.OutstandingPrincipal.Add(p.InterestReceivable)
	if err != nil {
		return Money{}, err
	}
	return sum.Add(p.FeeReceivable)
}

// RebuildFromLines is the ONLY function in this codebase that produces
// a BalanceProjection. It always recomputes from the FULL line history
// handed to it — never applies an incremental delta to a prior
// projection value — which is what makes "refreshed on gl.entry.posted"
// safe to call redundantly, out of order, or after a missed event: the
// result is always exactly what GL's ledger says right now, never a
// function of what this projection previously held.
//
// Each receivable-category GL account (LoanReceivable, InterestReceivable,
// FeeReceivable) carries a normal DEBIT balance — a DEBIT line increases
// the outstanding amount, a CREDIT line decreases it (a repayment credits
// the receivable it clears). Lines against any other GL account
// (CashNostro, the income accounts, AllowanceForLoanLosses) are present
// in a loan account's full statement but are not part of what the
// borrower still owes, so they contribute nothing to this projection —
// deliberately, not by omission.
func RebuildFromLines(loanAccountID string, lines []StatementLine, at time.Time) (BalanceProjection, error) {
	p := BalanceProjection{LoanAccountID: loanAccountID, RebuiltAt: at}

	// overallCurrency is validated against EVERY receivable-category
	// line, not just lines within the same category -- a loan account
	// has exactly one currency for its whole life (its TermSet's
	// principalAmount.currency), so a line in a second currency turning
	// up anywhere in this account's receivable history is always a
	// defect, never legitimate multi-currency activity. GL itself
	// already rejects a multi-currency JOURNAL ENTRY at posting time;
	// this is the analogous check across an account's full history.
	var overallCurrency string
	for _, l := range lines {
		if l.GLAccount != GLAccountLoanReceivable && l.GLAccount != GLAccountInterestReceivable && l.GLAccount != GLAccountFeeReceivable {
			continue
		}
		if !l.Amount.IsZero() {
			if overallCurrency == "" {
				overallCurrency = l.Amount.Currency
			} else if l.Amount.Currency != overallCurrency {
				return BalanceProjection{}, &ErrMultiCurrencyProjection{First: overallCurrency, Second: l.Amount.Currency}
			}
		}

		signed := l.Amount
		if l.Direction == Credit {
			signed.Amount = -signed.Amount
		}
		var err error
		switch l.GLAccount {
		case GLAccountLoanReceivable:
			p.OutstandingPrincipal, err = addSigned(p.OutstandingPrincipal, signed)
		case GLAccountInterestReceivable:
			p.InterestReceivable, err = addSigned(p.InterestReceivable, signed)
		case GLAccountFeeReceivable:
			p.FeeReceivable, err = addSigned(p.FeeReceivable, signed)
		}
		if err != nil {
			return BalanceProjection{}, err
		}
		p.LineCount++
	}

	if overallCurrency == "" {
		overallCurrency = "USD"
	}
	p.OutstandingPrincipal.Currency = overallCurrency
	p.InterestReceivable.Currency = overallCurrency
	p.FeeReceivable.Currency = overallCurrency
	return p, nil
}

// addSigned adds two Money values that may carry an already-negated
// (credit-side) amount, tolerating a zero-value/uncurrencied accumulator
// the same way Money.Add does.
func addSigned(acc, signed Money) (Money, error) {
	if acc.Amount == 0 && acc.Currency == "" {
		return signed, nil
	}
	if signed.Currency == "" {
		return acc, nil
	}
	if acc.Currency != signed.Currency {
		return Money{}, &ErrMultiCurrencyProjection{First: acc.Currency, Second: signed.Currency}
	}
	return Money{Amount: acc.Amount + signed.Amount, Currency: acc.Currency}, nil
}

// ErrMultiCurrencyProjection mirrors GL's own multi-currency rejection —
// a loan account's statement mixing currencies would mean GL itself
// violated its own invariant, since GL rejects multi-currency entries at
// the source; this is a defensive check, not an expected path.
type ErrMultiCurrencyProjection struct {
	First, Second string
}

func (e *ErrMultiCurrencyProjection) Error() string {
	return "domain: loan account statement mixes currencies " + e.First + " and " + e.Second + " — GL should have rejected this at posting time"
}
