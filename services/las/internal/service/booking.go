package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/store"
)

type BookLoanAccountInput struct {
	ApprovalReferenceID string // must equal the Idempotency-Key header (REQ-CB-BOOK-004)
	PartyID             string
	BookedBy            string
	Terms               domain.TermSet
}

type loanAccountBookedPayload struct {
	LoanAccountID       string `json:"loanAccountId"`
	ApprovalReferenceID string `json:"approvalReferenceId"`
	PartyID             string `json:"partyId"`
	BookedAt            string `json:"bookedAt"`
}

// BookLoanAccount implements bookLoanAccount (REQ-CB-BOOK-001, 003, 004,
// 006): creates a loan account in Approved status with immutable term
// version v1. Zero GLPostingAPI calls, by design — booking moves no
// balance, so there is nothing to translate into a posting.
//
// PartyAPI validation (REQ-CB-BOOK-002: reject Blocked/Closed/KYC-not-
// Verified parties) is a deferred integration — see this package's doc
// comment; this method accepts any non-empty partyId.
func (s *Service) BookLoanAccount(ctx context.Context, in BookLoanAccountInput) (domain.LoanAccount, error) {
	if existing, found, err := s.store.FindLoanAccountByApprovalReference(ctx, in.ApprovalReferenceID); err != nil {
		return domain.LoanAccount{}, err
	} else if found {
		return existing, nil
	}

	if err := validateTerms(in.Terms); err != nil {
		return domain.LoanAccount{}, err
	}
	if in.PartyID == "" || in.BookedBy == "" || in.ApprovalReferenceID == "" {
		return domain.LoanAccount{}, fmt.Errorf("%w: partyId, bookedBy, and approvalReferenceId are required", ErrMalformedTerms)
	}

	now := s.now()
	account := domain.NewLoanAccount(s.newID(), in.ApprovalReferenceID, in.PartyID, in.BookedBy, in.Terms, now)

	err := s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.SaveLoanAccount(ctx, account); err != nil {
			return err
		}
		if err := tx.SaveTermVersion(ctx, account.LoanAccountID, account.CurrentTermVersion); err != nil {
			return err
		}
		return publishEvent(ctx, tx, account.LoanAccountID+":booked", "loan.account.booked", loanAccountBookedPayload{
			LoanAccountID: account.LoanAccountID, ApprovalReferenceID: account.ApprovalReferenceID, PartyID: account.PartyID, BookedAt: now.Format(time.RFC3339),
		})
	})
	if err != nil {
		return domain.LoanAccount{}, err
	}
	return account, nil
}

func validateTerms(t domain.TermSet) error {
	if t.PrincipalAmount.Amount <= 0 || t.PrincipalAmount.Currency == "" {
		return fmt.Errorf("%w: principalAmount must be positive", ErrMalformedTerms)
	}
	if t.AnnualInterestRateBps <= 0 {
		return fmt.Errorf("%w: annualInterestRateBps must be positive", ErrMalformedTerms)
	}
	if t.TermMonths <= 0 {
		return fmt.Errorf("%w: termMonths must be positive", ErrMalformedTerms)
	}
	switch t.DayCountConvention {
	case "ACTUAL_365", "ACTUAL_360", "THIRTY_360":
	default:
		return fmt.Errorf("%w: dayCountConvention %q is not a recognized convention", ErrMalformedTerms, t.DayCountConvention)
	}
	return nil
}

func (s *Service) GetLoanAccount(ctx context.Context, loanAccountID string) (domain.LoanAccount, error) {
	a, err := s.store.GetLoanAccount(ctx, loanAccountID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.LoanAccount{}, ErrLoanAccountNotFound
	}
	return a, err
}

func (s *Service) ListTermVersions(ctx context.Context, loanAccountID string) ([]domain.TermVersion, error) {
	if _, err := s.GetLoanAccount(ctx, loanAccountID); err != nil {
		return nil, err
	}
	return s.store.ListTermVersions(ctx, loanAccountID)
}

func (s *Service) GetEffectiveTermVersion(ctx context.Context, loanAccountID string, asOf time.Time) (domain.TermVersion, error) {
	if _, err := s.GetLoanAccount(ctx, loanAccountID); err != nil {
		return domain.TermVersion{}, err
	}
	v, found, err := s.store.GetEffectiveTermVersion(ctx, loanAccountID, asOf)
	if err != nil {
		return domain.TermVersion{}, err
	}
	if !found {
		return domain.TermVersion{}, fmt.Errorf("%w: no term version effective as of %s (asOf is before the account's booking date)", ErrNotFound, asOf.Format("2006-01-02"))
	}
	return v, nil
}
