package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/store"
)

// ReconciliationSweepSummary reports what one sweep did — returned so a
// caller (cmd/payment-service's polling loop, or a test) can log/assert
// without this method itself needing a logger dependency.
type ReconciliationSweepSummary struct {
	Checked      int
	Confirmed    int
	Failed       int
	StillPending int
	Unmatched    int
}

// RunReconciliationSweep is the polling half of this role's
// "reconciliation logic matching confirmations to instructions" ground
// rule: for every OUTBOUND PaymentInstruction still Submitted, ask the
// rail for its current outcome via railclient.Client.Confirm and apply
// it. This is the correct integration pattern for a rail with no push
// mechanism at all (a pure-polling rail); a push-style rail (a real
// webhook) would instead call ProcessConfirmation directly per
// delivered webhook, reusing the exact same reconciliation logic below
// rather than duplicating it.
func (s *Service) RunReconciliationSweep(ctx context.Context) (ReconciliationSweepSummary, error) {
	var summary ReconciliationSweepSummary
	pending, err := s.store.ListSubmittedOutbound(ctx)
	if err != nil {
		return summary, err
	}
	summary.Checked = len(pending)

	for _, p := range pending {
		if p.RailReference == nil {
			continue // should be structurally impossible -- InitiateDisbursement always sets it before saving
		}
		outcome, err := s.rail.Confirm(ctx, *p.RailReference)
		if err != nil {
			if errors.Is(err, railclient.ErrNotFound) {
				summary.Unmatched++
				if exErr := s.fileUnmatchedConfirmation(ctx, *p.RailReference, p.Rail, "Confirm returned ErrNotFound for a rail reference this service itself recorded"); exErr != nil {
					return summary, exErr
				}
				continue
			}
			return summary, err // transport/unavailable -- abort the sweep, retry next interval
		}
		result, applyErr := s.applyOutcome(ctx, p, outcome)
		if applyErr != nil {
			return summary, applyErr
		}
		switch result {
		case appliedExecuted:
			summary.Confirmed++
		case appliedFailedOrReturned:
			summary.Failed++
		case appliedPending:
			summary.StillPending++
		}
	}
	return summary, nil
}

// ProcessConfirmation is the push-style (webhook) equivalent of one
// iteration of RunReconciliationSweep's loop body — a rail adapter with
// a real webhook would call this directly per delivered notification.
// railReference not matching any known PaymentInstruction is this role's
// literal "unmatched confirmation is logged as an exception, never
// posted speculatively" ground rule.
func (s *Service) ProcessConfirmation(ctx context.Context, railReference string, outcome railclient.Outcome) error {
	p, found, err := s.store.GetPaymentInstructionByRailReference(ctx, railReference)
	if err != nil {
		return err
	}
	if !found {
		return s.fileUnmatchedConfirmation(ctx, railReference, nil, "confirmation referenced an instructionId this service has no PaymentInstruction for")
	}
	_, err = s.applyOutcome(ctx, p, outcome)
	return err
}

type applyResult int

const (
	appliedPending applyResult = iota
	appliedExecuted
	appliedFailedOrReturned
	appliedNoOp
)

// applyOutcome maps a railclient.Outcome onto p's own status transition
// and, for a terminal outcome, publishes the matching event — the
// "compensating reversal journal entry through the normal posting path"
// ground rule, for the OUTBOUND/disbursement direction: this service's
// job stops at reliably publishing payment.disbursement.failed;
// AccountAPI (LAS) owns the disbursement state machine and its own
// already-built PR-DISB-02 reversal (ReverseDisbursement) is the actual
// compensating posting — see PR_DESCRIPTION.md for why this service
// does not call GLPostingAPI directly for this case (module ownership;
// see docs/design-notes.md's CB-DISB section).
func (s *Service) applyOutcome(ctx context.Context, p domain.PaymentInstruction, outcome railclient.Outcome) (applyResult, error) {
	now := s.now()
	var target domain.Status
	switch outcome.Status {
	case railclient.OutcomePending:
		return appliedPending, nil
	case railclient.OutcomeExecuted:
		target = domain.StatusExecuted
	case railclient.OutcomeFailed:
		target = domain.StatusFailed
	case railclient.OutcomeReturned:
		target = domain.StatusReturned
	default:
		return appliedNoOp, fmt.Errorf("service: rail reported an unrecognized outcome status %q", outcome.Status)
	}

	if p.Status == target {
		return appliedNoOp, nil // already applied -- a re-delivered confirmation, safe no-op
	}
	next, err := p.TransitionTo(target, outcome.FailureReason, now)
	var invalidErr *domain.ErrInvalidTransition
	if errors.As(err, &invalidErr) {
		// The rail reported a DIFFERENT terminal outcome than what this
		// service already recorded (e.g. already Failed, now reported
		// Executed) -- a genuine data-integrity anomaly, not a routine
		// unmatched case. Filed as an exception rather than silently
		// overwritten or silently dropped.
		detail := fmt.Sprintf("rail reported %s for instruction %s, which is already %s", outcome.Status, p.InstructionID, p.Status)
		if exErr := s.fileUnmatchedConfirmation(ctx, derefOrEmpty(p.RailReference), p.Rail, detail); exErr != nil {
			return appliedNoOp, exErr
		}
		return appliedNoOp, nil
	}
	if err != nil {
		return appliedNoOp, err
	}

	topic := "payment.disbursement.confirmed"
	result := appliedExecuted
	if target != domain.StatusExecuted {
		topic = "payment.disbursement.failed"
		result = appliedFailedOrReturned
	}
	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.SavePaymentInstruction(ctx, next); err != nil {
			return err
		}
		return publishEvent(ctx, tx, next.InstructionID+":"+string(target), topic, toPaymentInstructionPayload(next))
	})
	return result, err
}

func (s *Service) fileUnmatchedConfirmation(ctx context.Context, railReference string, rail *string, details string) error {
	railName := ""
	if rail != nil {
		railName = *rail
	}
	ex := domain.ReconciliationException{
		ExceptionID: s.newID(), Kind: domain.ExceptionUnmatchedConfirmation, RailReference: railReference,
		Rail: railName, Details: details, OccurredAt: s.now(), CreatedAt: s.now(),
	}
	return s.store.WithinTx(ctx, func(tx store.Tx) error { return tx.SaveReconciliationException(ctx, ex) })
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
