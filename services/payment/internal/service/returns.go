package service

import (
	"context"
	"errors"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/accountclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/store"
)

// ReturnInboundPaymentInput identifies an Ops/compliance-initiated
// return of a specific, previously-received inbound payment — see
// railclient.Client's doc comment for how this differs from
// applyInboundReturn (inbound.go), which handles the RAIL originating a
// return on money already received, not this service originating one.
type ReturnInboundPaymentInput struct {
	InstructionID  string // the original INBOUND PaymentInstruction's ID
	IdempotencyKey string
	ReasonCode     string
}

// ReturnInboundPayment originates a return of a previously-received
// inbound payment. Ordering is deliberate: the AccountAPI-side
// compensating reversal (immediate ledger correctness) happens BEFORE
// the rail-side money movement (which, for a batch rail like ACH, may
// not settle for another one to two banking days regardless) — so this
// service's own books are correct even if the rail submission is
// delayed or needs to be retried independently.
func (s *Service) ReturnInboundPayment(ctx context.Context, in ReturnInboundPaymentInput) (railclient.Submission, error) {
	original, err := s.store.GetPaymentInstruction(ctx, in.InstructionID)
	if errors.Is(err, store.ErrNotFound) {
		return railclient.Submission{}, ErrNotFound
	}
	if err != nil {
		return railclient.Submission{}, err
	}

	if original.Purpose == domain.PurposePayoff {
		return railclient.Submission{}, ErrNoCompliantReversalPath
	}

	reverseResult, err := s.account.ReverseRepayment(ctx, accountclient.ReverseRepaymentInput{
		RepaymentID: original.InstructionID, IdempotencyKey: in.IdempotencyKey + ":reverse",
		ConfirmedBy: "payment-execution-service", ReasonCode: "PAYMENT_RETURNED",
	})
	if err != nil {
		return railclient.Submission{}, err
	}

	now := s.now()
	next, err := original.TransitionTo(domain.StatusReturned, nil, now)
	if err != nil {
		return railclient.Submission{}, err
	}
	next.JournalEntryID = reverseResult.JournalEntryID

	railRef := ""
	if original.RailReference != nil {
		railRef = *original.RailReference
	}
	sub, err := s.rail.ReturnPayment(ctx, railclient.ReturnPaymentInput{
		IdempotencyKey: in.IdempotencyKey, OriginalRailReference: railRef, Amount: original.Amount, ReasonCode: in.ReasonCode,
	})
	if err != nil {
		// The GL-side correction above already committed and is durably
		// correct regardless of this failure -- only the physical rail
		// submission needs retrying, which a caller can safely do again
		// with the same IdempotencyKey once the rail is available.
		return railclient.Submission{}, err
	}

	if err := s.store.WithinTx(ctx, func(tx store.Tx) error { return tx.SavePaymentInstruction(ctx, next) }); err != nil {
		return railclient.Submission{}, err
	}
	return sub, nil
}
