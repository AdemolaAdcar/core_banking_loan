package service

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/postingrules"
)

// TestConcurrentDisbursementFundingConfirmations_DifferentAccounts proves
// this service's write path is safe under real concurrent load -- the
// concurrent-posting edge case flagged as a coverage gap in
// PR_DESCRIPTION.md. Run with `go test -race`: N goroutines each confirm
// funding for their OWN, distinct loan account through the fakeStore
// (mutex-guarded, per fakestore_test.go's own doc comment) and the fake
// GL client (also mutex-guarded) -- zero data races expected, all N
// succeed independently, and each account ends up Disbursed with exactly
// one PR-DISB-01 posting.
func TestConcurrentDisbursementFundingConfirmations_DifferentAccounts(t *testing.T) {
	const n = 20
	svc, _, gl := newTestService()

	accountIDs := make([]string, n)
	for i := 0; i < n; i++ {
		a := mustBook(t, svc, fmt.Sprintf("approval-%d", i))
		accountIDs[i] = a.LoanAccountID
		if _, err := svc.CreateDisbursement(context.Background(), a.LoanAccountID, fmt.Sprintf("disb-%d", i), "officer-1"); err != nil {
			t.Fatalf("CreateDisbursement %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.ConfirmDisbursementFunding(context.Background(), fmt.Sprintf("disb-%d", i), fmt.Sprintf("instr-%d", i))
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: ConfirmDisbursementFunding failed: %v", i, err)
		}
	}
	if gl.CallCountForRule(string(postingrules.PRDISB01)) != n {
		t.Fatalf("expected exactly %d PR-DISB-01 calls, got %d", n, gl.CallCountForRule(string(postingrules.PRDISB01)))
	}
	for i, id := range accountIDs {
		a, err := svc.GetLoanAccount(context.Background(), id)
		if err != nil {
			t.Fatalf("GetLoanAccount %d: %v", i, err)
		}
		if a.Status != domain.StatusDisbursed {
			t.Fatalf("account %d: expected Disbursed, got %s", i, a.Status)
		}
	}
}
