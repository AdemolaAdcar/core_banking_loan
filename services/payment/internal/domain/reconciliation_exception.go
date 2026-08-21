package domain

import "time"

// ExceptionKind classifies why a rail event couldn't be reconciled
// against a known PaymentInstruction.
type ExceptionKind string

const (
	// ExceptionUnmatchedConfirmation: a rail Confirm/settlement result
	// referenced a rail reference this service has no OUTBOUND
	// PaymentInstruction for at all.
	ExceptionUnmatchedConfirmation ExceptionKind = "UNMATCHED_CONFIRMATION"
	// ExceptionUnmatchedInboundReturn: a rail-reported return of a
	// previously-received inbound payment referenced an
	// OriginalRailReference this service never recorded a receipt for.
	ExceptionUnmatchedInboundReturn ExceptionKind = "UNMATCHED_INBOUND_RETURN"
	// ExceptionNoCompliantReversalPath: a returned inbound payment whose
	// original PaymentInstruction has Purpose=PAYOFF has no existing
	// AccountAPI reversal endpoint to call — see
	// service.ErrNoCompliantReversalPath's doc comment.
	ExceptionNoCompliantReversalPath ExceptionKind = "NO_COMPLIANT_REVERSAL_PATH"
)

// ReconciliationException is this role's own ground rule made concrete:
// "An unmatched confirmation is logged as an exception, never posted
// speculatively." Nothing in internal/service ever guesses which
// PaymentInstruction an unmatched rail event was probably for — every
// such event becomes exactly one row here instead, for Ops/reconciliation
// triage, and is never handed to AccountAPI or GLPostingAPI.
type ReconciliationException struct {
	ExceptionID   string
	Kind          ExceptionKind
	RailReference string
	Rail          string
	Details       string // free-text: what the rail reported, for triage
	OccurredAt    time.Time
	CreatedAt     time.Time
}
