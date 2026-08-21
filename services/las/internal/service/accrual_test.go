package service

import (
	"context"
	"testing"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/glclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/postingrules"
)

func TestRunDailyAccrual_PostsInterestForEligibleAccount(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 365000, 0, 0, "USD") // 365000*1200bps/10000/365 = 120/day

	summary := svc.RunDailyAccrual(context.Background(), time.Now().UTC())
	if summary.PostedCount != 1 {
		t.Fatalf("expected 1 posted, got %d (failed=%v)", summary.PostedCount, summary.FailedAccountIDs)
	}
	if gl.CallCountForRule(string(postingrules.PRACCR01)) != 1 {
		t.Fatalf("expected exactly one PR-ACCR-01 call")
	}
	call := gl.Calls[len(gl.Calls)-1]
	if call.Amount == nil || call.Amount.Amount != 120 {
		t.Fatalf("expected daily interest 120 (365000*1200bps/10000/365), got %+v", call.Amount)
	}
}

func TestRunDailyAccrual_SkipsAccountWithZeroPrincipal(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 0, 0, 0, "USD")

	summary := svc.RunDailyAccrual(context.Background(), time.Now().UTC())
	if summary.ExcludedCount != 1 {
		t.Fatalf("expected 1 excluded, got %d", summary.ExcludedCount)
	}
	if summary.PostedCount != 0 {
		t.Fatalf("expected 0 posted, got %d", summary.PostedCount)
	}
	if gl.CallCountForRule(string(postingrules.PRACCR01)) != 0 {
		t.Fatalf("expected zero PR-ACCR-01 calls, got %d", gl.CallCountForRule(string(postingrules.PRACCR01)))
	}
}

func TestRunDailyAccrual_NonAccrualAccountExcludedFromEligibility(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 100000, 0, 0, "USD")

	if _, err := svc.UpdateDelinquency(context.Background(), account.LoanAccountID, 95, time.Now(), nil, time.Now()); err != nil {
		t.Fatalf("UpdateDelinquency: %v", err)
	}

	summary := svc.RunDailyAccrual(context.Background(), time.Now().UTC())
	if summary.EligibleAccountCount != 0 {
		t.Fatalf("expected a non-accrual-flagged account to be excluded from eligibility entirely, got %d eligible", summary.EligibleAccountCount)
	}
}

func TestRunDailyAccrual_PerAccountFailureDoesNotAbortBatch(t *testing.T) {
	svc, _, gl := newTestService()
	a1 := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, a1.LoanAccountID)
	a2 := mustBook(t, svc, "approval-2")
	mustDisburse(t, svc, a2.LoanAccountID)
	seedOutstanding(gl, a1.LoanAccountID, 100000, 0, 0, "USD")
	seedOutstanding(gl, a2.LoanAccountID, 100000, 0, 0, "USD")

	gl.NextErr = glclient.ErrGLUnavailable // consumed by whichever account is processed first
	summary := svc.RunDailyAccrual(context.Background(), time.Now().UTC())
	if summary.EligibleAccountCount != 2 {
		t.Fatalf("expected 2 eligible accounts, got %d", summary.EligibleAccountCount)
	}
	if summary.PostedCount != 1 || len(summary.FailedAccountIDs) != 1 {
		t.Fatalf("expected exactly one success and one recorded failure, got posted=%d failed=%v", summary.PostedCount, summary.FailedAccountIDs)
	}
}
