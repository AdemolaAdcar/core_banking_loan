package domain

import (
	"fmt"
	"time"
)

// Direction mirrors payment-instruction.schema.json#/$defs/PaymentInstruction/properties/direction.
type Direction string

const (
	Outbound Direction = "OUTBOUND"
	Inbound  Direction = "INBOUND"
)

// Purpose mirrors .../purpose.
type Purpose string

const (
	PurposeDisbursement Purpose = "DISBURSEMENT"
	PurposeRepayment    Purpose = "REPAYMENT"
	PurposePayoff       Purpose = "PAYOFF"
)

// Status mirrors .../status exactly — this package's ONLY vocabulary for
// where a PaymentInstruction stands. There is deliberately no "Settled" or
// "Pending" status beyond what the shipped schema defines: a rail whose
// own lifecycle has more granularity (ACH's provisional-then-final
// settlement window, for instance) still collapses onto these four values
// from this service's point of view — see internal/rails/ach's doc
// comment for how that adapter maps its own states onto this enum.
type Status string

const (
	StatusSubmitted Status = "Submitted"
	StatusExecuted  Status = "Executed"
	StatusFailed    Status = "Failed"
	StatusReturned  Status = "Returned"
)

// FailureReason mirrors .../failureReason's enum.
type FailureReason string

const (
	ReasonPaymentReturned FailureReason = "PAYMENT_RETURNED"
	ReasonPaymentFailed   FailureReason = "PAYMENT_FAILED"
	ReasonRailRejected    FailureReason = "RAIL_REJECTED"
)

// validTransitions is the ONLY table this package consults to decide
// whether a status change is allowed.
//
//	Submitted -> Executed   (rail confirms execution/settlement)
//	Submitted -> Failed     (rail rejects before ever executing —
//	                         RAIL_REJECTED/PAYMENT_FAILED)
//	Submitted -> Returned   (rail returns the payment without this
//	                         service ever having received a separate
//	                         Executed confirmation first — a real ACH
//	                         possibility when a batch return arrives in
//	                         the same or an earlier cycle than any
//	                         settlement acknowledgment would have)
//	Executed  -> Returned   (a payment this service already reported as
//	                         executed is later reversed by the receiving
//	                         bank/rail — the case
//	                         REQ-CB-DISB-007/REQ-CB-REPAY-007's reversal
//	                         flows on the AccountAPI side of this system
//	                         are actually built for: LAS's
//	                         ReverseDisbursement requires the disbursement
//	                         to already be "Disbursed", i.e. this
//	                         instruction was already Executed)
//
// Failed and Returned are both terminal: no row below lists either as a
// "from" state. Executed is terminal for everything except the single
// Returned transition above — there is no path back to Submitted or to
// Failed from Executed.
var validTransitions = map[Status]map[Status]bool{
	StatusSubmitted: {StatusExecuted: true, StatusFailed: true, StatusReturned: true},
	StatusExecuted:  {StatusReturned: true},
	StatusFailed:    {},
	StatusReturned:  {},
}

// ErrInvalidTransition is returned by PaymentInstruction.TransitionTo for
// any (from, to) pair not present in validTransitions — mapped to HTTP
// 409 by internal/api, never silently allowed and never a 500.
type ErrInvalidTransition struct {
	From, To Status
}

func (e *ErrInvalidTransition) Error() string {
	return fmt.Sprintf("domain: invalid payment instruction transition %s -> %s", e.From, e.To)
}

// CanTransition reports whether from -> to is a legal status change,
// without mutating anything.
func CanTransition(from, to Status) bool {
	next, ok := validTransitions[from]
	return ok && next[to]
}

// PaymentInstruction mirrors
// payment-instruction.schema.json#/$defs/PaymentInstruction field-for-
// field. InstructionID equals the Idempotency-Key the initiating caller
// sent — no separate ID space (see the schema's own doc comment on that
// field): for OUTBOUND this is the disbursementId LAS's
// createDisbursement/ConfirmDisbursementFunding call used; for INBOUND
// it is the paymentReferenceId this service's own rail adapter assigns
// on receipt.
type PaymentInstruction struct {
	InstructionID  string
	LoanAccountID  string
	Direction      Direction
	Purpose        Purpose
	Amount         Money
	PartyID        *string // OUTBOUND only; nil for INBOUND
	JournalEntryID *string
	Status         Status
	Rail           *string
	// RailReference is the rail adapter's own tracking reference for
	// this specific payment (railclient.Submission.RailReference for
	// OUTBOUND; railclient.InboundEvent.RailReference for INBOUND) — NOT
	// part of the shipped payment-instruction.schema.json (which
	// deliberately keeps rail wire details out of the shared object;
	// see that schema's own field-level doc comments), so this field is
	// this service's own internal persistence concern only, never
	// serialized into a payment.disbursement.confirmed/failed event
	// payload. Reconciliation (internal/service) looks a confirmation
	// up BY this field.
	RailReference *string
	FailureReason *FailureReason
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// NewOutboundDisbursement constructs a new OUTBOUND/DISBURSEMENT
// instruction in Submitted status — the ONLY way to create one. Per this
// role's own ground rule ("refuse to post a disbursement or repayment
// without a matched, confirmed rail event"), nothing in this package
// or internal/service ever constructs one already in Executed status:
// that transition is only ever reachable through TransitionTo, driven by
// a reconciled rail confirmation.
func NewOutboundDisbursement(instructionID, loanAccountID, partyID, journalEntryID string, amount Money, at time.Time) PaymentInstruction {
	return PaymentInstruction{
		InstructionID: instructionID, LoanAccountID: loanAccountID, Direction: Outbound, Purpose: PurposeDisbursement,
		Amount: amount, PartyID: &partyID, JournalEntryID: &journalEntryID, Status: StatusSubmitted,
		CreatedAt: at, UpdatedAt: at,
	}
}

// NewInboundReceipt constructs a new INBOUND instruction already in
// Executed status — unlike an outbound disbursement, an inbound payment
// has, by construction, already arrived by the time this service learns
// about it from the rail; there is no earlier "Submitted" state to pass
// through on this service's side. purpose is REPAYMENT here; the
// REPAYMENT-vs-PAYOFF determination belongs to AccountAPI's
// receiveRepaymentNotification, not to this service (see
// internal/service's doc comment) — the initial event/local record
// always carries REPAYMENT and is never rewritten based on AccountAPI's
// answer, matching payment-execution-events.yaml's own
// PaymentInboundReceivedPayload, which likewise never carries a purpose
// field.
func NewInboundReceipt(instructionID, rail, railReference string, amount Money, at time.Time) PaymentInstruction {
	return PaymentInstruction{
		InstructionID: instructionID, Direction: Inbound, Purpose: PurposeRepayment,
		Amount: amount, Status: StatusExecuted, Rail: &rail, RailReference: &railReference,
		CreatedAt: at, UpdatedAt: at,
	}
}

// WithRailReference returns a copy with RailReference set — used once
// Initiate's Submission comes back with the rail's own tracking
// reference for an OUTBOUND instruction (NewOutboundDisbursement itself
// doesn't take one, since it's constructed before Initiate is ever
// called).
func (p PaymentInstruction) WithRailReference(railReference string, at time.Time) PaymentInstruction {
	next := p
	next.RailReference = &railReference
	next.UpdatedAt = at
	return next
}

// TransitionTo returns a new PaymentInstruction value with Status
// advanced to `to`, UpdatedAt set to `at`, and (for a Failed/Returned
// transition) failureReason recorded, or ErrInvalidTransition if the
// move isn't in validTransitions. The receiver is never mutated in
// place.
func (p PaymentInstruction) TransitionTo(to Status, reason *FailureReason, at time.Time) (PaymentInstruction, error) {
	if !CanTransition(p.Status, to) {
		return PaymentInstruction{}, &ErrInvalidTransition{From: p.Status, To: to}
	}
	next := p
	next.Status = to
	next.UpdatedAt = at
	if to == StatusFailed || to == StatusReturned {
		next.FailureReason = reason
	}
	return next, nil
}

// IsTerminal reports whether no further transition is possible at all —
// true for Failed/Returned, false for Executed (which can still move to
// Returned) and Submitted.
func (p PaymentInstruction) IsTerminal() bool {
	return len(validTransitions[p.Status]) == 0
}
