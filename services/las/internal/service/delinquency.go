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

// nonAccrualThresholdDPD matches the design note's "DPD crosses the
// non-accrual threshold" language with a concrete number — 90 days,
// aligned with the 90+ dpdBucket boundary already in the shared enum.
const nonAccrualThresholdDPD = 90

func bucketFor(dpd int) domain.DPDBucket {
	switch {
	case dpd <= 0:
		return domain.DPDCurrent
	case dpd <= 29:
		return domain.DPD1to29
	case dpd <= 59:
		return domain.DPD30to59
	case dpd <= 89:
		return domain.DPD60to89
	default:
		return domain.DPD90Plus
	}
}

func (s *Service) ListPastDueAccounts(ctx context.Context, asOf time.Time) ([]domain.LoanAccount, error) {
	return s.store.ListPastDueAccounts(ctx, asOf)
}

type delinquencyStatusChangedPayload struct {
	LoanAccountID     string `json:"loanAccountId"`
	PreviousDPDBucket string `json:"previousDpdBucket"`
	NewDPDBucket      string `json:"newDpdBucket"`
	ChangedAt         string `json:"changedAt"`
}

type loanNonAccrualFlaggedPayload struct {
	LoanAccountID  string `json:"loanAccountId"`
	NonAccrualFlag bool   `json:"nonAccrualFlag"`
	EffectiveDate  string `json:"effectiveDate"`
}

// DelinquencyUpdate is one account's DPD-as-of-asOf input to
// UpdateDelinquency / RunDailyDelinquencyAssessment. Deriving DPD from
// a payment schedule (comparing cumulative scheduled amounts due
// against cumulative PR-REPAY-01 postings, per REQ-CB-DELINQ-001) needs
// an amortization-schedule subsystem this role does not own and this
// build does not yet have — see PR_DESCRIPTION.md. This type is the
// deliberate scope boundary: everything FROM a known DPD onward (bucket
// transition, PR-DELINQ-01 assessment, non-accrual flagging, event
// publishing) is fully implemented and tested here; computing DPD
// itself is deferred, exactly like this service's PartyAPI/PaymentAPI
// deferrals.
type DelinquencyUpdate struct {
	LoanAccountID    string
	DPD              int
	ScheduledDueDate time.Time
	// LateFeeAmount, if non-nil, is assessed via PR-DELINQ-01 when this
	// update causes the account to newly cross out of "Current" — the
	// fee AMOUNT itself is also schedule-derived and so is supplied by
	// the (deferred) caller, not computed here.
	LateFeeAmount *domain.Money
}

type DelinquencyBatchSummary struct {
	AsOf                   time.Time
	EvaluatedAccountCount  int
	NewlyPastDueCount      int
	FeesAssessedCount      int
	NonAccrualFlaggedCount int
	FailedAccountIDs       []string
	StartedAt              time.Time
	CompletedAt            time.Time
}

// RunDailyDelinquencyAssessment implements runDailyDelinquencyAssessment
// (REQ-CB-DELINQ-001..005, 008; REQ-CB-CHARGEOFF-005) over an explicit
// per-account DPD input list — see DelinquencyUpdate's doc comment for
// why. ChargedOff accounts are never assessed even if present in
// updates (REQ-CB-CHARGEOFF-005).
func (s *Service) RunDailyDelinquencyAssessment(ctx context.Context, asOf time.Time, updates []DelinquencyUpdate) DelinquencyBatchSummary {
	startedAt := s.now()
	summary := DelinquencyBatchSummary{AsOf: asOf, StartedAt: startedAt, EvaluatedAccountCount: len(updates)}
	for _, u := range updates {
		account, err := s.GetLoanAccount(ctx, u.LoanAccountID)
		if err != nil {
			summary.FailedAccountIDs = append(summary.FailedAccountIDs, u.LoanAccountID)
			continue
		}
		if account.Status == domain.StatusChargedOff || account.Status == domain.StatusClosed {
			continue // REQ-CB-CHARGEOFF-005; Closed accounts have nothing outstanding to assess a fee against
		}
		result, err := s.UpdateDelinquency(ctx, u.LoanAccountID, u.DPD, u.ScheduledDueDate, u.LateFeeAmount, asOf)
		if err != nil {
			summary.FailedAccountIDs = append(summary.FailedAccountIDs, u.LoanAccountID)
			continue
		}
		if result.BucketChanged {
			summary.NewlyPastDueCount++
		}
		if result.FeeAssessed {
			summary.FeesAssessedCount++
		}
		if result.NonAccrualChanged {
			summary.NonAccrualFlaggedCount++
		}
	}
	summary.CompletedAt = s.now()
	return summary
}

type DelinquencyUpdateResult struct {
	Account           domain.LoanAccount
	BucketChanged     bool
	FeeAssessed       bool
	NonAccrualChanged bool
}

// UpdateDelinquency applies one account's DPD-as-of-asOf: recomputes
// its dpdBucket, assesses a late fee via PR-DELINQ-01 when the account
// newly crosses out of "Current" and a fee amount is supplied
// (REQ-CB-DELINQ-002, 004), and sets/clears NonAccrualFlag when DPD
// crosses nonAccrualThresholdDPD (REQ-CB-DELINQ-005). Publishes
// delinquency.status.changed whenever the bucket actually changed, and
// loan.nonaccrual.flagged whenever the flag actually changed — never
// unconditionally.
func (s *Service) UpdateDelinquency(ctx context.Context, loanAccountID string, dpd int, scheduledDueDate time.Time, lateFeeAmount *domain.Money, asOf time.Time) (DelinquencyUpdateResult, error) {
	account, err := s.GetLoanAccount(ctx, loanAccountID)
	if err != nil {
		return DelinquencyUpdateResult{}, err
	}
	if account.IsTerminal() {
		return DelinquencyUpdateResult{}, fmt.Errorf("%w: loan account %s is Closed or ChargedOff", ErrTerminalAccount, loanAccountID)
	}

	previousBucket := account.DPDBucket
	newBucket := bucketFor(dpd)
	newNonAccrual := dpd >= nonAccrualThresholdDPD
	var nonAccrualSince *time.Time
	if newNonAccrual {
		if account.NonAccrualFlag && account.NonAccrualSince != nil {
			nonAccrualSince = account.NonAccrualSince
		} else {
			d := asOf
			nonAccrualSince = &d
		}
	} else {
		nonAccrualSince = account.NonAccrualSince // retained for reporting even after a cure, per schema doc comment
	}

	now := s.now()
	var journalEntryID *string
	feeAssessed := false
	if previousBucket == domain.DPDCurrent && newBucket != domain.DPDCurrent && lateFeeAmount != nil {
		feeID := fmt.Sprintf("late-fee:%s:%s", loanAccountID, scheduledDueDate.Format("2006-01-02"))
		if _, err := s.store.GetFee(ctx, feeID); err == nil {
			// already assessed for this due date -- idempotent no-op.
		} else if !errors.Is(err, store.ErrNotFound) {
			return DelinquencyUpdateResult{}, err
		} else {
			entry, err := s.postToGL(ctx, glclient.PostJournalEntryInput{
				IdempotencyKey: feeID, PostingRuleCode: postingrules.PRDELINQ01, LoanAccountID: loanAccountID, Amount: lateFeeAmount,
			})
			if err != nil {
				return DelinquencyUpdateResult{}, err
			}
			journalEntryID = &entry.JournalEntryID
			fee := domain.Fee{
				FeeID: feeID, LoanAccountID: loanAccountID, ScheduledDueDate: scheduledDueDate, Amount: *lateFeeAmount,
				Status: domain.FeeAssessed, JournalEntryID: journalEntryID, AssessedAt: now, UpdatedAt: now,
			}
			if err := s.store.WithinTx(ctx, func(tx store.Tx) error { return tx.SaveFee(ctx, fee) }); err != nil {
				return DelinquencyUpdateResult{}, err
			}
			feeAssessed = true
		}
	}

	nextAccount := account.WithDelinquency(dpd, newBucket, newNonAccrual, nonAccrualSince, now)
	bucketChanged := previousBucket != newBucket
	nonAccrualChanged := account.NonAccrualFlag != newNonAccrual

	err = s.store.WithinTx(ctx, func(tx store.Tx) error {
		if err := tx.SaveLoanAccount(ctx, nextAccount); err != nil {
			return err
		}
		if bucketChanged {
			if err := publishEvent(ctx, tx, loanAccountID+":delinq:"+now.Format(time.RFC3339Nano), "delinquency.status.changed", delinquencyStatusChangedPayload{
				LoanAccountID: loanAccountID, PreviousDPDBucket: string(previousBucket), NewDPDBucket: string(newBucket), ChangedAt: now.Format(time.RFC3339),
			}); err != nil {
				return err
			}
		}
		if nonAccrualChanged {
			effectiveDate := asOf
			if nonAccrualSince != nil {
				effectiveDate = *nonAccrualSince
			}
			if err := publishEvent(ctx, tx, loanAccountID+":nonaccrual:"+now.Format(time.RFC3339Nano), "loan.nonaccrual.flagged", loanNonAccrualFlaggedPayload{
				LoanAccountID: loanAccountID, NonAccrualFlag: newNonAccrual, EffectiveDate: effectiveDate.Format("2006-01-02"),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return DelinquencyUpdateResult{}, err
	}
	if feeAssessed {
		if _, err := s.RefreshBalanceProjection(ctx, loanAccountID); err != nil {
			return DelinquencyUpdateResult{}, err
		}
	}
	return DelinquencyUpdateResult{Account: nextAccount, BucketChanged: bucketChanged, FeeAssessed: feeAssessed, NonAccrualChanged: nonAccrualChanged}, nil
}

func (s *Service) GetFee(ctx context.Context, feeID string) (domain.Fee, error) {
	f, err := s.store.GetFee(ctx, feeID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Fee{}, ErrNotFound
	}
	return f, err
}

// WaiveFee implements waiveFee (REQ-CB-DELINQ-007). Requires the fee to
// be Assessed; posts PR-DELINQ-02 (a reversal of PR-DELINQ-01) — the
// original assessment entry is never edited.
func (s *Service) WaiveFee(ctx context.Context, feeID, idempotencyKey, waivedBy string, reasonCode domain.WaiveReasonCode) (domain.Fee, error) {
	f, err := s.GetFee(ctx, feeID)
	if err != nil {
		return domain.Fee{}, err
	}
	if f.Status == domain.FeeWaived || f.Status == domain.FeeWaivePending {
		return f, nil
	}
	if f.Status != domain.FeeAssessed {
		return domain.Fee{}, fmt.Errorf("%w: fee %s is not Assessed", ErrNotModifiable, feeID)
	}

	now := s.now()
	f.Status = domain.FeeWaivePending
	f.UpdatedAt = now
	if err := s.store.WithinTx(ctx, func(tx store.Tx) error { return tx.SaveFee(ctx, f) }); err != nil {
		return domain.Fee{}, err
	}

	_, err = s.postToGL(ctx, glclient.PostJournalEntryInput{
		IdempotencyKey: idempotencyKey, PostingRuleCode: postingrules.PRDELINQ02, LoanAccountID: f.LoanAccountID, Amount: &f.Amount,
		ReversalOfSourceEventID: f.JournalEntryID,
		Metadata:                map[string]any{"waivedBy": waivedBy, "reasonCode": string(reasonCode)},
	})
	if err != nil {
		return domain.Fee{}, err // f remains WaivePending -- visible via GET
	}

	f.Status = domain.FeeWaived
	f.WaivedBy = &waivedBy
	f.WaiveReasonCode = &reasonCode
	f.WaivedAt = &now
	f.UpdatedAt = now
	if err := s.store.WithinTx(ctx, func(tx store.Tx) error { return tx.SaveFee(ctx, f) }); err != nil {
		return domain.Fee{}, err
	}
	if _, err := s.RefreshBalanceProjection(ctx, f.LoanAccountID); err != nil {
		return domain.Fee{}, err
	}
	return f, nil
}
