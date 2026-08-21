// Package domain holds Payment Execution's core entities: the
// PaymentInstruction state machine and its status-transition rules.
// Nothing in this package performs I/O — persistence lives in
// internal/store, rail integration in internal/railclient and
// internal/rails/*, AccountAPI orchestration in internal/service.
package domain

import "fmt"

// Money mirrors the shared {amount, currency} object every service in
// this system uses — integer minor units, never a float. See
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
