package domain

import "time"

// Capitalization mirrors GLPostingAPI's Capitalization shape —
// duplicated rather than shared across the service boundary, same
// rationale as Allocation. Present on ApplyModificationRequest only for
// Branch A (REQ-CB-MOD-003).
type Capitalization struct {
	InterestAmount Money
	FeeAmount      Money
}

func (c Capitalization) Total() (Money, error) {
	return c.InterestAmount.Add(c.FeeAmount)
}

// Modification mirrors the Modification schema. ModificationID equals
// the Idempotency-Key used on applyModification (modificationReferenceId).
//
// CapitalizedAmount and JournalEntryID are both nil for a Branch B
// (rate/term-only) modification — REQ-CB-MOD-004's no-op means there is
// nothing to reference, not a zero-amount posting.
type Modification struct {
	ModificationID      string
	LoanAccountID       string
	EffectiveDate       time.Time
	PreviousTermVersion int
	NewTermVersion      int
	CapitalizedAmount   *Money
	JournalEntryID      *string
	ConfirmedBy         string
	UpdatedAt           time.Time
}
