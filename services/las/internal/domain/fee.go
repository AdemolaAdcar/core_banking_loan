package domain

import "time"

// FeeStatus mirrors the Fee schema's status enum.
type FeeStatus string

const (
	FeeAssessed     FeeStatus = "Assessed"
	FeeWaivePending FeeStatus = "WaivePending"
	FeeWaived       FeeStatus = "Waived"
)

// WaiveReasonCode mirrors WaiveFeeRequest.reasonCode.
type WaiveReasonCode string

const (
	WaiveGoodwill  WaiveReasonCode = "GOODWILL"
	WaiveBankError WaiveReasonCode = "BANK_ERROR"
	WaiveHardship  WaiveReasonCode = "HARDSHIP"
)

// Fee mirrors the Fee schema. FeeID is deterministic:
// late-fee:{loanAccountId}:{scheduledDueDate} — the same value used as
// GLPostingAPI's Idempotency-Key for PR-DELINQ-01.
type Fee struct {
	FeeID            string
	LoanAccountID    string
	ScheduledDueDate time.Time
	Amount           Money
	Status           FeeStatus
	JournalEntryID   *string
	WaivedBy         *string
	WaiveReasonCode  *WaiveReasonCode
	WaivedAt         *time.Time
	AssessedAt       time.Time
	UpdatedAt        time.Time
}
