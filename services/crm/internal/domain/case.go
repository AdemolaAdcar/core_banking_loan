// Package domain holds the CRM service's core business types and logic,
// independent of transport (HTTP) and persistence (Postgres). Nothing
// here ever imports a GL/posting type, references a Money value, or
// calls out to the Posting Engine — CRM has no balance-affecting
// operation, ever (see the package-level ground rule this whole service
// is built under).
package domain

import "time"

type CaseStatus string

const (
	CaseStatusOpen       CaseStatus = "Open"
	CaseStatusInProgress CaseStatus = "InProgress"
	CaseStatusResolved   CaseStatus = "Resolved"
	CaseStatusClosed     CaseStatus = "Closed"
)

type ReasonCode string

const (
	ReasonGeneralInquiry      ReasonCode = "GENERAL_INQUIRY"
	ReasonPaymentDispute      ReasonCode = "PAYMENT_DISPUTE"
	ReasonHardshipRequest     ReasonCode = "HARDSHIP_REQUEST"
	ReasonDelinquencyFollowup ReasonCode = "DELINQUENCY_FOLLOWUP"
	ReasonOther               ReasonCode = "OTHER"
)

// SLADuration is the fixed, deterministic per-reasonCode SLA window a
// case's slaDueAt is computed from at open (and recomputed at reopen).
// Not configurable at runtime in this increment — a lookup table, same
// deterministic-not-ML-or-runtime-configurable discipline the dedup
// engine in the Party/CIF service uses for its confidence scores.
var SLADuration = map[ReasonCode]time.Duration{
	ReasonPaymentDispute:      24 * time.Hour,
	ReasonHardshipRequest:     48 * time.Hour,
	ReasonDelinquencyFollowup: 48 * time.Hour,
	ReasonGeneralInquiry:      72 * time.Hour,
	ReasonOther:               72 * time.Hour,
}

// slaDurationFor defaults to the most conservative (shortest) window for
// any reasonCode not in the table -- a missing entry must never silently
// mean "no SLA," which is the opposite of conservative.
func slaDurationFor(r ReasonCode) time.Duration {
	if d, ok := SLADuration[r]; ok {
		return d
	}
	return 24 * time.Hour
}

// ServiceCase is the domain representation of a case. loanAccountId and
// partyId are opaque references into other services (Loan Account
// Subledger, Party/CIF) — never embedded balance/identity data that
// could drift from the source of truth the moment it's cached.
type ServiceCase struct {
	ID            string
	PartyID       string
	LoanAccountID *string
	Status        CaseStatus
	ReasonCode    ReasonCode
	AssignedTo    *string
	Version       int
	SLADueAt      time.Time
	Escalated     bool
	CloseReason   *string
	ReopenReason  *string
	OpenedAt      time.Time
	UpdatedAt     time.Time
}

// NewCase constructs a freshly opened case: Status=Open, Version=1,
// SLADueAt computed from openedAt via SLADuration, Escalated=false.
func NewCase(id, partyID string, loanAccountID, assignedTo *string, reasonCode ReasonCode, openedAt time.Time) ServiceCase {
	return ServiceCase{
		ID:            id,
		PartyID:       partyID,
		LoanAccountID: loanAccountID,
		Status:        CaseStatusOpen,
		ReasonCode:    reasonCode,
		AssignedTo:    assignedTo,
		Version:       1,
		SLADueAt:      openedAt.Add(slaDurationFor(reasonCode)),
		Escalated:     false,
		OpenedAt:      openedAt,
		UpdatedAt:     openedAt,
	}
}

// ErrInvalidTransition is returned by any state-machine method attempted
// from a status that does not permit it.
type ErrInvalidTransition struct {
	From, To CaseStatus
	Op       string
}

func (e *ErrInvalidTransition) Error() string {
	return "domain: cannot " + e.Op + " a case from status " + string(e.From) + " to " + string(e.To)
}

// ErrStaleVersion is returned when a caller's expectedVersion no longer
// matches the case's current version -- someone else updated it first.
type ErrStaleVersion struct {
	Expected, Actual int
}

func (e *ErrStaleVersion) Error() string {
	return "domain: stale version (expected concurrent update conflict)"
}

// updatableTransitions lists every status transition Update permits.
// Never includes anything to or from Closed -- Close/Reopen own that
// boundary exclusively, for clearer audit/event semantics (a case
// closing or reopening is always its own distinct domain event, never
// folded into a generic "case updated").
var updatableTransitions = map[CaseStatus]map[CaseStatus]bool{
	CaseStatusOpen:       {CaseStatusInProgress: true, CaseStatusResolved: true},
	CaseStatusInProgress: {CaseStatusResolved: true},
}

// Update applies an optimistic-concurrency-checked status/assignee
// change. newStatus == nil means "no status change requested" (an
// assignedTo-only update). Returns ErrStaleVersion if expectedVersion
// doesn't match c.Version, or ErrInvalidTransition if the requested
// status change isn't permitted from the case's current status.
//
// changedFields names exactly which fields actually changed ("status",
// "assignedTo") -- the same field-NAMES-only convention
// CaseUpdatedPayload and Party/CIF's PartyUpdatedPayload use for their
// event payloads. If neither field actually changes the case's current
// state (e.g. a caller resubmits the same values already on file),
// changedFields is empty, and Version/UpdatedAt are left untouched --
// matching Party/CIF's updateParty no-op discipline: a call that changes
// nothing writes nothing and publishes nothing.
func (c ServiceCase) Update(expectedVersion int, newStatus *CaseStatus, newAssignedTo *string, now time.Time) (result ServiceCase, changedFields []string, err error) {
	if expectedVersion != c.Version {
		return ServiceCase{}, nil, &ErrStaleVersion{Expected: expectedVersion, Actual: c.Version}
	}
	if c.Status == CaseStatusClosed {
		return ServiceCase{}, nil, &ErrInvalidTransition{From: c.Status, To: c.Status, Op: "update"}
	}

	if newStatus != nil && *newStatus != c.Status {
		allowed := updatableTransitions[c.Status]
		if !allowed[*newStatus] {
			return ServiceCase{}, nil, &ErrInvalidTransition{From: c.Status, To: *newStatus, Op: "update"}
		}
		c.Status = *newStatus
		changedFields = append(changedFields, "status")
	}
	if newAssignedTo != nil && (c.AssignedTo == nil || *newAssignedTo != *c.AssignedTo) {
		c.AssignedTo = newAssignedTo
		changedFields = append(changedFields, "assignedTo")
	}
	if len(changedFields) == 0 {
		return c, nil, nil
	}
	c.Version++
	c.UpdatedAt = now
	return c, changedFields, nil
}

// Close transitions Open/InProgress/Resolved -> Closed. Idempotent: if
// the case is already Closed, returns it unchanged with changed=false so
// the caller knows not to publish a duplicate crm.case.closed event.
func (c ServiceCase) Close(reason string, now time.Time) (result ServiceCase, changed bool, err error) {
	if c.Status == CaseStatusClosed {
		return c, false, nil
	}
	c.Status = CaseStatusClosed
	c.CloseReason = &reason
	c.Version++
	c.UpdatedAt = now
	return c, true, nil
}

// Reopen transitions Closed -> Open ONLY. Unlike Close, this is NOT
// idempotent-as-no-op on a non-Closed case -- reopening a case that was
// never closed is a genuine state error (ErrInvalidTransition), not a
// retry of a prior success, since there is no prior "reopen" to have
// already happened. Resets Escalated to false and recomputes SLADueAt
// from now -- a reopened case gets a fresh SLA clock.
func (c ServiceCase) Reopen(reason string, now time.Time) (ServiceCase, error) {
	if c.Status != CaseStatusClosed {
		return ServiceCase{}, &ErrInvalidTransition{From: c.Status, To: CaseStatusOpen, Op: "reopen"}
	}
	c.Status = CaseStatusOpen
	c.ReopenReason = &reason
	c.Escalated = false
	c.SLADueAt = now.Add(slaDurationFor(c.ReasonCode))
	c.Version++
	c.UpdatedAt = now
	return c, nil
}

// IsPastSLA reports whether an active (Open/InProgress) case has passed
// its SLA deadline as of now, and has not already been marked escalated.
// Resolved/Closed cases are never escalated -- the SLA clock only
// matters while a case is still actively being worked.
func (c ServiceCase) IsPastSLA(now time.Time) bool {
	if c.Escalated {
		return false
	}
	if c.Status != CaseStatusOpen && c.Status != CaseStatusInProgress {
		return false
	}
	return now.After(c.SLADueAt)
}
