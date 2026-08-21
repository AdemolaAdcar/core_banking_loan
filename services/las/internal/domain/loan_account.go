package domain

import (
	"fmt"
	"time"
)

// Status is LoanAccount's coarse lifecycle stage, matching
// specs/schemas/loan-account.schema.json#/$defs/LoanAccount.status
// exactly. Deliberately NOT the generic Pending/Active/Delinquent/
// WrittenOff/Closed template a from-scratch state machine might invent:
// this is the enum actually reviewed and shipped in the approved
// v0.3.0 spec, and delinquency is a separate overlay (DPD/DPDBucket/
// NonAccrualFlag below) that never gates or replaces this field — an
// account stays "Disbursed" while past due.
type Status string

const (
	StatusApproved            Status = "Approved"
	StatusPendingDisbursement Status = "PendingDisbursement"
	StatusDisbursed           Status = "Disbursed"
	StatusClosed              Status = "Closed"
	StatusChargedOff          Status = "ChargedOff"
)

// DPDBucket matches the shared enum used by LoanAccount.dpdBucket,
// PastDueAccount.dpdBucket, and DelinquencyStatusChangedPayload.
type DPDBucket string

const (
	DPDCurrent DPDBucket = "Current"
	DPD1to29   DPDBucket = "1-29"
	DPD30to59  DPDBucket = "30-59"
	DPD60to89  DPDBucket = "60-89"
	DPD90Plus  DPDBucket = "90+"
)

// validTransitions is the ONLY table this package consults to decide
// whether a status change is allowed. It is deliberately not the
// literal Pending/Active/Delinquent/WrittenOff/Closed template — see
// this package's doc comment — but it is exactly as strict: any pair
// not listed here is rejected with ErrInvalidTransition (mapped to 409
// by internal/api), never silently allowed.
//
//	Approved -> PendingDisbursement   (createDisbursement accepted)
//	PendingDisbursement -> Disbursed  (PR-DISB-01 confirmed posted)
//	Disbursed -> Approved             (reverseDisbursement confirmed —
//	                                   REQ-CB-DISB-007; only valid while
//	                                   the Disbursement resource itself
//	                                   is in "Disbursed" status per
//	                                   loan-account-subledger.yaml, so
//	                                   the LoanAccount side of this
//	                                   transition starts from Disbursed,
//	                                   not PendingDisbursement)
//	Disbursed -> Closed               (payoff settlement, PR-PAYOFF-01
//	                                   confirmed — REQ-CB-PAYOFF-006)
//	Disbursed -> ChargedOff           (charge-off confirmed,
//	                                   PR-CHGOFF-01 posted — REQ-CB-
//	                                   CHARGEOFF-003)
//
// Closed and ChargedOff are both terminal: no row below lists either as
// a "from" state. There is no cure/reinstatement transition out of
// ChargedOff anywhere in the approved spec or docs/design-notes.md (the
// design note explicitly states a written-off asset is permanently
// excluded from accrual/delinquency, with no reversal flow specified) —
// confirmed with the requester before implementation rather than
// invented here.
var validTransitions = map[Status]map[Status]bool{
	StatusApproved:            {StatusPendingDisbursement: true},
	StatusPendingDisbursement: {StatusDisbursed: true},
	StatusDisbursed:           {StatusApproved: true, StatusClosed: true, StatusChargedOff: true},
	StatusClosed:              {},
	StatusChargedOff:          {},
}

// ErrInvalidTransition is returned by LoanAccount.TransitionTo for any
// (from, to) pair not present in validTransitions — mapped to HTTP 409
// by internal/api, never silently allowed and never a 500.
type ErrInvalidTransition struct {
	From, To Status
}

func (e *ErrInvalidTransition) Error() string {
	return fmt.Sprintf("domain: invalid loan account transition %s -> %s", e.From, e.To)
}

// CanTransition reports whether from -> to is a legal LoanAccount
// status change, without mutating anything — used by callers that need
// to check eligibility before attempting a write (e.g. applyModification
// checking the account isn't Closed/ChargedOff).
func CanTransition(from, to Status) bool {
	next, ok := validTransitions[from]
	return ok && next[to]
}

// TermSet is the negotiated loan terms, immutable once part of a
// TermVersion — mirrors loan-account.schema.json#/$defs/TermSet.
type TermSet struct {
	PrincipalAmount       Money
	AnnualInterestRateBps int
	TermMonths            int
	DayCountConvention    string
}

// TermVersion is one immutable, effective-dated snapshot of a loan's
// terms — booking creates v1; every modification appends a new version,
// never edits a prior one (REQ-CB-MOD-002). Mirrors
// loan-account.schema.json#/$defs/TermVersion.
type TermVersion struct {
	TermSet
	Version       int
	EffectiveDate time.Time
}

// LoanAccount mirrors loan-account.schema.json#/$defs/LoanAccount
// field-for-field. Deliberately has NO balance field of any kind — a
// loan account's balance is never a stored, independently-mutable
// property of this struct. It is always a separate, read-optimized
// BalanceProjection (see projection.go) rebuilt from GL's posted
// entries. If a future change to this struct ever adds a balance-shaped
// field, that change is wrong by this package's own ground rules.
type LoanAccount struct {
	LoanAccountID       string
	ApprovalReferenceID string
	PartyID             string
	Status              Status
	CurrentTermVersion  TermVersion
	BookedBy            string
	DPD                 int
	DPDBucket           DPDBucket
	NonAccrualFlag      bool
	NonAccrualSince     *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// NewLoanAccount books a loan account in Approved status with term
// version 1 — the only way to construct a LoanAccount. No balance is
// posted or referenced at any point in this constructor, matching
// REQ-CB-BOOK-*'s "zero GLPostingAPI calls" rule for booking.
func NewLoanAccount(loanAccountID, approvalReferenceID, partyID, bookedBy string, terms TermSet, bookedAt time.Time) LoanAccount {
	return LoanAccount{
		LoanAccountID:       loanAccountID,
		ApprovalReferenceID: approvalReferenceID,
		PartyID:             partyID,
		Status:              StatusApproved,
		CurrentTermVersion:  TermVersion{TermSet: terms, Version: 1, EffectiveDate: bookedAt},
		BookedBy:            bookedBy,
		DPD:                 0,
		DPDBucket:           DPDCurrent,
		NonAccrualFlag:      false,
		CreatedAt:           bookedAt,
		UpdatedAt:           bookedAt,
	}
}

// TransitionTo returns a new LoanAccount value with Status advanced to
// `to` and UpdatedAt set to `at`, or ErrInvalidTransition if the move
// isn't in validTransitions. The receiver is never mutated in place —
// callers persist the returned value inside the same transaction that
// writes the account.status.changed-equivalent outbox event (see
// internal/service), so the state write and the event publish commit
// or roll back together.
func (a LoanAccount) TransitionTo(to Status, at time.Time) (LoanAccount, error) {
	if !CanTransition(a.Status, to) {
		return LoanAccount{}, &ErrInvalidTransition{From: a.Status, To: to}
	}
	next := a
	next.Status = to
	next.UpdatedAt = at
	return next, nil
}

// IsTerminal reports whether the account is in Closed or ChargedOff
// status — used to reject a posting request against it (other than a
// recovery, which is checked separately since it targets ChargedOff
// specifically and deliberately).
func (a LoanAccount) IsTerminal() bool {
	return a.Status == StatusClosed || a.Status == StatusChargedOff
}

// WithDelinquency returns a copy with the DPD overlay updated —
// deliberately a separate method from TransitionTo, since dpd/dpdBucket
// changes never go through the Status transition table at all (see this
// package's doc comment: delinquency never gates or replaces Status).
func (a LoanAccount) WithDelinquency(dpd int, bucket DPDBucket, nonAccrualFlag bool, nonAccrualSince *time.Time, at time.Time) LoanAccount {
	next := a
	next.DPD = dpd
	next.DPDBucket = bucket
	next.NonAccrualFlag = nonAccrualFlag
	next.NonAccrualSince = nonAccrualSince
	next.UpdatedAt = at
	return next
}

// WithTermVersion returns a copy with a new current term version
// appended (CB-MOD) — the prior version is never edited, only ever
// superseded as CurrentTermVersion; callers persist the full version
// history separately (internal/store keeps every TermVersion, not just
// the current one).
func (a LoanAccount) WithTermVersion(v TermVersion, at time.Time) LoanAccount {
	next := a
	next.CurrentTermVersion = v
	next.UpdatedAt = at
	return next
}
