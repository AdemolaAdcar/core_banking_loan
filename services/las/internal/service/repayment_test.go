package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/postingrules"
)

func TestReceiveRepaymentNotification_NoLoanAccountRef_Unmatched(t *testing.T) {
	svc, _, _ := newTestService()
	result, err := svc.ReceiveRepaymentNotification(context.Background(), ReceiveRepaymentNotificationInput{
		IdempotencyKey: "pay-1", Amount: usd(500), Rail: "ACH", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("ReceiveRepaymentNotification: %v", err)
	}
	if result.Kind != KindRepayment || result.Repayment.Status != domain.RepaymentUnmatched {
		t.Fatalf("expected an Unmatched repayment, got %+v", result)
	}
	if *result.Repayment.UnmatchedReasonCode != domain.UnmatchedNoMatch {
		t.Fatalf("expected NO_MATCH, got %s", *result.Repayment.UnmatchedReasonCode)
	}
}

func TestReceiveRepaymentNotification_UnknownAccount_Unmatched(t *testing.T) {
	svc, _, _ := newTestService()
	ref := "does-not-exist"
	result, err := svc.ReceiveRepaymentNotification(context.Background(), ReceiveRepaymentNotificationInput{
		IdempotencyKey: "pay-1", LoanAccountRef: &ref, Amount: usd(500), Rail: "ACH", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("ReceiveRepaymentNotification: %v", err)
	}
	if result.Repayment.Status != domain.RepaymentUnmatched {
		t.Fatalf("expected Unmatched, got %s", result.Repayment.Status)
	}
}

// TestReceiveRepaymentNotification_ChargedOffAccount_AlwaysRecovery_EvenWithPayoffQuoteID
// is the key branch-order regression test flagged as a coverage gap in
// PR_DESCRIPTION.md: a ChargedOff account must ALWAYS be treated as a
// recovery, even when the caller also supplies a still-valid, unexpired
// payoffQuoteId for it -- branch (0) is checked unconditionally, before
// any payoffQuoteId branching.
func TestReceiveRepaymentNotification_ChargedOffAccount_AlwaysRecovery_EvenWithPayoffQuoteID(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 50000, 0, 0, "USD")

	if _, err := svc.RecordChargeOff(context.Background(), account.LoanAccountID, "chargeoff-1", "ops-1"); err != nil {
		t.Fatalf("RecordChargeOff: %v", err)
	}

	// A payoff quote issued BEFORE the charge-off (quoteCache is
	// in-process and never expires proactively on a status change) could
	// still be sitting in quoteCache when the repayment notification
	// arrives -- simulate that directly, since GetPayoffQuote itself
	// correctly refuses to issue a NEW quote for a now-terminal account.
	fakeQuoteID := "quote-issued-before-chargeoff"
	svc.quotes.put(domain.PayoffQuote{
		QuoteID: fakeQuoteID, LoanAccountID: account.LoanAccountID, GoodThrough: time.Now().Add(24 * time.Hour),
		TotalAmountDue: usd(50000), GeneratedAt: time.Now(),
	})

	ref := account.LoanAccountID
	result, err := svc.ReceiveRepaymentNotification(context.Background(), ReceiveRepaymentNotificationInput{
		IdempotencyKey: "pay-1", LoanAccountRef: &ref, PayoffQuoteID: &fakeQuoteID, Amount: usd(50000), Rail: "ACH", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("ReceiveRepaymentNotification: %v", err)
	}
	if result.Kind != KindRecovery {
		t.Fatalf("expected a ChargedOff account with a payoffQuoteId to STILL be treated as a recovery, got Kind=%s", result.Kind)
	}
	if gl.CallCountForRule(string(postingrules.PRCHGOFF02)) != 1 {
		t.Fatalf("expected exactly one PR-CHGOFF-02 recovery posting")
	}
	if gl.CallCountForRule(string(postingrules.PRPAYOFF01)) != 0 {
		t.Fatalf("expected NO PR-PAYOFF-01 posting -- the payoffQuoteId branch must never be reached for a ChargedOff account")
	}
}

func TestReceiveRepaymentNotification_ClosedAccount_Rejected(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 100, 0, 0, "USD")

	quote, err := svc.GetPayoffQuote(context.Background(), account.LoanAccountID, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("GetPayoffQuote: %v", err)
	}
	ref := account.LoanAccountID
	if _, err := svc.ReceiveRepaymentNotification(context.Background(), ReceiveRepaymentNotificationInput{
		IdempotencyKey: "pay-payoff", LoanAccountRef: &ref, PayoffQuoteID: &quote.QuoteID, Amount: quote.TotalAmountDue, Rail: "ACH", ReceivedAt: time.Now(),
	}); err != nil {
		t.Fatalf("closing payoff: %v", err)
	}

	_, err = svc.ReceiveRepaymentNotification(context.Background(), ReceiveRepaymentNotificationInput{
		IdempotencyKey: "pay-after-close", LoanAccountRef: &ref, Amount: usd(10), Rail: "ACH", ReceivedAt: time.Now(),
	})
	if !errors.Is(err, ErrTerminalAccount) {
		t.Fatalf("expected ErrTerminalAccount for a repayment against a Closed account, got %v", err)
	}
}

func TestReceiveRepaymentNotification_OrdinaryRepayment_WaterfallAllocation(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 100000, 500, 1000, "USD") // fee 1000, interest 500, principal 100000

	ref := account.LoanAccountID
	result, err := svc.ReceiveRepaymentNotification(context.Background(), ReceiveRepaymentNotificationInput{
		IdempotencyKey: "pay-1", LoanAccountRef: &ref, Amount: usd(1300), Rail: "ACH", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("ReceiveRepaymentNotification: %v", err)
	}
	if result.Kind != KindRepayment || result.Repayment.Status != domain.RepaymentPosted {
		t.Fatalf("expected a Posted repayment, got %+v", result)
	}
	alloc := result.Repayment.Allocation
	if alloc.FeeAmount.Amount != 1000 || alloc.InterestAmount.Amount != 300 || alloc.PrincipalAmount.Amount != 0 {
		t.Fatalf("expected waterfall fee=1000 interest=300 principal=0, got %+v", alloc)
	}
	if gl.CallCountForRule(string(postingrules.PRREPAY01)) != 1 {
		t.Fatalf("expected exactly one PR-REPAY-01 call")
	}
}

func TestReceiveRepaymentNotification_PayoffExactMatch_ClosesAccount(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 50000, 100, 0, "USD")

	quote, err := svc.GetPayoffQuote(context.Background(), account.LoanAccountID, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("GetPayoffQuote: %v", err)
	}
	ref := account.LoanAccountID
	result, err := svc.ReceiveRepaymentNotification(context.Background(), ReceiveRepaymentNotificationInput{
		IdempotencyKey: "pay-payoff", LoanAccountRef: &ref, PayoffQuoteID: &quote.QuoteID, Amount: quote.TotalAmountDue, Rail: "ACH", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("ReceiveRepaymentNotification: %v", err)
	}
	if result.Kind != KindPayoff || result.Payoff.Status != domain.PayoffClosed {
		t.Fatalf("expected a Closed payoff, got %+v", result)
	}
	if !result.Payoff.SuspenseAmount.IsZero() {
		t.Fatalf("expected zero suspense on an exact match, got %+v", result.Payoff.SuspenseAmount)
	}

	reloaded, err := svc.GetLoanAccount(context.Background(), account.LoanAccountID)
	if err != nil {
		t.Fatalf("GetLoanAccount: %v", err)
	}
	if reloaded.Status != domain.StatusClosed {
		t.Fatalf("expected account status Closed, got %s", reloaded.Status)
	}
}

func TestReceiveRepaymentNotification_PayoffOverpayment_RecordsSuspense(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 50000, 0, 0, "USD")

	quote, err := svc.GetPayoffQuote(context.Background(), account.LoanAccountID, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("GetPayoffQuote: %v", err)
	}
	overpay := domain.Money{Amount: quote.TotalAmountDue.Amount + 500, Currency: "USD"}
	ref := account.LoanAccountID
	result, err := svc.ReceiveRepaymentNotification(context.Background(), ReceiveRepaymentNotificationInput{
		IdempotencyKey: "pay-payoff", LoanAccountRef: &ref, PayoffQuoteID: &quote.QuoteID, Amount: overpay, Rail: "ACH", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("ReceiveRepaymentNotification: %v", err)
	}
	if result.Payoff.SuspenseAmount.Amount != 500 {
		t.Fatalf("expected suspense 500, got %d", result.Payoff.SuspenseAmount.Amount)
	}
}

func TestReceiveRepaymentNotification_UnderpaidAgainstQuote_FallsBackToOrdinaryRepayment(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 50000, 0, 0, "USD")

	quote, err := svc.GetPayoffQuote(context.Background(), account.LoanAccountID, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("GetPayoffQuote: %v", err)
	}
	underpay := domain.Money{Amount: quote.TotalAmountDue.Amount - 100, Currency: "USD"}
	ref := account.LoanAccountID
	result, err := svc.ReceiveRepaymentNotification(context.Background(), ReceiveRepaymentNotificationInput{
		IdempotencyKey: "pay-underpaid", LoanAccountRef: &ref, PayoffQuoteID: &quote.QuoteID, Amount: underpay, Rail: "ACH", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("ReceiveRepaymentNotification: %v", err)
	}
	if result.Kind != KindRepayment {
		t.Fatalf("expected an underpayment against a payoff quote to degrade to an ordinary repayment, got Kind=%s", result.Kind)
	}

	reloaded, err := svc.GetLoanAccount(context.Background(), account.LoanAccountID)
	if err != nil {
		t.Fatalf("GetLoanAccount: %v", err)
	}
	if reloaded.Status != domain.StatusDisbursed {
		t.Fatalf("expected the account to remain Disbursed (not closed) after an underpayment, got %s", reloaded.Status)
	}
}

func TestReceiveRepaymentNotification_IdempotentReplay(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 100000, 0, 0, "USD")

	ref := account.LoanAccountID
	in := ReceiveRepaymentNotificationInput{IdempotencyKey: "pay-1", LoanAccountRef: &ref, Amount: usd(500), Rail: "ACH", ReceivedAt: time.Now()}
	first, err := svc.ReceiveRepaymentNotification(context.Background(), in)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := svc.ReceiveRepaymentNotification(context.Background(), in)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.Repayment.RepaymentID != first.Repayment.RepaymentID {
		t.Fatalf("expected the replay to return the same repayment")
	}
	if gl.CallCountForRule(string(postingrules.PRREPAY01)) != 1 {
		t.Fatalf("expected the replay NOT to post a second PR-REPAY-01, got %d", gl.CallCountForRule(string(postingrules.PRREPAY01)))
	}
}

func TestReverseRepayment_PaymentReturned_TransitionsToReversed(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 100000, 0, 0, "USD")

	ref := account.LoanAccountID
	result, err := svc.ReceiveRepaymentNotification(context.Background(), ReceiveRepaymentNotificationInput{
		IdempotencyKey: "pay-1", LoanAccountRef: &ref, Amount: usd(500), Rail: "ACH", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("ReceiveRepaymentNotification: %v", err)
	}

	out, err := svc.ReverseRepayment(context.Background(), result.Repayment.RepaymentID, "reverse-1", "ops-1", string(domain.ReasonPaymentReturned), nil)
	if err != nil {
		t.Fatalf("ReverseRepayment: %v", err)
	}
	if out.Status != domain.RepaymentReversed {
		t.Fatalf("expected Reversed, got %s", out.Status)
	}
	if out.CorrectedRepaymentID != nil {
		t.Fatalf("expected no correction for a PAYMENT_RETURNED reversal")
	}
	if gl.CallCountForRule(string(postingrules.PRREPAY02)) != 1 {
		t.Fatalf("expected exactly one PR-REPAY-02 reversal")
	}
	if gl.CallCountForRule(string(postingrules.PRREPAY01)) != 1 {
		t.Fatalf("expected exactly one PR-REPAY-01 posting (the original; no correction for PAYMENT_RETURNED), got %d", gl.CallCountForRule(string(postingrules.PRREPAY01)))
	}
}

// TestReverseRepayment_Misapplied_CreatesCorrectionOnNewAccount is the
// misapplied-repayment correction-path regression test flagged as a
// coverage gap: reversing with reasonCode=MISAPPLIED must reverse the
// original posting AND post a fresh PR-REPAY-01 against the CORRECT
// account, producing a second Repayment record the original references
// via CorrectedRepaymentID.
func TestReverseRepayment_Misapplied_CreatesCorrectionOnNewAccount(t *testing.T) {
	svc, _, gl := newTestService()
	wrongAccount := mustBook(t, svc, "approval-wrong")
	mustDisburse(t, svc, wrongAccount.LoanAccountID)
	correctAccount := mustBook(t, svc, "approval-correct")
	mustDisburse(t, svc, correctAccount.LoanAccountID)

	seedOutstanding(gl, wrongAccount.LoanAccountID, 100000, 0, 0, "USD")
	wrongRef := wrongAccount.LoanAccountID
	result, err := svc.ReceiveRepaymentNotification(context.Background(), ReceiveRepaymentNotificationInput{
		IdempotencyKey: "pay-1", LoanAccountRef: &wrongRef, Amount: usd(500), Rail: "ACH", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("ReceiveRepaymentNotification: %v", err)
	}

	seedOutstanding(gl, correctAccount.LoanAccountID, 100000, 0, 0, "USD")
	correctRef := correctAccount.LoanAccountID
	out, err := svc.ReverseRepayment(context.Background(), result.Repayment.RepaymentID, "reverse-1", "ops-1", string(domain.ReasonMisapplied), &correctRef)
	if err != nil {
		t.Fatalf("ReverseRepayment: %v", err)
	}
	if out.Status != domain.RepaymentCorrected {
		t.Fatalf("expected Corrected, got %s", out.Status)
	}
	if out.CorrectedRepaymentID == nil {
		t.Fatalf("expected CorrectedRepaymentID to be set")
	}

	correction, err := svc.GetRepayment(context.Background(), *out.CorrectedRepaymentID)
	if err != nil {
		t.Fatalf("GetRepayment(correction): %v", err)
	}
	if correction.LoanAccountID == nil || *correction.LoanAccountID != correctAccount.LoanAccountID {
		t.Fatalf("expected the correction repayment to belong to the CORRECT account, got %+v", correction.LoanAccountID)
	}
	if correction.Status != domain.RepaymentPosted {
		t.Fatalf("expected the correction repayment to be Posted, got %s", correction.Status)
	}
	if gl.CallCountForRule(string(postingrules.PRREPAY02)) != 1 {
		t.Fatalf("expected exactly one PR-REPAY-02 reversal (of the original, wrong-account posting)")
	}
	if gl.CallCountForRule(string(postingrules.PRREPAY01)) != 2 {
		t.Fatalf("expected two PR-REPAY-01 postings (original wrong-account + correction), got %d", gl.CallCountForRule(string(postingrules.PRREPAY01)))
	}
}

func TestReverseRepayment_Misapplied_MissingCorrectedAccount_Rejected(t *testing.T) {
	svc, _, gl := newTestService()
	account := mustBook(t, svc, "approval-1")
	mustDisburse(t, svc, account.LoanAccountID)
	seedOutstanding(gl, account.LoanAccountID, 100000, 0, 0, "USD")

	ref := account.LoanAccountID
	result, err := svc.ReceiveRepaymentNotification(context.Background(), ReceiveRepaymentNotificationInput{
		IdempotencyKey: "pay-1", LoanAccountRef: &ref, Amount: usd(500), Rail: "ACH", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("ReceiveRepaymentNotification: %v", err)
	}

	_, err = svc.ReverseRepayment(context.Background(), result.Repayment.RepaymentID, "reverse-1", "ops-1", string(domain.ReasonMisapplied), nil)
	if !errors.Is(err, ErrMalformedTerms) {
		t.Fatalf("expected ErrMalformedTerms when correctedLoanAccountId is missing for a MISAPPLIED reversal, got %v", err)
	}
}

func TestReverseRepayment_NotPosted_Rejected(t *testing.T) {
	svc, _, _ := newTestService()
	_, err := svc.ReverseRepayment(context.Background(), "no-such-repayment", "reverse-1", "ops-1", string(domain.ReasonPaymentReturned), nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
