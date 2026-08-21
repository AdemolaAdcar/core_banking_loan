package domain

import "time"

// RepaymentStatus mirrors the Repayment schema's status enum exactly.
type RepaymentStatus string

const (
	RepaymentPosted          RepaymentStatus = "Posted"
	RepaymentUnmatched       RepaymentStatus = "Unmatched"
	RepaymentReversalPending RepaymentStatus = "ReversalPending"
	RepaymentReversed        RepaymentStatus = "Reversed"
	RepaymentCorrected       RepaymentStatus = "Corrected"
)

// UnmatchedReason mirrors Repayment.unmatchedReasonCode.
type UnmatchedReason string

const (
	UnmatchedNoMatch        UnmatchedReason = "NO_MATCH"
	UnmatchedAmbiguousMatch UnmatchedReason = "AMBIGUOUS_MATCH"
)

// Allocation is the fee/interest/principal waterfall split — same shape
// as GLPostingAPI's Allocation, duplicated deliberately across the
// service boundary (see loan-account-subledger.yaml's Allocation doc
// comment: not in the API/Event Spec Agent's shared-object list).
type Allocation struct {
	FeeAmount       Money
	InterestAmount  Money
	PrincipalAmount Money
}

func (a Allocation) Total() (Money, error) {
	sum, err := a.FeeAmount.Add(a.InterestAmount)
	if err != nil {
		return Money{}, err
	}
	return sum.Add(a.PrincipalAmount)
}

// Repayment mirrors the Repayment schema. RepaymentID equals the
// Idempotency-Key used on receiveRepaymentNotification (REQ-CB-REPAY-004).
type Repayment struct {
	RepaymentID          string
	LoanAccountID        *string
	Status               RepaymentStatus
	Amount               Money
	Allocation           *Allocation
	SuspenseAmount       Money
	JournalEntryID       *string
	UnmatchedReasonCode  *UnmatchedReason
	CorrectedRepaymentID *string
	ReceivedAt           time.Time
	UpdatedAt            time.Time
}

// ReversalReason mirrors ReverseRepaymentRequest.reasonCode.
type ReversalReason string

const (
	ReasonPaymentReturned ReversalReason = "PAYMENT_RETURNED"
	ReasonMisapplied      ReversalReason = "MISAPPLIED"
)
