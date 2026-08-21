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

type loanAccountDisbursedPayload struct {
	LoanAccountID   string       `json:"loanAccountId"`
	DisbursementID  string       `json:"disbursementId"`
	PrincipalAmount payloadMoney `json:"principalAmount"`
	JournalEntryID  string       `json:"journalEntryId"`
	InstructionID   string       `json:"instructionId"`
	DisbursedAt     string       `json:"disbursedAt"`
}

type payloadMoney struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

func toPayloadMoney(m domain.Money) payloadMoney {
	return payloadMoney{Amount: m.Amount, Currency: m.Currency}
}

// CreateDisbursement implements createDisbursement (REQ-CB-DISB-001):
// accepts a disbursement request against an Approved loan account,
// creating the resource in PendingDisbursement — moves no balance.
func (s *Service) CreateDisbursement(ctx context.Context, loanAccountID, disbursementID, requestedBy string) (domain.Disbursement, error) {
	if existing, err := s.store.GetDisbursement(ctx, disbursementID); err == nil {
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return domain.Disbursement{}, err
	}

	account, err := s.GetLoanAccount(ctx, loanAccountID)
	if err != nil {
		return domain.Disbursement{}, err
	}
	now := s.now()
	nextAccount, err := account.TransitionTo(domain.StatusPendingDisbursement, now)
	if err != nil {
		return domain.Disbursement{}, err
	}

	d := domain.Disbursement{
		DisbursementID: disbursementID, LoanAccountID: loanAccountID, Status: domain.DisbursementPendingDisbursement,
		PrincipalAmount: account.CurrentTermVersion.PrincipalAmount, RequestedBy: requestedBy, CreatedAt: now, UpdatedAt: now,
	}
	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.SaveLoanAccount(ctx, nextAccount); err != nil {
			return err
		}
		return tx.SaveDisbursement(ctx, d)
	})
	if err != nil {
		return domain.Disbursement{}, err
	}
	return d, nil
}

// ConfirmDisbursementFunding is the internal method a future PaymentAPI
// payment.disbursement.confirmed consumer invokes (see this package's
// doc comment) — the actual translation of "disburse" into exactly one
// PR-DISB-01 posting. LoanAccount only transitions PendingDisbursement
// -> Disbursed once GL confirms; a GL rejection or unavailability
// leaves both the Disbursement and LoanAccount exactly as they were,
// never a partial state.
func (s *Service) ConfirmDisbursementFunding(ctx context.Context, disbursementID, instructionID string) (domain.Disbursement, error) {
	d, err := s.store.GetDisbursement(ctx, disbursementID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return domain.Disbursement{}, ErrNotFound
		}
		return domain.Disbursement{}, err
	}
	if d.Status == domain.DisbursementDisbursed {
		return d, nil // idempotent replay
	}
	if d.Status != domain.DisbursementPendingDisbursement {
		return domain.Disbursement{}, fmt.Errorf("%w: disbursement %s is not PendingDisbursement", ErrNotModifiable, disbursementID)
	}

	account, err := s.GetLoanAccount(ctx, d.LoanAccountID)
	if err != nil {
		return domain.Disbursement{}, err
	}
	now := s.now()
	nextAccount, err := account.TransitionTo(domain.StatusDisbursed, now)
	if err != nil {
		return domain.Disbursement{}, err
	}

	glKey := "disb:" + disbursementID
	entry, err := s.postToGL(ctx, glclient.PostJournalEntryInput{
		IdempotencyKey: glKey, PostingRuleCode: postingrules.PRDISB01, LoanAccountID: d.LoanAccountID, Amount: &d.PrincipalAmount,
	})
	if err != nil {
		return domain.Disbursement{}, err
	}

	d.Status = domain.DisbursementDisbursed
	d.JournalEntryID = &entry.JournalEntryID
	d.InstructionID = &instructionID
	d.UpdatedAt = now

	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.SaveLoanAccount(ctx, nextAccount); err != nil {
			return err
		}
		if err := tx.SaveDisbursement(ctx, d); err != nil {
			return err
		}
		return publishEvent(ctx, tx, disbursementID+":disbursed", "loan.account.disbursed", loanAccountDisbursedPayload{
			LoanAccountID: d.LoanAccountID, DisbursementID: disbursementID, PrincipalAmount: toPayloadMoney(d.PrincipalAmount),
			JournalEntryID: entry.JournalEntryID, InstructionID: instructionID, DisbursedAt: now.Format(time.RFC3339),
		})
	})
	if err != nil {
		return domain.Disbursement{}, err
	}
	if _, err := s.RefreshBalanceProjection(ctx, d.LoanAccountID); err != nil {
		return domain.Disbursement{}, err
	}
	return d, nil
}

// ReverseDisbursement implements reverseDisbursement (REQ-CB-DISB-007).
// Only valid while the Disbursement is itself in "Disbursed" status; the
// resource moves through an intermediate ReversalPending status (saved
// before the GL call) so a crash between the DB write and GL's response
// leaves a visible, poll-able state rather than a silently stuck
// "Disbursed" resource. The LoanAccount only reverts Disbursed ->
// Approved once GL confirms PR-DISB-02.
func (s *Service) ReverseDisbursement(ctx context.Context, disbursementID, idempotencyKey, confirmedBy, reasonCode string) (domain.Disbursement, error) {
	d, err := s.store.GetDisbursement(ctx, disbursementID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return domain.Disbursement{}, ErrNotFound
		}
		return domain.Disbursement{}, err
	}
	if d.Status == domain.DisbursementReversed || d.Status == domain.DisbursementReversalPending {
		return d, nil // idempotent replay
	}
	if d.Status != domain.DisbursementDisbursed {
		return domain.Disbursement{}, fmt.Errorf("%w: disbursement %s is not reversible from status %s", ErrNotModifiable, disbursementID, d.Status)
	}

	account, err := s.GetLoanAccount(ctx, d.LoanAccountID)
	if err != nil {
		return domain.Disbursement{}, err
	}
	if !domain.CanTransition(account.Status, domain.StatusApproved) {
		return domain.Disbursement{}, &domain.ErrInvalidTransition{From: account.Status, To: domain.StatusApproved}
	}

	now := s.now()
	d.Status = domain.DisbursementReversalPending
	d.UpdatedAt = now
	if err := s.store.WithinTx(ctx, func(tx store.Tx) error { return tx.SaveDisbursement(ctx, d) }); err != nil {
		return domain.Disbursement{}, err
	}

	target := "disb:" + disbursementID
	entry, err := s.postToGL(ctx, glclient.PostJournalEntryInput{
		IdempotencyKey: idempotencyKey, PostingRuleCode: postingrules.PRDISB02, LoanAccountID: d.LoanAccountID,
		Amount: &d.PrincipalAmount, ReversalOfSourceEventID: &target,
	})
	if err != nil {
		return domain.Disbursement{}, err // d remains ReversalPending -- visible via GET, safe to retry
	}

	nextAccount, err := account.TransitionTo(domain.StatusApproved, now)
	if err != nil {
		return domain.Disbursement{}, err
	}
	d.Status = domain.DisbursementReversed
	d.JournalEntryID = &entry.JournalEntryID
	d.UpdatedAt = now

	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.SaveLoanAccount(ctx, nextAccount); err != nil {
			return err
		}
		return tx.SaveDisbursement(ctx, d)
	})
	if err != nil {
		return domain.Disbursement{}, err
	}
	if _, err := s.RefreshBalanceProjection(ctx, d.LoanAccountID); err != nil {
		return domain.Disbursement{}, err
	}
	return d, nil
}

func (s *Service) GetDisbursement(ctx context.Context, disbursementID string) (domain.Disbursement, error) {
	d, err := s.store.GetDisbursement(ctx, disbursementID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Disbursement{}, ErrNotFound
	}
	return d, err
}
