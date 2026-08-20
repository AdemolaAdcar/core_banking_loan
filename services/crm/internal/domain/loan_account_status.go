package domain

// LoanAccountStatus mirrors Customer360.loanAccountSummaries[].status
// exactly -- status only, deliberately no principal/balance/term data.
type LoanAccountStatus string

const (
	LoanAccountApproved            LoanAccountStatus = "Approved"
	LoanAccountPendingDisbursement LoanAccountStatus = "PendingDisbursement"
	LoanAccountDisbursed           LoanAccountStatus = "Disbursed"
	LoanAccountClosed              LoanAccountStatus = "Closed"
	LoanAccountChargedOff          LoanAccountStatus = "ChargedOff"
)

// InferLoanAccountStatus derives a Customer360 status summary from the
// most recent Interaction CRM has logged for a loan account, since CRM
// has no direct read access to AccountAPI's own state machine (and no
// synchronous cross-service call at read time -- see this service's PR
// description for why: Customer360 is built entirely from CRM's own
// event-sourced interaction log, event-carried state transfer rather
// than a live query). REPAYMENT_POSTED, DELINQUENCY_STATUS_CHANGED, and
// TERMS_MODIFIED don't change this coarse status -- they all mean the
// account is still Disbursed; this enum has no delinquent/modified value
// of its own by design (status only, deliberately simple).
func InferLoanAccountStatus(e EventType) LoanAccountStatus {
	switch e {
	case EventAccountBooked:
		return LoanAccountPendingDisbursement
	case EventAccountDisbursed, EventRepaymentPosted, EventDelinquencyStatusChanged, EventTermsModified:
		return LoanAccountDisbursed
	case EventAccountClosed:
		return LoanAccountClosed
	case EventAccountChargedOff:
		return LoanAccountChargedOff
	default:
		return LoanAccountApproved
	}
}
