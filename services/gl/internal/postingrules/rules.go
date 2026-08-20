// Package postingrules implements the 11 approved posting rules from
// docs/design-notes.md Appendix B ("Consolidated Posting-Rule Catalog"),
// exactly as currently spec'd in specs/openapi/gl-posting-engine.yaml
// v0.4.0 — nothing here invents a rule, an account mapping, or a
// behavior beyond what that catalog already documents.
//
// Only the 8 "forward" (non-reversal) rules get a constructor in this
// file: PR-DISB-01, PR-ACCR-01, PR-REPAY-01, PR-DELINQ-01, PR-PAYOFF-01,
// PR-CHGOFF-01, PR-CHGOFF-02, PR-MOD-01. The 3 reversal rules
// (PR-DISB-02, PR-REPAY-02, PR-DELINQ-02) are deliberately NOT built
// here as independent debit/credit constructors — see ReversalRules and
// internal/service, which builds a reversal's lines by fetching the
// original entry (via reversalOfSourceEventId) and mirroring its actual
// posted lines (domain.MirrorForReversal), never by re-deriving from the
// reversal request's own amount. This is what makes "a reversal of a
// reversal" well-defined generically, for all three reversal rules, via
// one shared mechanism, rather than three bespoke ones — and it is also
// simply more correct: a reversal must undo exactly what was posted, not
// whatever the reversal caller happens to submit as `amount`.
package postingrules

import (
	"fmt"

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/coa"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/domain"
)

// ManifestVersion is the posting-rule catalog version stamped onto
// every JournalEntry.PostingRuleVersion this package produces — bump it
// deliberately whenever a rule's account mapping or logic changes, so
// invariant 5 ("any entry can be explained after the fact") stays true
// even after this catalog itself changes in a future increment.
const ManifestVersion = "1.0.0"

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

// AllRuleCodes matches PostingRuleCode's enum in gl-posting-engine.yaml
// exactly, in the same order.
var AllRuleCodes = []RuleCode{
	PRDISB01, PRDISB02, PRACCR01, PRREPAY01, PRREPAY02,
	PRDELINQ01, PRDELINQ02, PRPAYOFF01, PRCHGOFF01, PRCHGOFF02, PRMOD01,
}

func IsKnownRuleCode(code string) bool {
	for _, c := range AllRuleCodes {
		if string(c) == code {
			return true
		}
	}
	return false
}

// ReversalRules maps every reversal rule to the forward rule it undoes
// — documentation and test value only; internal/service does not use
// this map to derive a reversal's lines (see the package doc comment
// for why).
var ReversalRules = map[RuleCode]RuleCode{
	PRDISB02:   PRDISB01,
	PRREPAY02:  PRREPAY01,
	PRDELINQ02: PRDELINQ01,
}

func IsReversalRule(code RuleCode) bool {
	_, ok := ReversalRules[code]
	return ok
}

type InputShape string

const (
	ShapeAmount         InputShape = "amount"
	ShapeAllocation     InputShape = "allocation"
	ShapeCapitalization InputShape = "capitalization"
)

// ShapeFor matches PostJournalEntryRequest's documented per-rule input
// shape exactly.
func ShapeFor(code RuleCode) (InputShape, error) {
	switch code {
	case PRDISB01, PRDISB02, PRACCR01, PRDELINQ01, PRDELINQ02, PRCHGOFF02:
		return ShapeAmount, nil
	case PRREPAY01, PRREPAY02, PRPAYOFF01, PRCHGOFF01:
		return ShapeAllocation, nil
	case PRMOD01:
		return ShapeCapitalization, nil
	default:
		return "", fmt.Errorf("postingrules: unknown rule code %q", code)
	}
}

// RequiredMetadataKeys lists the metadata keys PostJournalEntryRequest
// requires for a given rule — PR-ACCR-01, PR-DELINQ-02, and PR-CHGOFF-01
// only; every other rule requires none.
func RequiredMetadataKeys(code RuleCode) []string {
	switch code {
	case PRACCR01:
		return []string{"businessDate", "annualInterestRateBps", "dayCountConvention", "principalBasis", "termVersion"}
	case PRDELINQ02:
		return []string{"waivedBy", "reasonCode"}
	case PRCHGOFF01:
		return []string{"chargeoffDecisionReference", "confirmedBy"}
	default:
		return nil
	}
}

// Allocation mirrors PostJournalEntryRequest's Allocation shape.
type Allocation struct {
	FeeAmount       domain.Money
	InterestAmount  domain.Money
	PrincipalAmount domain.Money
}

// Capitalization mirrors PostJournalEntryRequest's Capitalization shape.
type Capitalization struct {
	InterestAmount domain.Money
	FeeAmount      domain.Money
}

func twoLine(debitAccount, creditAccount string, amount domain.Money) []domain.Line {
	return []domain.Line{
		{GLAccount: debitAccount, Direction: domain.Debit, Amount: amount},
		{GLAccount: creditAccount, Direction: domain.Credit, Amount: amount},
	}
}

// PRDISB01Lines: Dr LoanReceivable P / Cr CashNostro P — fund a disbursement.
func PRDISB01Lines(amount domain.Money) []domain.Line {
	return twoLine(coa.LoanReceivable, coa.CashNostro, amount)
}

// PRACCR01Lines: Dr InterestReceivable X / Cr InterestIncome X — one day's accrual.
func PRACCR01Lines(amount domain.Money) []domain.Line {
	return twoLine(coa.InterestReceivable, coa.InterestIncome, amount)
}

// PRDELINQ01Lines: Dr FeeReceivable F / Cr FeeIncome F — assess a late fee.
func PRDELINQ01Lines(amount domain.Money) []domain.Line {
	return twoLine(coa.FeeReceivable, coa.FeeIncome, amount)
}

// PRCHGOFF02Lines: Dr CashNostro R / Cr RecoveryIncome R — recovery on a
// charged-off account. Deliberately NOT a reversal of PR-CHGOFF-01 (see
// the catalog): a recovery collects money on an asset already written
// off, it doesn't undo the write-off decision.
func PRCHGOFF02Lines(amount domain.Money) []domain.Line {
	return twoLine(coa.CashNostro, coa.RecoveryIncome, amount)
}

func allocationSum(fee, interest, principal domain.Money) (domain.Money, error) {
	total, err := fee.Add(interest)
	if err != nil {
		return domain.Money{}, err
	}
	return total.Add(principal)
}

// categoryLines builds up to 3 lines, one per non-zero category — per
// Allocation/Capitalization's documented rule: "GL omits the
// corresponding line entirely for any category that is exactly 0."
func categoryLines(direction domain.Direction, fee, interest, principal domain.Money, feeAcct, interestAcct, principalAcct string) []domain.Line {
	var lines []domain.Line
	if !fee.IsZero() {
		lines = append(lines, domain.Line{GLAccount: feeAcct, Direction: direction, Amount: fee})
	}
	if !interest.IsZero() {
		lines = append(lines, domain.Line{GLAccount: interestAcct, Direction: direction, Amount: interest})
	}
	if !principal.IsZero() {
		lines = append(lines, domain.Line{GLAccount: principalAcct, Direction: direction, Amount: principal})
	}
	return lines
}

// PRREPAY01Lines implements PR-REPAY-01: Dr CashNostro (the allocation's
// sum) / Cr FeeReceivable + InterestReceivable + LoanReceivable, per
// category.
//
// KNOWN, ESCALATED DEFECT — see services/gl/PR_DESCRIPTION.md: the debit
// side is derived as the SUM of the three credit categories. There is no
// independent "amount actually received" field anywhere in
// PostJournalEntryRequest's Allocation shape for this function to source
// the CashNostro debit from instead, so an over- or under-payment
// relative to what was actually collected is structurally invisible to
// the ledger — this function has no other number to compare the sum
// against. Flagged in an earlier phase, still unresolved in the
// currently-approved v0.4.0 spec, and implemented here exactly as
// approved per this role's own "refuse and escalate, don't silently
// implement" instruction: escalated loudly in the PR description, not
// silently accepted as correct.
func PRREPAY01Lines(a Allocation) ([]domain.Line, error) {
	debitTotal, err := allocationSum(a.FeeAmount, a.InterestAmount, a.PrincipalAmount)
	if err != nil {
		return nil, err
	}
	lines := []domain.Line{{GLAccount: coa.CashNostro, Direction: domain.Debit, Amount: debitTotal}}
	lines = append(lines, categoryLines(domain.Credit, a.FeeAmount, a.InterestAmount, a.PrincipalAmount, coa.FeeReceivable, coa.InterestReceivable, coa.LoanReceivable)...)
	return lines, nil
}

// PRPAYOFF01Lines implements PR-PAYOFF-01: Dr CashNostro (the
// allocation's sum) / Cr FeeReceivable + InterestReceivable +
// LoanReceivable, zeroing the loan. Shares PR-REPAY-01's exact same
// known, escalated defect — see PRREPAY01Lines's doc comment.
func PRPAYOFF01Lines(a Allocation) ([]domain.Line, error) {
	debitTotal, err := allocationSum(a.FeeAmount, a.InterestAmount, a.PrincipalAmount)
	if err != nil {
		return nil, err
	}
	lines := []domain.Line{{GLAccount: coa.CashNostro, Direction: domain.Debit, Amount: debitTotal}}
	lines = append(lines, categoryLines(domain.Credit, a.FeeAmount, a.InterestAmount, a.PrincipalAmount, coa.FeeReceivable, coa.InterestReceivable, coa.LoanReceivable)...)
	return lines, nil
}

// PRCHGOFF01Lines implements PR-CHGOFF-01: Dr AllowanceForLoanLosses
// (the allocation's sum) / Cr FeeReceivable + InterestReceivable +
// LoanReceivable — write off a loan.
//
// KNOWN, ESCALATED DEFECT — see services/gl/PR_DESCRIPTION.md:
// AllowanceForLoanLosses is a contra-asset reserve that should be funded
// (credited) by a provisioning rule before it is ever debited here — no
// such rule exists anywhere in the current 11-rule catalog; this is the
// ONLY rule that ever touches this account, and it only debits it.
// Every call to this function drives the account further into an
// unfunded position. Flagged in an earlier phase, still unresolved in
// the currently-approved v0.4.0 spec, implemented here exactly as
// approved: escalated, not silently accepted.
func PRCHGOFF01Lines(a Allocation) ([]domain.Line, error) {
	debitTotal, err := allocationSum(a.FeeAmount, a.InterestAmount, a.PrincipalAmount)
	if err != nil {
		return nil, err
	}
	lines := []domain.Line{{GLAccount: coa.AllowanceForLoanLosses, Direction: domain.Debit, Amount: debitTotal}}
	lines = append(lines, categoryLines(domain.Credit, a.FeeAmount, a.InterestAmount, a.PrincipalAmount, coa.FeeReceivable, coa.InterestReceivable, coa.LoanReceivable)...)
	return lines, nil
}

// PRMOD01Lines implements PR-MOD-01: Dr LoanReceivable (interest+fee
// combined) / Cr InterestReceivable, Cr FeeReceivable (whichever are
// non-zero) — capitalize past-due interest/fees into principal. Touches
// no Income account: both amounts were already recognized as Income
// once, at accrual/assessment time; capitalizing only moves where the
// corresponding receivable balance lives.
func PRMOD01Lines(c Capitalization) ([]domain.Line, error) {
	total, err := c.InterestAmount.Add(c.FeeAmount)
	if err != nil {
		return nil, err
	}
	lines := []domain.Line{{GLAccount: coa.LoanReceivable, Direction: domain.Debit, Amount: total}}
	if !c.InterestAmount.IsZero() {
		lines = append(lines, domain.Line{GLAccount: coa.InterestReceivable, Direction: domain.Credit, Amount: c.InterestAmount})
	}
	if !c.FeeAmount.IsZero() {
		lines = append(lines, domain.Line{GLAccount: coa.FeeReceivable, Direction: domain.Credit, Amount: c.FeeAmount})
	}
	return lines, nil
}
