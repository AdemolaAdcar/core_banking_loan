package domain

import (
	"testing"
	"time"
)

func TestPeriod_Close_FirstCall_ChangesState(t *testing.T) {
	p := Period{ID: "2026-08", Status: PeriodOpen}
	updated, changed := p.Close("ops.analyst", time.Now())
	if !changed {
		t.Fatalf("expected changed=true on first close")
	}
	if updated.Status != PeriodClosed {
		t.Fatalf("expected Closed, got %s", updated.Status)
	}
	if updated.ClosedBy == nil || *updated.ClosedBy != "ops.analyst" {
		t.Fatalf("expected closedBy recorded")
	}
}

func TestPeriod_Close_AlreadyClosed_IsIdempotentNoOp(t *testing.T) {
	now := time.Now()
	p := Period{ID: "2026-08", Status: PeriodOpen}
	p, _ = p.Close("ops.analyst", now)

	again, changed := p.Close("someone.else", now.Add(time.Hour))
	if changed {
		t.Fatalf("expected changed=false on a repeat close")
	}
	if again.ClosedBy == nil || *again.ClosedBy != "ops.analyst" {
		t.Fatalf("expected original closedBy preserved, not overwritten by the repeat call")
	}
}
