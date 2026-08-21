// Package postingrules names the posting-rule codes and input shapes
// this service is allowed to send to GLPostingAPI, per
// docs/design-notes.md Appendix B and services/gl/internal/postingrules
// (the authoritative implementation of what each code actually posts).
//
// This package does NOT construct journal entry lines, pick a GL
// account, or decide a debit/credit direction — none of that.
// GLPostingAPI is the sole owner of that logic; this service only ever
// selects a rule code and builds the request shape (amount / allocation
// / capitalization) that code requires, then hands it to
// internal/glclient.Client.PostJournalEntry. That is what "this service
// never constructs a journal entry inline — it always goes through the
// Posting Engine's typed client" means concretely: not even the GL
// account codes are known here.
package postingrules

// RuleCode mirrors GLPostingAPI's PostingRuleCode enum exactly — see
// services/gl/internal/postingrules.AllRuleCodes, the source of truth
// this list is copied from.
type RuleCode string

const (
	PRDISB01   RuleCode = "PR-DISB-01"
	PRDISB02   RuleCode = "PR-DISB-02"
	PRACCR01   RuleCode = "PR-ACCR-01"
	PRREPAY01  RuleCode = "PR-REPAY-01"
	PRREPAY02  RuleCode = "PR-REPAY-02"
	PRDELINQ01 RuleCode = "PR-DELINQ-01"
	PRDELINQ02 RuleCode = "PR-DELINQ-02"
	PRPAYOFF01 RuleCode = "PR-PAYOFF-01"
	PRCHGOFF01 RuleCode = "PR-CHGOFF-01"
	PRCHGOFF02 RuleCode = "PR-CHGOFF-02"
	PRMOD01    RuleCode = "PR-MOD-01"
)

// Trigger documents, for every LAS operation that posts to GL, exactly
// one rule code and the condition under which it fires — the same
// mapping restated in PR_DESCRIPTION.md for human review. Kept here as
// a single source of truth internal/service's tests assert against, so
// the two documents can never silently drift.
type Trigger struct {
	Operation string
	Rule      RuleCode
	Condition string
}

var Triggers = []Trigger{
	{Operation: "createDisbursement (GL confirmation path)", Rule: PRDISB01, Condition: "disbursement funding confirmed for an Approved account"},
	{Operation: "reverseDisbursement", Rule: PRDISB02, Condition: "Ops-confirmed payment return/failure on a Disbursed-pending-reversal disbursement"},
	{Operation: "runDailyAccrual", Rule: PRACCR01, Condition: "one posting per eligible Disbursed, non-excluded account per business day"},
	{Operation: "receiveRepaymentNotification — branch (a) ordinary repayment", Rule: PRREPAY01, Condition: "matched, not ChargedOff, no valid payoffQuoteId (or underpaid against one)"},
	{Operation: "receiveRepaymentNotification — branch (b)/(d) payoff", Rule: PRPAYOFF01, Condition: "matched, not ChargedOff, valid unexpired payoffQuoteId with amount >= quote total"},
	{Operation: "receiveRepaymentNotification — branch (0) recovery", Rule: PRCHGOFF02, Condition: "matched loan account already ChargedOff — checked first, regardless of payoffQuoteId"},
	{Operation: "reverseRepayment", Rule: PRREPAY02, Condition: "confirmed NSF/return or misapplication of a Posted repayment"},
	{Operation: "runDailyDelinquencyAssessment", Rule: PRDELINQ01, Condition: "account newly past its contractual grace period on a missed installment"},
	{Operation: "waiveFee", Rule: PRDELINQ02, Condition: "confirmed authorized waiver of an Assessed fee"},
	{Operation: "recordChargeOff", Rule: PRCHGOFF01, Condition: "confirmed, approved charge-off decision on a Disbursed account"},
	{Operation: "applyModification — Branch A", Rule: PRMOD01, Condition: "capitalization present (interest and/or fee capitalized into principal); Branch B posts nothing"},
}
