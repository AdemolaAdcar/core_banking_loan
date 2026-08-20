package domain

import "time"

type PeriodStatus string

const (
	PeriodOpen   PeriodStatus = "Open"
	PeriodClosed PeriodStatus = "Closed"
)

// Period is invariant 7's distinct, auditable close record. Closing is
// one-way: there is no Reopen method anywhere in this package, matching
// the same "no update/delete" discipline JournalEntry follows.
type Period struct {
	ID       string
	Status   PeriodStatus
	ClosedAt *time.Time
	ClosedBy *string
}

// ErrEarlierPeriodOpen is returned when closing periodId is attempted
// while an earlier period is still Open -- periods must close in
// chronological order.
type ErrEarlierPeriodOpen struct {
	RequestedPeriodID, EarliestOpenPeriodID string
}

func (e *ErrEarlierPeriodOpen) Error() string {
	return "domain: cannot close " + e.RequestedPeriodID + ": earlier period " + e.EarliestOpenPeriodID + " is still Open"
}

// Close transitions Open -> Closed. Idempotent: closing an
// already-closed period is a no-op success (changed=false), matching
// the same idempotent-close discipline services/crm's ServiceCase.Close
// established -- a retried close call must not fail just because it
// already succeeded once. Chronological-order enforcement
// (ErrEarlierPeriodOpen) happens one level up, in the service layer,
// since it requires looking at every OTHER period, not just this one.
func (p Period) Close(closedBy string, now time.Time) (result Period, changed bool) {
	if p.Status == PeriodClosed {
		return p, false
	}
	p.Status = PeriodClosed
	p.ClosedAt = &now
	p.ClosedBy = &closedBy
	return p, true
}
