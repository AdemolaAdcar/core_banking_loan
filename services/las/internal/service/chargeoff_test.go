package service

import (
	"context"
	"errors"
	"testing"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/postingrules"
)

func TestRecordChargeOff_TransitionsAccountAndPostsWriteOff(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 40000, 200, 100, "USD")

	c, err := svc.RecordChargeOff(context.Background(), account.LoanAccountID, "chargeoff-1", "ops-1")
	if err != nil {
		t.Fatalf("RecordChargeOff: %v", err)
	}
	if c.Status != domain.ChargeOffDone {
		t.Fatalf("expected ChargedOff, got %s", c.Status)
	}
	if c.WriteOffAmount.Amount != 40300 {
		t.Fatalf("expected write-off amount 40300 (40000+200+100), got %d", c.WriteOffAmount.Amount)
	}
	if gl.CallCountForRule(string(postingrules.PRCHGOFF01)) != 1 {
		t.Fatalf("expected exactly one PR-CHGOFF-01 call")
	}

	reloaded, err := svc.GetLoanAccount(context.Background(), account.LoanAccountID)
	if err != nil {
		t.Fatalf("GetLoanAccount: %v", err)
	}
	if reloaded.Status != domain.StatusChargedOff {
		t.Fatalf("expected account status ChargedOff, got %s", reloaded.Status)
	}
}

func TestRecordChargeOff_FromApprovedAccount_Rejected(t *testing.T) {
	svc, _, _ := newTestService()
	account := mustBook(t, svc, "approval-1") // never disbursed

	_, err := svc.RecordChargeOff(context.Background(), account.LoanAccountID, "chargeoff-1", "ops-1")
	var invalidErr *domain.ErrInvalidTransition
	if !errors.As(err, &invalidErr) {
		t.Fatalf("expected ErrInvalidTransition for a charge-off attempt on a non-Disbursed account, got %v", err)
	}
}

func TestRecordChargeOff_AlreadyChargedOff_Rejected(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 1000, 0, 0, "USD")
	if _, err := svc.RecordChargeOff(context.Background(), account.LoanAccountID, "chargeoff-1", "ops-1"); err != nil {
		t.Fatalf("first charge-off: %v", err)
	}

	_, err := svc.RecordChargeOff(context.Background(), account.LoanAccountID, "chargeoff-2", "ops-1")
	var invalidErr *domain.ErrInvalidTransition
	if !errors.As(err, &invalidErr) {
		t.Fatalf("expected ErrInvalidTransition -- ChargedOff is terminal, a second charge-off must be rejected, got %v", err)
	}
}

func TestRecordChargeOff_IdempotentReplay(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 1000, 0, 0, "USD")

	first, err := svc.RecordChargeOff(context.Background(), account.LoanAccountID, "chargeoff-1", "ops-1")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := svc.RecordChargeOff(context.Background(), account.LoanAccountID, "chargeoff-1", "someone-else")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.ConfirmedBy != first.ConfirmedBy {
		t.Fatalf("expected the replay to return the ORIGINAL chargeoff, not process the second call's input")
	}
	if gl.CallCountForRule(string(postingrules.PRCHGOFF01)) != 1 {
		t.Fatalf("expected exactly one PR-CHGOFF-01 call across both the original and the replay")
	}
}
