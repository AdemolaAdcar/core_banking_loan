// Package railclient defines PaymentRailClient — the single abstraction
// every payment rail in this system is implemented behind. Per this
// role's own ground rules: domain code (internal/service) never imports
// a specific rail's SDK or wire format directly; it depends only on this
// interface. Adding a second real rail means writing a new adapter
// package (see internal/rails/ach for the first one, internal/rails/
// sandbox for the deterministic test double) — it is never a change to
// internal/service.
//
// Method-to-ground-rule mapping ("initiate, confirm, receiveInbound,
// returnPayment"), resolved here into concrete, non-overlapping
// responsibilities since no rail-specific documentation existed to
// clarify the terse ground-rule names beyond the design note's mention
// of ACH — flagged in PR_DESCRIPTION.md as an interpretation decision
// for the Architect Agent to review, the same way a prior codegen phase
// flagged an ambiguous ground-rule conflict rather than silently
// guessing:
//
//	Initiate       — send money OUT (a disbursement).
//	Confirm        — check the current outcome of a previously-Initiated
//	                 submission (Pending/Executed/Failed/Returned). The
//	                 one method both a polling loop AND a push-style
//	                 (webhook) adapter funnel into: a push adapter's
//	                 webhook handler just updates its own outcome store
//	                 ahead of time so Confirm reads it back immediately.
//	ReceiveInbound — pull money that ARRIVED, batch-style (matches a
//	                 file-based rail like ACH, which has no per-payment
//	                 push). Also surfaces newly-discovered RETURNS of
//	                 previously-received inbound payments (e.g. an NSF
//	                 return arriving days later) via each result's Kind
//	                 field — ACH processes incoming-credit and return
//	                 files through the same daily batch cadence, so this
//	                 package mirrors that rather than inventing a
//	                 fifth method.
//	ReturnPayment  — send money BACK OUT: an Ops/compliance-initiated
//	                 return of a specific, previously-received inbound
//	                 payment to its original sender (e.g. an
//	                 unauthorized or misdirected credit that must be
//	                 returned within the rail's return window). This is
//	                 an action THIS system originates — distinct from
//	                 ReceiveInbound surfacing a return the RAIL
//	                 originated on money we sent or received.
package railclient

import (
	"context"
	"errors"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
)

// InitiateInput is what this service knows before ever contacting a
// rail: the confirmed, already-posted GL entry that authorizes this
// payment (per this role's refusal clause below), the destination, and
// the idempotency key that survives retries.
type InitiateInput struct {
	// InstructionID is both this call's idempotency key AND becomes
	// PaymentInstruction.InstructionID — see domain.NewOutboundDisbursement's
	// doc comment. A retried Initiate call with the same InstructionID
	// MUST NOT result in a second outbound payment, even if the first
	// attempt's response was lost — every adapter in this package
	// enforces this itself (see internal/rails/sandbox and
	// internal/rails/ach's own dedup ledgers), as a second, independent
	// backstop behind internal/service's own idempotent-lookup-before-
	// initiate discipline.
	InstructionID  string
	LoanAccountID  string
	PartyID        string
	JournalEntryID string
	Amount         domain.Money
}

// Submission is a rail's immediate, synchronous acknowledgment of an
// Initiate or ReturnPayment call — NEVER the terminal outcome. Per this
// role's refusal clause, nothing in this package or internal/service
// ever treats RailReference/Accepted as grounds to mark a
// PaymentInstruction Executed; only a later Confirm (or a ReceiveInbound-
// surfaced return) does that.
type Submission struct {
	RailReference string
	SubmittedAt   time.Time
}

// OutcomeStatus is Confirm's answer, narrower than domain.Status —
// Pending is not a valid domain.Status (a PaymentInstruction stays
// Submitted while pending; Pending exists here only so Confirm can
// distinguish "still working on it" from "here is a terminal outcome").
type OutcomeStatus string

const (
	OutcomePending  OutcomeStatus = "PENDING"
	OutcomeExecuted OutcomeStatus = "EXECUTED"
	OutcomeFailed   OutcomeStatus = "FAILED"
	OutcomeReturned OutcomeStatus = "RETURNED"
)

// Outcome is what Confirm reports for a previously-Initiated submission.
type Outcome struct {
	Status        OutcomeStatus
	FailureReason *domain.FailureReason // set only when Status is Failed or Returned
	ConfirmedAt   time.Time
}

// InboundKind distinguishes the two things ReceiveInbound can surface —
// see this package's doc comment.
type InboundKind string

const (
	InboundReceived InboundKind = "RECEIVED"
	InboundReturned InboundKind = "RETURNED"
)

// InboundEvent is one item ReceiveInbound returns. For Kind=Received,
// RailReference is this specific payment's own reference (what a later
// ReturnPayment call, or a later Kind=Returned event for the SAME
// payment, refers back to via OriginalRailReference). For
// Kind=Returned, OriginalRailReference identifies which earlier
// Kind=Received event this return corresponds to — internal/service's
// reconciliation logic matches on this, and an OriginalRailReference
// with no matching prior Received event is logged as an exception, never
// processed speculatively (this role's ground rule).
type InboundEvent struct {
	Kind                  InboundKind
	RailReference         string
	OriginalRailReference string // only set when Kind == InboundReturned
	LoanAccountRef        *string
	Amount                domain.Money
	Rail                  string
	FailureReason         *domain.FailureReason // only set when Kind == InboundReturned
	OccurredAt            time.Time
}

// ReturnPaymentInput identifies which previously-received inbound
// payment this service is now originating a return for.
type ReturnPaymentInput struct {
	// IdempotencyKey: same survives-retries guarantee Initiate documents
	// — a retried ReturnPayment call for the same OriginalRailReference
	// must never send the money back twice.
	IdempotencyKey        string
	OriginalRailReference string
	Amount                domain.Money
	ReasonCode            string
}

// Sentinel errors every adapter maps its rail's own errors to, so
// internal/service can react via errors.Is without knowing which rail is
// behind the interface.
var (
	// ErrDuplicateInstruction: Initiate/ReturnPayment called twice with
	// the same idempotency key but a materially different payload — a
	// caller-side bug (internal/service's own idempotent-lookup-before-
	// initiate discipline should make this unreachable in practice; see
	// InitiateInput.InstructionID's doc comment for why this exists
	// anyway as a second backstop).
	ErrDuplicateInstruction = errors.New("railclient: instruction ID already used with a different payload")
	// ErrRailRejected: the rail refused the submission outright
	// (malformed destination, sanctions hold, insufficient rail-side
	// balance, etc.) — mapped to domain.ReasonRailRejected.
	ErrRailRejected = errors.New("railclient: rail rejected the submission")
	// ErrRailUnavailable: transport failure or unexpected rail response
	// — the rail's outcome for this specific call is UNKNOWN, same
	// "never assume nothing happened" discipline every other typed
	// client in this system (e.g. services/las/internal/glclient)
	// documents for its own unavailable-error sentinel.
	ErrRailUnavailable = errors.New("railclient: rail unavailable or returned an unexpected response")
	// ErrNotFound: Confirm called with a RailReference this adapter has
	// no record of.
	ErrNotFound = errors.New("railclient: no submission found for that rail reference")
)

// Client is the single abstraction every payment rail in this system is
// implemented behind — see this package's doc comment.
type Client interface {
	// Initiate submits an outbound disbursement. Idempotent on
	// in.InstructionID: a retried call with the same InstructionID and
	// an identical payload returns the ORIGINAL Submission, never
	// re-executes the transfer.
	Initiate(ctx context.Context, in InitiateInput) (Submission, error)

	// Confirm reports the current outcome of a previously-Initiated
	// submission, identified by the RailReference Initiate returned.
	// Returns ErrNotFound for an unknown reference.
	Confirm(ctx context.Context, railReference string) (Outcome, error)

	// ReceiveInbound drains inbound payments (and returns of previously-
	// received inbound payments) the rail has for this service, with
	// OccurredAt strictly after `since` — a cursor-based pull, matching
	// a batch/file-based rail's actual cadence rather than pretending a
	// real-time push exists where none does.
	ReceiveInbound(ctx context.Context, since time.Time) ([]InboundEvent, error)

	// ReturnPayment originates a return of a specific, previously-
	// received inbound payment back to its sender.
	ReturnPayment(ctx context.Context, in ReturnPaymentInput) (Submission, error)
}
