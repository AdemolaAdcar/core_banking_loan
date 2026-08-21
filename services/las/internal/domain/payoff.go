package domain

import "time"

// PayoffQuote mirrors the PayoffQuote schema — always a live
// calculation (principalAmount from the current BalanceProjection,
// projectedInterestAmount computed forward from the last confirmed
// PR-ACCR-01 through goodThrough), never a cached/idempotent resource.
type PayoffQuote struct {
	QuoteID                 string
	LoanAccountID           string
	GoodThrough             time.Time
	PrincipalAmount         Money
	ProjectedInterestAmount Money
	FeesAmount              Money
	TotalAmountDue          Money
	GeneratedAt             time.Time
}

// PayoffStatus mirrors the Payoff schema's status enum.
type PayoffStatus string

const (
	PayoffPosted PayoffStatus = "Posted"
	PayoffClosed PayoffStatus = "Closed"
)

// Payoff mirrors the Payoff schema. PayoffID equals the paymentReferenceId
// (the Idempotency-Key) from the receiveRepaymentNotification call that
// settled it — no separate ID space.
type Payoff struct {
	PayoffID       string
	LoanAccountID  string
	QuoteID        string
	Status         PayoffStatus
	AmountPosted   Money
	SuspenseAmount Money
	JournalEntryID *string
	ClosedAt       *time.Time
	UpdatedAt      time.Time
}
