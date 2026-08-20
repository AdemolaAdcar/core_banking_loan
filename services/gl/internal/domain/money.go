// Package domain holds the GL Posting Engine's core business types and
// logic, independent of transport (HTTP) and persistence (Postgres).
// This package is the one place in the entire system permitted to
// construct a balanced JournalEntry -- every invariant in this task's
// Section 7.3 is enforced here first, in memory, before anything ever
// reaches the database (which enforces invariant 1 again, independently,
// as the last line of defense -- see internal/store/postgres's
// migration).
package domain

import "fmt"

// Money mirrors specs/schemas/money.schema.json exactly: amount is
// integer minor units, never a float -- floats cannot represent currency
// exactly and this system never uses one for money, anywhere.
type Money struct {
	Amount   int64
	Currency string
}

func (m Money) IsZero() bool { return m.Amount == 0 }

// Add requires both operands to share a currency -- this package never
// silently mixes currencies. Multi-currency is explicitly out of scope
// (see the multi-currency-rejection tests) until a future increment
// makes it explicit.
func (m Money) Add(other Money) (Money, error) {
	if m.Currency != other.Currency {
		return Money{}, fmt.Errorf("domain: cannot add %s to %s: currency mismatch", other.Currency, m.Currency)
	}
	return Money{Amount: m.Amount + other.Amount, Currency: m.Currency}, nil
}
