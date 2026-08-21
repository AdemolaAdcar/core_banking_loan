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

type ApplyModificationInput struct {
	LoanAccountID            string
	ModificationID           string // modificationReferenceId, = Idempotency-Key
	EffectiveDate            time.Time
	ConfirmedBy              string
	NewAnnualInterestRateBps *int
	NewTermMonths            *int
	Capitalization           *domain.Capitalization
}

type loanTermsModifiedPayload struct {
	LoanAccountID  string `json:"loanAccountId"`
	ModificationID string `json:"modificationId"`
	EffectiveDate  string `json:"effectiveDate"`
	ModifiedAt     string `json:"modifiedAt"`
}

// ApplyModification implements applyModification (REQ-CB-MOD-001..005).
// Branch A (capitalization present) posts exactly one PR-MOD-01 and
// grows the new TermVersion's principal by the capitalized total.
// Branch B (rate/term-only) makes ZERO GLPostingAPI calls — a true
// no-op, not a zero-amount posting (REQ-CB-MOD-004).
func (s *Service) ApplyModification(ctx context.Context, in ApplyModificationInput) (domain.Modification, error) {
	if existing, err := s.store.GetModification(ctx, in.ModificationID); err == nil {
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return domain.Modification{}, err
	}

	if in.NewAnnualInterestRateBps == nil && in.NewTermMonths == nil && in.Capitalization == nil {
		return domain.Modification{}, fmt.Errorf("%w: a modification must change at least one of rate, term, or capitalization", ErrMalformedTerms)
	}

	account, err := s.GetLoanAccount(ctx, in.LoanAccountID)
	if err != nil {
		return domain.Modification{}, err
	}
	if account.Status != domain.StatusDisbursed {
		return domain.Modification{}, fmt.Errorf("%w: loan account %s is not modifiable from status %s", ErrNotModifiable, in.LoanAccountID, account.Status)
	}

	current := account.CurrentTermVersion
	next := domain.TermVersion{TermSet: current.TermSet, Version: current.Version + 1, EffectiveDate: in.EffectiveDate}
	if in.NewAnnualInterestRateBps != nil {
		next.AnnualInterestRateBps = *in.NewAnnualInterestRateBps
	}
	if in.NewTermMonths != nil {
		next.TermMonths = *in.NewTermMonths
	}

	now := s.now()
	var journalEntryID *string
	var capitalizedAmount *domain.Money

	if in.Capitalization != nil {
		total, err := in.Capitalization.Total()
		if err != nil {
			return domain.Modification{}, err
		}
		grown, err := next.PrincipalAmount.Add(total)
		if err != nil {
			return domain.Modification{}, err
		}
		next.PrincipalAmount = grown

		entry, err := s.postToGL(ctx, glclient.PostJournalEntryInput{
			IdempotencyKey: in.ModificationID, PostingRuleCode: postingrules.PRMOD01, LoanAccountID: in.LoanAccountID, Capitalization: capitalizationToGL(*in.Capitalization),
		})
		if err != nil {
			return domain.Modification{}, err
		}
		journalEntryID = &entry.JournalEntryID
		capitalizedAmount = &total
	}

	nextAccount := account.WithTermVersion(next, now)
	m := domain.Modification{
		ModificationID: in.ModificationID, LoanAccountID: in.LoanAccountID, EffectiveDate: in.EffectiveDate,
		PreviousTermVersion: current.Version, NewTermVersion: next.Version, CapitalizedAmount: capitalizedAmount,
		JournalEntryID: journalEntryID, ConfirmedBy: in.ConfirmedBy, UpdatedAt: now,
	}

	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.SaveTermVersion(ctx, in.LoanAccountID, next); err != nil {
			return err
		}
		if err := tx.SaveLoanAccount(ctx, nextAccount); err != nil {
			return err
		}
		if err := tx.SaveModification(ctx, m); err != nil {
			return err
		}
		return publishEvent(ctx, tx, in.ModificationID+":modified", "loan.terms.modified", loanTermsModifiedPayload{
			LoanAccountID: in.LoanAccountID, ModificationID: in.ModificationID, EffectiveDate: in.EffectiveDate.Format("2006-01-02"), ModifiedAt: now.Format(time.RFC3339),
		})
	})
	if err != nil {
		return domain.Modification{}, err
	}
	if in.Capitalization != nil {
		if _, err := s.RefreshBalanceProjection(ctx, in.LoanAccountID); err != nil {
			return domain.Modification{}, err
		}
	}
	return m, nil
}

func (s *Service) GetModification(ctx context.Context, modificationID string) (domain.Modification, error) {
	m, err := s.store.GetModification(ctx, modificationID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Modification{}, ErrNotFound
	}
	return m, err
}
