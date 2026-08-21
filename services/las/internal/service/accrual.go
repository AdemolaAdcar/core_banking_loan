package service

import (
	"context"
	"errors"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/glclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/postingrules"
)

func (s *Service) ListAccrualEligibleAccounts(ctx context.Context, asOf time.Time) ([]domain.LoanAccount, error) {
	return s.store.ListAccrualEligibleAccounts(ctx, asOf)
}

type AccrualBatchSummary struct {
	AsOf                 time.Time
	EligibleAccountCount int
	PostedCount          int
	ExcludedCount        int
	FailedAccountIDs     []string
	StartedAt            time.Time
	CompletedAt          time.Time
}

func daysInYear(convention string) int64 {
	switch convention {
	case "ACTUAL_365":
		return 365
	default: // ACTUAL_360, THIRTY_360
		return 360
	}
}

// RunDailyAccrual implements runDailyAccrual (REQ-CB-ACCR-001..007):
// for every eligible account, resolves the term version effective as of
// asOf (REQ-CB-MOD-006 cross-epic dependency), computes one day's simple
// interest on the account's current outstanding principal (from its
// live BalanceProjection, never a separately tracked principal field),
// and posts exactly one PR-ACCR-01. A per-account failure is recorded in
// FailedAccountIDs and does not abort the rest of the batch
// (REQ-CB-ACCR-003) — idempotent per (loanAccountId, asOf) via the
// deterministic accr:{loanAccountId}:{asOf} Idempotency-Key, so a retried
// run never double-posts.
func (s *Service) RunDailyAccrual(ctx context.Context, asOf time.Time) AccrualBatchSummary {
	startedAt := s.now()
	eligible, err := s.store.ListAccrualEligibleAccounts(ctx, asOf)
	summary := AccrualBatchSummary{AsOf: asOf, StartedAt: startedAt}
	if err != nil {
		summary.CompletedAt = s.now()
		return summary
	}
	summary.EligibleAccountCount = len(eligible)

	for _, account := range eligible {
		if err := s.accrueOne(ctx, account, asOf); err != nil {
			if errors.Is(err, errAccrualSkipped) {
				summary.ExcludedCount++
				continue
			}
			summary.FailedAccountIDs = append(summary.FailedAccountIDs, account.LoanAccountID)
			continue
		}
		summary.PostedCount++
	}
	summary.CompletedAt = s.now()
	return summary
}

// errAccrualSkipped marks an account that was eligible but has nothing
// to accrue (no effective term version yet, or zero outstanding
// principal/interest) — counted as excluded, not failed.
var errAccrualSkipped = errors.New("service: nothing to accrue")

func (s *Service) accrueOne(ctx context.Context, account domain.LoanAccount, asOf time.Time) error {
	term, found, err := s.store.GetEffectiveTermVersion(ctx, account.LoanAccountID, asOf)
	if err != nil {
		return err
	}
	if !found {
		return errAccrualSkipped
	}
	projection, err := s.GetBalanceProjection(ctx, account.LoanAccountID)
	if err != nil {
		return err
	}
	principal := projection.OutstandingPrincipal.Amount
	if principal <= 0 {
		return errAccrualSkipped
	}
	dailyInterest := (principal * int64(term.AnnualInterestRateBps)) / 10000 / daysInYear(term.DayCountConvention)
	if dailyInterest <= 0 {
		return errAccrualSkipped
	}
	amount := domain.Money{Amount: dailyInterest, Currency: projection.OutstandingPrincipal.Currency}

	idempotencyKey := "accr:" + account.LoanAccountID + ":" + asOf.Format("2006-01-02")
	_, err = s.postToGL(ctx, glclient.PostJournalEntryInput{
		IdempotencyKey: idempotencyKey, PostingRuleCode: postingrules.PRACCR01, LoanAccountID: account.LoanAccountID, Amount: &amount,
		Metadata: map[string]any{
			"businessDate": asOf.Format("2006-01-02"), "annualInterestRateBps": term.AnnualInterestRateBps,
			"dayCountConvention": term.DayCountConvention, "principalBasis": principal, "termVersion": term.Version,
		},
	})
	if err != nil {
		return err
	}
	_, err = s.RefreshBalanceProjection(ctx, account.LoanAccountID)
	return err
}
