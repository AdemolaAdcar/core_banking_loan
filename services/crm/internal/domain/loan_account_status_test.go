package domain

import "testing"

func TestInferLoanAccountStatus(t *testing.T) {
	cases := []struct {
		event EventType
		want  LoanAccountStatus
	}{
		{EventAccountBooked, LoanAccountPendingDisbursement},
		{EventAccountDisbursed, LoanAccountDisbursed},
		{EventRepaymentPosted, LoanAccountDisbursed},
		{EventDelinquencyStatusChanged, LoanAccountDisbursed},
		{EventTermsModified, LoanAccountDisbursed},
		{EventAccountClosed, LoanAccountClosed},
		{EventAccountChargedOff, LoanAccountChargedOff},
	}
	for _, c := range cases {
		if got := InferLoanAccountStatus(c.event); got != c.want {
			t.Errorf("InferLoanAccountStatus(%s) = %s, want %s", c.event, got, c.want)
		}
	}
}
