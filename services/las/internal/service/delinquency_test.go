package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/glclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/postingrules"
)

func TestUpdateDelinquency_BucketTransition_AssessesLateFee(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)

	dueDate := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lateFee := usd(2500)
	result, err := svc.UpdateDelinquency(context.Background(), account.LoanAccountID, 5, dueDate, &lateFee, time.Now())
	if err != nil {
		t.Fatalf("UpdateDelinquency: %v", err)
	}
	if !result.BucketChanged || result.Account.DPDBucket != domain.DPD1to29 {
		t.Fatalf("expected bucket to change to 1-29, got %+v", result)
	}
	if !result.FeeAssessed {
		t.Fatalf("expected a late fee to be assessed on crossing out of Current")
	}
	if gl.CallCountForRule(string(postingrules.PRDELINQ01)) != 1 {
		t.Fatalf("expected exactly one PR-DELINQ-01 call")
	}
}

func TestUpdateDelinquency_NoFeeAmountSupplied_NoFeeAssessed(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)

	result, err := svc.UpdateDelinquency(context.Background(), account.LoanAccountID, 5, time.Now(), nil, time.Now())
	if err != nil {
		t.Fatalf("UpdateDelinquency: %v", err)
	}
	if !result.BucketChanged {
		t.Fatalf("expected the bucket to still change even without a fee amount")
	}
	if result.FeeAssessed {
		t.Fatalf("expected no fee to be assessed when lateFeeAmount is nil")
	}
	if gl.CallCountForRule(string(postingrules.PRDELINQ01)) != 0 {
		t.Fatalf("expected zero PR-DELINQ-01 calls, got %d", gl.CallCountForRule(string(postingrules.PRDELINQ01)))
	}
}

func TestUpdateDelinquency_CrossesNonAccrualThreshold_SetsFlag(t *testing.T) {
	svc, _, _ := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)

	result, err := svc.UpdateDelinquency(context.Background(), account.LoanAccountID, 95, time.Now(), nil, time.Now())
	if err != nil {
		t.Fatalf("UpdateDelinquency: %v", err)
	}
	if !result.NonAccrualChanged || !result.Account.NonAccrualFlag {
		t.Fatalf("expected NonAccrualFlag to be set at DPD 95, got %+v", result.Account)
	}
	if result.Account.DPDBucket != domain.DPD90Plus {
		t.Fatalf("expected bucket 90+, got %s", result.Account.DPDBucket)
	}
}

func TestUpdateDelinquency_CuresBelowThreshold_ClearsFlag(t *testing.T) {
	svc, _, _ := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)

	if _, err := svc.UpdateDelinquency(context.Background(), account.LoanAccountID, 95, time.Now(), nil, time.Now()); err != nil {
		t.Fatalf("first update: %v", err)
	}
	result, err := svc.UpdateDelinquency(context.Background(), account.LoanAccountID, 0, time.Now(), nil, time.Now())
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if result.Account.NonAccrualFlag {
		t.Fatalf("expected NonAccrualFlag to clear once DPD cures below the threshold")
	}
	if result.Account.NonAccrualSince == nil {
		t.Fatalf("expected NonAccrualSince to be retained for reporting even after a cure")
	}
}

func TestUpdateDelinquency_TerminalAccount_Rejected(t *testing.T) {
	for _, setup := range []struct {
		name  string
		build func(t *testing.T, svc *Service, gl *glclient.Fake, account domain.LoanAccount)
	}{
		{"ChargedOff", func(t *testing.T, svc *Service, gl *glclient.Fake, account domain.LoanAccount) {
			seedOutstanding(gl, account.LoanAccountID, 1000, 0, 0, "USD")
			if _, err := svc.RecordChargeOff(context.Background(), account.LoanAccountID, "chargeoff-1", "ops-1"); err != nil {
				t.Fatalf("RecordChargeOff: %v", err)
			}
		}},
		{"Closed", func(t *testing.T, svc *Service, gl *glclient.Fake, account domain.LoanAccount) {
			seedOutstanding(gl, account.LoanAccountID, 1000, 0, 0, "USD")
			quote, err := svc.GetPayoffQuote(context.Background(), account.LoanAccountID, time.Now().Add(24*time.Hour))
			if err != nil {
				t.Fatalf("GetPayoffQuote: %v", err)
			}
			ref := account.LoanAccountID
			if _, err := svc.ReceiveRepaymentNotification(context.Background(), ReceiveRepaymentNotificationInput{
				IdempotencyKey: "pay-close", LoanAccountRef: &ref, PayoffQuoteID: &quote.QuoteID, Amount: quote.TotalAmountDue, Rail: "ACH", ReceivedAt: time.Now(),
			}); err != nil {
				t.Fatalf("closing payoff: %v", err)
			}
		}},
	} {
		t.Run(setup.name, func(t *testing.T) {
			svc, _, gl := newTestService()
			account := mustBook(t, svc, "approval-"+setup.name)
			mustDisburse(t, svc, account.LoanAccountID)
			setup.build(t, svc, gl, account)

			_, err := svc.UpdateDelinquency(context.Background(), account.LoanAccountID, 30, time.Now(), nil, time.Now())
			if !errors.Is(err, ErrTerminalAccount) {
				t.Fatalf("expected ErrTerminalAccount, got %v", err)
			}
		})
	}
}

// TestRunDailyDelinquencyAssessment_SkipsChargedOffAndClosedAccounts is a
// branch-order coverage-gap test: REQ-CB-CHARGEOFF-005 requires that
// ChargedOff (and, by the same "nothing outstanding" rationale, Closed)
// accounts are never assessed even when present in the update batch --
// checked BEFORE calling UpdateDelinquency at all, not relying on
// UpdateDelinquency's own ErrTerminalAccount guard to filter them out
// after the fact.
func TestRunDailyDelinquencyAssessment_SkipsChargedOffAndClosedAccounts(t *testing.T) {
	svc, _, gl := newTestService()
	chargedOff := mustBook(t, svc, "approval-chargedoff")
	mustDisburse(t, svc, chargedOff.LoanAccountID)
	seedOutstanding(gl, chargedOff.LoanAccountID, 1000, 0, 0, "USD")
	if _, err := svc.RecordChargeOff(context.Background(), chargedOff.LoanAccountID, "chargeoff-1", "ops-1"); err != nil {
		t.Fatalf("RecordChargeOff: %v", err)
	}

	current := mustBook(t, svc, "approval-current")
	mustDisburse(t, svc, current.LoanAccountID)

	summary := svc.RunDailyDelinquencyAssessment(context.Background(), time.Now().UTC(), []DelinquencyUpdate{
		{LoanAccountID: chargedOff.LoanAccountID, DPD: 45, ScheduledDueDate: time.Now()},
		{LoanAccountID: current.LoanAccountID, DPD: 10, ScheduledDueDate: time.Now()},
	})
	if summary.EvaluatedAccountCount != 2 {
		t.Fatalf("expected 2 evaluated, got %d", summary.EvaluatedAccountCount)
	}
	if summary.NewlyPastDueCount != 1 {
		t.Fatalf("expected only the non-terminal account to be counted as newly past due, got %d", summary.NewlyPastDueCount)
	}
	if len(summary.FailedAccountIDs) != 0 {
		t.Fatalf("expected a terminal account to be silently SKIPPED, not recorded as a failure, got %v", summary.FailedAccountIDs)
	}

	reloaded, err := svc.GetLoanAccount(context.Background(), chargedOff.LoanAccountID)
	if err != nil {
		t.Fatalf("GetLoanAccount: %v", err)
	}
	if reloaded.DPDBucket != domain.DPDCurrent {
		t.Fatalf("expected the ChargedOff account's DPD bucket to be untouched, got %s", reloaded.DPDBucket)
	}
}

func TestWaiveFee_PostsReversalAndTransitionsToWaived(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)

	dueDate := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	lateFee := usd(2500)
	if _, err := svc.UpdateDelinquency(context.Background(), account.LoanAccountID, 5, dueDate, &lateFee, time.Now()); err != nil {
		t.Fatalf("UpdateDelinquency: %v", err)
	}
	feeID := "late-fee:" + account.LoanAccountID + ":" + dueDate.Format("2006-01-02")

	f, err := svc.WaiveFee(context.Background(), feeID, "waive-1", "ops-1", domain.WaiveGoodwill)
	if err != nil {
		t.Fatalf("WaiveFee: %v", err)
	}
	if f.Status != domain.FeeWaived {
		t.Fatalf("expected Waived, got %s", f.Status)
	}
	if gl.CallCountForRule(string(postingrules.PRDELINQ02)) != 1 {
		t.Fatalf("expected exactly one PR-DELINQ-02 reversal")
	}
}

func TestWaiveFee_NotAssessed_Rejected(t *testing.T) {
	svc, _, _ := newTestService()
	_, err := svc.WaiveFee(context.Background(), "no-such-fee", "waive-1", "ops-1", domain.WaiveGoodwill)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
