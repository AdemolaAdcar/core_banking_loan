package service

import (
	"context"
	"errors"
	"testing"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/glclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/postingrules"
)

func TestCreateDisbursement_TransitionsAccountToPendingDisbursement(t *testing.T) {
	svc, _, _ := newTestService()
	account := mustBook(t, svc, "approval-1")

	d, err := svc.CreateDisbursement(context.Background(), account.LoanAccountID, "disb-1", "officer-1")
	if err != nil {
		t.Fatalf("CreateDisbursement: %v", err)
	}
	if d.Status != domain.DisbursementPendingDisbursement {
		t.Fatalf("expected PendingDisbursement, got %s", d.Status)
	}
	if d.PrincipalAmount != account.CurrentTermVersion.PrincipalAmount {
		t.Fatalf("expected disbursement principal to match the account's term principal")
	}

	reloaded, err := svc.GetLoanAccount(context.Background(), account.LoanAccountID)
	if err != nil {
		t.Fatalf("GetLoanAccount: %v", err)
	}
	if reloaded.Status != domain.StatusPendingDisbursement {
		t.Fatalf("expected account status PendingDisbursement, got %s", reloaded.Status)
	}
}

func TestCreateDisbursement_IdempotentReplay(t *testing.T) {
	svc, st, _ := newTestService()
	account := mustBook(t, svc, "approval-1")

	first, err := svc.CreateDisbursement(context.Background(), account.LoanAccountID, "disb-1", "officer-1")
	if err != nil {
		t.Fatalf("CreateDisbursement: %v", err)
	}
	second, err := svc.CreateDisbursement(context.Background(), account.LoanAccountID, "disb-1", "someone-else")
	if err != nil {
		t.Fatalf("expected idempotent replay to succeed, got %v", err)
	}
	if second.RequestedBy != first.RequestedBy {
		t.Fatalf("expected the replay to return the ORIGINAL disbursement, not process the second call's input")
	}
	if len(st.disbursements) != 1 {
		t.Fatalf("expected exactly one disbursement, got %d", len(st.disbursements))
	}
}

func TestCreateDisbursement_FromNonApprovedAccount_Rejected(t *testing.T) {
	svc, _, _ := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID) // now Disbursed

	_, err := svc.CreateDisbursement(context.Background(), account.LoanAccountID, "disb-2", "officer-1")
	var invalidErr *domain.ErrInvalidTransition
	if !errors.As(err, &invalidErr) {
		t.Fatalf("expected ErrInvalidTransition, got %v", err)
	}
}

func TestConfirmDisbursementFunding_PostsPRDISB01AndTransitionsAccount(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	if _, err := svc.CreateDisbursement(context.Background(), account.LoanAccountID, "disb-1", "officer-1"); err != nil {
		t.Fatalf("CreateDisbursement: %v", err)
	}

	d, err := svc.ConfirmDisbursementFunding(context.Background(), "disb-1", "instr-1")
	if err != nil {
		t.Fatalf("ConfirmDisbursementFunding: %v", err)
	}
	if d.Status != domain.DisbursementDisbursed {
		t.Fatalf("expected Disbursed, got %s", d.Status)
	}
	if d.JournalEntryID == nil {
		t.Fatalf("expected a journalEntryId to be recorded")
	}
	if gl.CallCountForRule(string(postingrules.PRDISB01)) != 1 {
		t.Fatalf("expected exactly one PR-DISB-01 call, got %d", gl.CallCountForRule(string(postingrules.PRDISB01)))
	}

	reloaded, err := svc.GetLoanAccount(context.Background(), account.LoanAccountID)
	if err != nil {
		t.Fatalf("GetLoanAccount: %v", err)
	}
	if reloaded.Status != domain.StatusDisbursed {
		t.Fatalf("expected account status Disbursed, got %s", reloaded.Status)
	}
}

func TestConfirmDisbursementFunding_IdempotentReplay_AlreadyDisbursed(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)

	disbursementID := "disb:" + account.LoanAccountID
	if _, err := svc.ConfirmDisbursementFunding(context.Background(), disbursementID, "instr-2"); err != nil {
		t.Fatalf("expected idempotent replay to succeed, got %v", err)
	}
	if gl.CallCountForRule(string(postingrules.PRDISB01)) != 1 {
		t.Fatalf("expected the replay NOT to post a second PR-DISB-01, got %d calls", gl.CallCountForRule(string(postingrules.PRDISB01)))
	}
}

func TestConfirmDisbursementFunding_NotFound_Rejected(t *testing.T) {
	svc, _, _ := newTestService()
	_, err := svc.ConfirmDisbursementFunding(context.Background(), "no-such-disbursement", "instr-1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestConfirmDisbursementFunding_WrongStatus_Rejected(t *testing.T) {
	svc, st, _ := newTestService()
	account := mustBook(t, svc, "approval-1")
	if _, err := svc.CreateDisbursement(context.Background(), account.LoanAccountID, "disb-1", "officer-1"); err != nil {
		t.Fatalf("CreateDisbursement: %v", err)
	}

	// Simulate a disbursement Ops already rejected before funding
	// confirmation -- the only reachable "wrong status" other than the
	// idempotent-replay Disbursed case covered separately above.
	d := st.disbursements["disb-1"]
	d.Status = domain.DisbursementRejected
	st.disbursements["disb-1"] = d

	_, err := svc.ConfirmDisbursementFunding(context.Background(), "disb-1", "instr-1")
	if !errors.Is(err, ErrNotModifiable) {
		t.Fatalf("expected ErrNotModifiable, got %v", err)
	}
}

func TestConfirmDisbursementFunding_GLRejection_LeavesStateUnchanged(t *testing.T) {
	svc, st, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	if _, err := svc.CreateDisbursement(context.Background(), account.LoanAccountID, "disb-1", "officer-1"); err != nil {
		t.Fatalf("CreateDisbursement: %v", err)
	}

	gl.NextErr = glclient.ErrRequestRejected
	_, err := svc.ConfirmDisbursementFunding(context.Background(), "disb-1", "instr-1")
	if !errors.Is(err, glclient.ErrRequestRejected) {
		t.Fatalf("expected ErrRequestRejected to propagate, got %v", err)
	}

	d := st.disbursements["disb-1"]
	if d.Status != domain.DisbursementPendingDisbursement {
		t.Fatalf("expected the disbursement to remain PendingDisbursement after a GL rejection, got %s", d.Status)
	}
	a := st.accounts[account.LoanAccountID]
	if a.Status != domain.StatusPendingDisbursement {
		t.Fatalf("expected the loan account to remain PendingDisbursement after a GL rejection, got %s", a.Status)
	}
}

func TestReverseDisbursement_TransitionsBackToApproved(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	disbursementID := "disb:" + account.LoanAccountID

	out, err := svc.ReverseDisbursement(context.Background(), disbursementID, "reverse-1", "ops-1", "PAYMENT_RETURNED")
	if err != nil {
		t.Fatalf("ReverseDisbursement: %v", err)
	}
	if out.Status != domain.DisbursementReversed {
		t.Fatalf("expected Reversed, got %s", out.Status)
	}
	if gl.CallCountForRule(string(postingrules.PRDISB02)) != 1 {
		t.Fatalf("expected exactly one PR-DISB-02 call")
	}

	reloaded, err := svc.GetLoanAccount(context.Background(), account.LoanAccountID)
	if err != nil {
		t.Fatalf("GetLoanAccount: %v", err)
	}
	if reloaded.Status != domain.StatusApproved {
		t.Fatalf("expected account status back to Approved, got %s", reloaded.Status)
	}
}

func TestReverseDisbursement_GLFailure_LeavesReversalPendingVisible(t *testing.T) {
	svc, st, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	disbursementID := "disb:" + account.LoanAccountID

	gl.NextErr = glclient.ErrGLUnavailable
	_, err := svc.ReverseDisbursement(context.Background(), disbursementID, "reverse-1", "ops-1", "PAYMENT_RETURNED")
	if !errors.Is(err, glclient.ErrGLUnavailable) {
		t.Fatalf("expected ErrGLUnavailable to propagate, got %v", err)
	}

	d := st.disbursements[disbursementID]
	if d.Status != domain.DisbursementReversalPending {
		t.Fatalf("expected the disbursement to be visible as ReversalPending after a GL failure, got %s", d.Status)
	}
	a := st.accounts[account.LoanAccountID]
	if a.Status != domain.StatusDisbursed {
		t.Fatalf("expected the loan account to remain Disbursed (not yet reverted) after a GL failure, got %s", a.Status)
	}
}
