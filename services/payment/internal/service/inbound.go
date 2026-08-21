package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/accountclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/store"
)

// InboundSweepSummary reports what one ReceiveInboundPayments call did.
type InboundSweepSummary struct {
	Received                int
	ReceivedUnmatched       int // AccountAPI itself reported Unmatched (still a success, not an error)
	Returned                int
	ReturnedUnmatched       int // rail reported a return this service has no original receipt for
	ReturnedNoCompliantPath int // a Payoff return -- see ErrNoCompliantReversalPath
}

type paymentInboundReceivedPayload struct {
	PaymentReferenceID string       `json:"paymentReferenceId"`
	LoanAccountRef     *string      `json:"loanAccountRef"`
	Amount             moneyPayload `json:"amount"`
	Rail               string       `json:"rail"`
	ReceivedAt         string       `json:"receivedAt"`
}

// ReceiveInboundPayments drains railclient.Client.ReceiveInbound since
// this rail's last persisted cursor and, for every event, either (a)
// hands a newly-received credit to AccountAPI synchronously via
// receiveRepaymentNotification — the design note's unification pattern,
// see this package's doc comment — or (b) triggers the compensating
// reversal for a previously-received credit the rail now reports as
// returned. A failure partway through stops the batch WITHOUT advancing
// the cursor past the failed event, so the next call safely retries it
// (accountclient's own idempotent-replay-by-key makes a retry safe even
// if the earlier attempt actually landed downstream and only this
// service's own acknowledgment was lost).
func (s *Service) ReceiveInboundPayments(ctx context.Context, railName string) (InboundSweepSummary, error) {
	var summary InboundSweepSummary
	since, _, err := s.store.GetInboundCursor(ctx, railName)
	if err != nil {
		return summary, err
	}

	events, err := s.rail.ReceiveInbound(ctx, since)
	if err != nil {
		return summary, err
	}

	for _, e := range events {
		var applyErr error
		switch e.Kind {
		case railclient.InboundReceived:
			applyErr = s.applyInboundReceived(ctx, railName, e)
			if applyErr == nil {
				summary.Received++
			}
		case railclient.InboundReturned:
			var unmatched, noCompliantPath bool
			unmatched, noCompliantPath, applyErr = s.applyInboundReturn(ctx, railName, e)
			if applyErr == nil {
				summary.Returned++
				if unmatched {
					summary.ReturnedUnmatched++
				}
				if noCompliantPath {
					summary.ReturnedNoCompliantPath++
				}
			}
		default:
			applyErr = fmt.Errorf("service: unrecognized inbound event kind %q", e.Kind)
		}
		if applyErr != nil {
			return summary, applyErr // cursor not advanced past this event -- safe to retry
		}
	}
	return summary, nil
}

func (s *Service) applyInboundReceived(ctx context.Context, railName string, e railclient.InboundEvent) error {
	if _, err := s.store.GetPaymentInstruction(ctx, e.RailReference); err == nil {
		return s.advanceCursor(ctx, railName, e.OccurredAt) // already processed in a prior sweep before the cursor advanced
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	now := s.now()
	instruction := domain.NewInboundReceipt(e.RailReference, e.Rail, e.RailReference, e.Amount, now)

	result, err := s.account.ReceiveRepaymentNotification(ctx, accountclient.ReceiveRepaymentNotificationInput{
		IdempotencyKey: e.RailReference, LoanAccountRef: e.LoanAccountRef, Amount: e.Amount, Rail: e.Rail, ReceivedAt: e.OccurredAt,
	})
	if err != nil {
		return err // cursor not advanced -- retried next sweep, safe via AccountAPI's own idempotent replay
	}
	instruction.JournalEntryID = result.JournalEntryID
	if result.Kind == accountclient.KindPayoff {
		instruction.Purpose = domain.PurposePayoff
	}
	// result.Kind == KindRecovery has no corresponding domain.Purpose
	// value in the shipped payment-instruction.schema.json (its purpose
	// enum is DISBURSEMENT/REPAYMENT/PAYOFF only) -- left as REPAYMENT,
	// flagged as a spec gap in PR_DESCRIPTION.md rather than inventing
	// an enum value the shared schema doesn't define.

	return s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.SavePaymentInstruction(ctx, instruction); err != nil {
			return err
		}
		// payment.inbound.received publishes unconditionally, matched or
		// not -- observability/audit only, per the design note ("no
		// longer the authoritative trigger... still publishes for other
		// consumers and audit").
		payload := paymentInboundReceivedPayload{
			PaymentReferenceID: e.RailReference, LoanAccountRef: e.LoanAccountRef,
			Amount: moneyPayload{Amount: e.Amount.Amount, Currency: e.Amount.Currency}, Rail: e.Rail, ReceivedAt: e.OccurredAt.Format(time.RFC3339),
		}
		if err := publishEvent(ctx, tx, e.RailReference+":received", "payment.inbound.received", payload); err != nil {
			return err
		}
		return tx.SetInboundCursor(ctx, railName, e.OccurredAt)
	})
}

// applyInboundReturn is the rail-originated half of ground rule #4's
// "returned payment triggers a compensating reversal" — see
// ErrNoCompliantReversalPath's doc comment for the one case this method
// deliberately refuses to complete automatically.
func (s *Service) applyInboundReturn(ctx context.Context, railName string, e railclient.InboundEvent) (unmatched, noCompliantPath bool, err error) {
	original, found, err := s.store.GetPaymentInstructionByRailReference(ctx, e.OriginalRailReference)
	if err != nil {
		return false, false, err
	}
	if !found {
		ex := domain.ReconciliationException{
			ExceptionID: s.newID(), Kind: domain.ExceptionUnmatchedInboundReturn, RailReference: e.OriginalRailReference,
			Rail: e.Rail, Details: "rail reported a return for a payment this service never recorded a receipt for", OccurredAt: e.OccurredAt, CreatedAt: s.now(),
		}
		if err := s.store.WithinTx(ctx, func(tx store.Tx) error {
			if err := tx.SaveReconciliationException(ctx, ex); err != nil {
				return err
			}
			return tx.SetInboundCursor(ctx, railName, e.OccurredAt)
		}); err != nil {
			return false, false, err
		}
		return true, false, nil
	}

	now := s.now()
	next, transitionErr := original.TransitionTo(domain.StatusReturned, e.FailureReason, now)
	if transitionErr != nil {
		return false, false, transitionErr // already Returned or an otherwise invalid transition -- surfaced, not silently swallowed
	}

	if original.Purpose == domain.PurposePayoff {
		// See ErrNoCompliantReversalPath's doc comment: refuse the
		// automatic reversal call, record the return locally for
		// accurate bookkeeping, and file an exception for manual Ops
		// correction instead of a silent write-off.
		ex := domain.ReconciliationException{
			ExceptionID: s.newID(), Kind: domain.ExceptionNoCompliantReversalPath, RailReference: e.OriginalRailReference,
			Rail: e.Rail, Details: ErrNoCompliantReversalPath.Error(), OccurredAt: e.OccurredAt, CreatedAt: s.now(),
		}
		if err := s.store.WithinTx(ctx, func(tx store.Tx) error {
			if err := tx.SavePaymentInstruction(ctx, next); err != nil {
				return err
			}
			if err := tx.SaveReconciliationException(ctx, ex); err != nil {
				return err
			}
			return tx.SetInboundCursor(ctx, railName, e.OccurredAt)
		}); err != nil {
			return false, false, err
		}
		return false, true, nil
	}

	reverseResult, err := s.account.ReverseRepayment(ctx, accountclient.ReverseRepaymentInput{
		RepaymentID: original.InstructionID, IdempotencyKey: e.RailReference + ":reverse",
		ConfirmedBy: "payment-execution-service", ReasonCode: "PAYMENT_RETURNED",
	})
	if err != nil {
		return false, false, err // cursor not advanced -- retried next sweep
	}
	next.JournalEntryID = reverseResult.JournalEntryID

	if err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.SavePaymentInstruction(ctx, next); err != nil {
			return err
		}
		return tx.SetInboundCursor(ctx, railName, e.OccurredAt)
	}); err != nil {
		return false, false, err
	}
	return false, false, nil
}

func (s *Service) advanceCursor(ctx context.Context, railName string, at time.Time) error {
	return s.store.WithinTx(ctx, func(tx store.Tx) error { return tx.SetInboundCursor(ctx, railName, at) })
}
