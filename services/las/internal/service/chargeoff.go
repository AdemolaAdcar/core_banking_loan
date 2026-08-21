package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/glclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/postingrules"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/store"
)

type loanAccountChargedOffPayload struct {
	LoanAccountID  string `json:"loanAccountId"`
	ChargeoffID    string `json:"chargeoffId"`
	JournalEntryID string `json:"journalEntryId"`
	ChargedOffAt   string `json:"chargedOffAt"`
}

// RecordChargeOff implements recordChargeOff (REQ-CB-CHARGEOFF-001..004).
// The resource is created PendingChargeOff, posts PR-CHGOFF-01, and only
// transitions LoanAccount Disbursed -> ChargedOff once GL confirms —
// matching the same confirmed-posting-gates-status-change pattern used
// throughout this service.
func (s *Service) RecordChargeOff(ctx context.Context, loanAccountID, chargeoffDecisionReference, confirmedBy string) (domain.ChargeOff, error) {
	if existing, err := s.store.GetChargeOff(ctx, chargeoffDecisionReference); err == nil {
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return domain.ChargeOff{}, err
	}

	account, err := s.GetLoanAccount(ctx, loanAccountID)
	if err != nil {
		return domain.ChargeOff{}, err
	}
	if !domain.CanTransition(account.Status, domain.StatusChargedOff) {
		return domain.ChargeOff{}, &domain.ErrInvalidTransition{From: account.Status, To: domain.StatusChargedOff}
	}

	projection, err := s.GetBalanceProjection(ctx, loanAccountID)
	if err != nil {
		return domain.ChargeOff{}, err
	}
	writeOffAmount, err := projection.TotalOutstanding()
	if err != nil {
		return domain.ChargeOff{}, err
	}

	now := s.now()
	c := domain.ChargeOff{
		ChargeoffID: chargeoffDecisionReference, LoanAccountID: loanAccountID, Status: domain.ChargeOffPending,
		WriteOffAmount: writeOffAmount, ChargeoffDecisionReference: chargeoffDecisionReference, ConfirmedBy: confirmedBy, UpdatedAt: now,
	}
	if err := s.store.WithinTx(ctx, func(tx store.Tx) error { return tx.SaveChargeOff(ctx, c) }); err != nil {
		return domain.ChargeOff{}, err
	}

	alloc := &glclient.Allocation{FeeAmount: projection.FeeReceivable, InterestAmount: projection.InterestReceivable, PrincipalAmount: projection.OutstandingPrincipal}
	entry, err := s.postToGL(ctx, glclient.PostJournalEntryInput{
		IdempotencyKey: chargeoffDecisionReference, PostingRuleCode: postingrules.PRCHGOFF01, LoanAccountID: loanAccountID, Allocation: alloc,
		Metadata: map[string]any{"chargeoffDecisionReference": chargeoffDecisionReference, "confirmedBy": confirmedBy},
	})
	if err != nil {
		return domain.ChargeOff{}, err // c remains PendingChargeOff -- visible via GET
	}

	nextAccount, err := account.TransitionTo(domain.StatusChargedOff, now)
	if err != nil {
		return domain.ChargeOff{}, err
	}
	c.Status = domain.ChargeOffDone
	c.JournalEntryID = &entry.JournalEntryID
	c.ChargedOffAt = &now
	c.UpdatedAt = now

	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.SaveLoanAccount(ctx, nextAccount); err != nil {
			return err
		}
		if err := tx.SaveChargeOff(ctx, c); err != nil {
			return err
		}
		return publishEvent(ctx, tx, chargeoffDecisionReference+":chargedoff", "loan.account.chargedoff", loanAccountChargedOffPayload{
			LoanAccountID: loanAccountID, ChargeoffID: chargeoffDecisionReference, JournalEntryID: entry.JournalEntryID, ChargedOffAt: now.Format(time.RFC3339),
		})
	})
	if err != nil {
		return domain.ChargeOff{}, err
	}
	if _, err := s.RefreshBalanceProjection(ctx, loanAccountID); err != nil {
		return domain.ChargeOff{}, err
	}
	return c, nil
}

func (s *Service) GetChargeOff(ctx context.Context, chargeoffID string) (domain.ChargeOff, error) {
	c, err := s.store.GetChargeOff(ctx, chargeoffID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.ChargeOff{}, fmt.Errorf("%w", ErrNotFound)
	}
	return c, err
}
