package domain

import "time"

// ChargeOffStatus mirrors the ChargeOff schema's status enum. This is
// deliberately a SEPARATE status space from LoanAccount.Status — a
// ChargeOff resource starts PendingChargeOff and only becomes
// ChargedOff once GL confirms PR-CHGOFF-01, which is the same instant
// LoanAccount.Status itself transitions Disbursed -> ChargedOff (see
// loan_account.go's validTransitions).
type ChargeOffStatus string

const (
	ChargeOffPending ChargeOffStatus = "PendingChargeOff"
	ChargeOffDone    ChargeOffStatus = "ChargedOff"
)

// ChargeOff mirrors the ChargeOff schema. ChargeoffID equals the
// Idempotency-Key used on recordChargeOff (chargeoffDecisionReference).
type ChargeOff struct {
	ChargeoffID                string
	LoanAccountID              string
	Status                     ChargeOffStatus
	WriteOffAmount             Money
	JournalEntryID             *string
	ChargeoffDecisionReference string
	ConfirmedBy                string
	ChargedOffAt               *time.Time
	UpdatedAt                  time.Time
}

// Recovery mirrors the Recovery schema. RecoveryID equals the
// Idempotency-Key used on the receiveRepaymentNotification call that
// produced it — deliberately not a reference to the original
// PR-CHGOFF-01 entry; a recovery is a distinct entry type (PR-CHGOFF-02),
// never a reversal.
type Recovery struct {
	RecoveryID     string
	LoanAccountID  string
	Amount         Money
	JournalEntryID *string
	ReceivedAt     time.Time
	UpdatedAt      time.Time
}
