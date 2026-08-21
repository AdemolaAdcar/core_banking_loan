// Package domain holds the Loan Account Subledger's core entities: the
// LoanAccount state machine, term versioning, the write-side resources
// (Disbursement, Repayment, Payoff, ChargeOff, Recovery, Modification,
// Fee), and the balance projection. Nothing in this package performs
// I/O — persistence lives in internal/store, GL orchestration in
// internal/service.
package domain

import "fmt"

// Money mirrors GLPostingAPI's shared Money shape: integer minor units,
// never a float — see services/gl/internal/domain's identical type and
// specs/schemas/money.schema.json.
type Money struct {
	Amount   int64
	Currency string
}

func (m Money) IsZero() bool { return m.Amount == 0 }

func (m Money) Add(other Money) (Money, error) {
	if m.Amount == 0 && m.Currency == "" {
		return other, nil
	}
	if other.Amount == 0 && other.Currency == "" {
		return m, nil
	}
	if m.Currency != other.Currency {
		return Money{}, fmt.Errorf("domain: cannot add %s to %s: currency mismatch", other.Currency, m.Currency)
	}
	return Money{Amount: m.Amount + other.Amount, Currency: m.Currency}, nil
}

func (m Money) Sub(other Money) (Money, error) {
	if other.Amount == 0 && other.Currency == "" {
		return m, nil
	}
	if m.Currency != other.Currency {
		return Money{}, fmt.Errorf("domain: cannot subtract %s from %s: currency mismatch", other.Currency, m.Currency)
	}
	return Money{Amount: m.Amount - other.Amount, Currency: m.Currency}, nil
}
