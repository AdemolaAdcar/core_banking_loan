package domain

import "time"

// DisbursementStatus mirrors loan-account-subledger.yaml#/components/
// schemas/Disbursement.status exactly.
type DisbursementStatus string

const (
	DisbursementPendingDisbursement DisbursementStatus = "PendingDisbursement"
	DisbursementRejected            DisbursementStatus = "Rejected"
	DisbursementDisbursed           DisbursementStatus = "Disbursed"
	DisbursementReversalPending     DisbursementStatus = "ReversalPending"
	DisbursementReversed            DisbursementStatus = "Reversed"
)

// Disbursement mirrors the Disbursement schema. DisbursementID equals
// the Idempotency-Key used on createDisbursement (REQ-CB-DISB-004).
type Disbursement struct {
	DisbursementID  string
	LoanAccountID   string
	Status          DisbursementStatus
	PrincipalAmount Money
	RejectionReason *string
	JournalEntryID  *string
	InstructionID   *string
	RequestedBy     string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}
