// Package api is the HTTP transport layer: chi handlers that translate
// between the wire shapes defined in specs/openapi/gl-posting-engine.yaml
// and internal/service's calls. Every DTO here mirrors a schema in
// specs/schemas/journal-entry.schema.json or an inline request/response
// schema in gl-posting-engine.yaml field-for-field.
package api

import (
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/postingrules"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/store"
)

type moneyDTO struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

func toMoneyDTO(m domain.Money) moneyDTO { return moneyDTO{Amount: m.Amount, Currency: m.Currency} }

func toDomainMoney(m moneyDTO) domain.Money {
	return domain.Money{Amount: m.Amount, Currency: m.Currency}
}

// lineDTO mirrors journal-entry.schema.json#/$defs/JournalEntryLine.
type lineDTO struct {
	GLAccount           string   `json:"glAccount"`
	Direction           string   `json:"direction"`
	Amount              moneyDTO `json:"amount"`
	RunningBalanceAfter moneyDTO `json:"runningBalanceAfter"`
}

// journalEntryDTO mirrors journal-entry.schema.json#/$defs/JournalEntry.
type journalEntryDTO struct {
	JournalEntryID          string         `json:"journalEntryId"`
	SourceEventID           string         `json:"sourceEventId"`
	PostingRuleCode         string         `json:"postingRuleCode"`
	PostingRuleVersion      string         `json:"postingRuleVersion"`
	LoanAccountID           string         `json:"loanAccountId,omitempty"`
	Lines                   []lineDTO      `json:"lines"`
	Balanced                bool           `json:"balanced"`
	Immutable               bool           `json:"immutable"`
	PostedAt                string         `json:"postedAt"`
	PeriodID                string         `json:"periodId"`
	IsPriorPeriodAdjustment bool           `json:"isPriorPeriodAdjustment"`
	AdjustmentForPeriodID   *string        `json:"adjustmentForPeriodId"`
	Metadata                map[string]any `json:"metadata,omitempty"`
}

func toJournalEntryDTO(e domain.JournalEntry) journalEntryDTO {
	lines := make([]lineDTO, len(e.Lines))
	for i, l := range e.Lines {
		lines[i] = lineDTO{
			GLAccount: l.GLAccount, Direction: string(l.Direction),
			Amount: toMoneyDTO(l.Amount), RunningBalanceAfter: toMoneyDTO(l.RunningBalanceAfter),
		}
	}
	return journalEntryDTO{
		JournalEntryID: e.ID, SourceEventID: e.SourceEventID, PostingRuleCode: e.PostingRuleCode,
		PostingRuleVersion: e.PostingRuleVersion, LoanAccountID: e.LoanAccountID, Lines: lines,
		Balanced: e.Balanced(), Immutable: e.Immutable(), PostedAt: e.PostedAt.Format(time.RFC3339),
		PeriodID: e.PeriodID, IsPriorPeriodAdjustment: e.IsPriorPeriodAdjustment,
		AdjustmentForPeriodID: e.AdjustmentForPeriodID, Metadata: e.Metadata,
	}
}

// postJournalEntryRequest mirrors PostJournalEntryRequest.
type postJournalEntryRequest struct {
	PostingRuleCode                  string             `json:"postingRuleCode"`
	LoanAccountID                    string             `json:"loanAccountId"`
	Amount                           *moneyDTO          `json:"amount"`
	Allocation                       *allocationDTO     `json:"allocation"`
	Capitalization                   *capitalizationDTO `json:"capitalization"`
	ReversalOfSourceEventID          *string            `json:"reversalOfSourceEventId"`
	PriorPeriodAdjustmentForPeriodID *string            `json:"priorPeriodAdjustmentForPeriodId"`
	Metadata                         map[string]any     `json:"metadata"`
}

type allocationDTO struct {
	FeeAmount       moneyDTO `json:"feeAmount"`
	InterestAmount  moneyDTO `json:"interestAmount"`
	PrincipalAmount moneyDTO `json:"principalAmount"`
}

type capitalizationDTO struct {
	InterestAmount moneyDTO `json:"interestAmount"`
	FeeAmount      moneyDTO `json:"feeAmount"`
}

// glAccountBalanceDTO mirrors GlAccountBalance.
type glAccountBalanceDTO struct {
	GLAccountCode string   `json:"glAccountCode"`
	Balance       moneyDTO `json:"balance"`
	AsOf          string   `json:"asOf"`
}

// trialBalanceDTO mirrors TrialBalance.
type trialBalanceAccountLineDTO struct {
	GLAccountCode string   `json:"glAccountCode"`
	DebitTotal    moneyDTO `json:"debitTotal"`
	CreditTotal   moneyDTO `json:"creditTotal"`
}

type trialBalanceDTO struct {
	AsOf         string                       `json:"asOf"`
	Accounts     []trialBalanceAccountLineDTO `json:"accounts"`
	TotalDebits  moneyDTO                     `json:"totalDebits"`
	TotalCredits moneyDTO                     `json:"totalCredits"`
}

func toTrialBalanceDTO(lines []store.TrialBalanceLine, asOf time.Time) trialBalanceDTO {
	accounts := make([]trialBalanceAccountLineDTO, 0, len(lines))
	var totalDebits, totalCredits int64
	currency := "USD"
	for _, l := range lines {
		if l.Currency != "" {
			currency = l.Currency
		}
		accounts = append(accounts, trialBalanceAccountLineDTO{
			GLAccountCode: l.GLAccount,
			DebitTotal:    moneyDTO{Amount: l.DebitTotal, Currency: currency},
			CreditTotal:   moneyDTO{Amount: l.CreditTotal, Currency: currency},
		})
		totalDebits += l.DebitTotal
		totalCredits += l.CreditTotal
	}
	return trialBalanceDTO{
		AsOf: asOf.Format(time.RFC3339), Accounts: accounts,
		TotalDebits: moneyDTO{Amount: totalDebits, Currency: currency}, TotalCredits: moneyDTO{Amount: totalCredits, Currency: currency},
	}
}

// statementLineDTO / statementOfAccountDTO mirror StatementLine /
// StatementOfAccount.
type statementLineDTO struct {
	JournalEntryID      string   `json:"journalEntryId"`
	PostedAt            string   `json:"postedAt"`
	PostingRuleCode     string   `json:"postingRuleCode"`
	GLAccount           string   `json:"glAccount"`
	Direction           string   `json:"direction"`
	Amount              moneyDTO `json:"amount"`
	RunningBalanceAfter moneyDTO `json:"runningBalanceAfter"`
}

type statementOfAccountDTO struct {
	LoanAccountID string             `json:"loanAccountId"`
	AsOf          string             `json:"asOf"`
	Lines         []statementLineDTO `json:"lines"`
}

func toStatementOfAccountDTO(loanAccountID string, lines []store.StatementLine, asOf time.Time) statementOfAccountDTO {
	out := make([]statementLineDTO, len(lines))
	for i, l := range lines {
		out[i] = statementLineDTO{
			JournalEntryID: l.JournalEntryID, PostedAt: l.PostedAt.Format(time.RFC3339), PostingRuleCode: l.PostingRuleCode,
			GLAccount: l.GLAccount, Direction: string(l.Direction), Amount: toMoneyDTO(l.Amount), RunningBalanceAfter: toMoneyDTO(l.RunningBalanceAfter),
		}
	}
	return statementOfAccountDTO{LoanAccountID: loanAccountID, AsOf: asOf.Format(time.RFC3339), Lines: out}
}

// periodDTO mirrors Period.
type periodDTO struct {
	PeriodID string  `json:"periodId"`
	Status   string  `json:"status"`
	ClosedAt *string `json:"closedAt"`
	ClosedBy *string `json:"closedBy"`
}

func toPeriodDTO(p domain.Period) periodDTO {
	d := periodDTO{PeriodID: p.ID, Status: string(p.Status), ClosedBy: p.ClosedBy}
	if p.ClosedAt != nil {
		s := p.ClosedAt.Format(time.RFC3339)
		d.ClosedAt = &s
	}
	return d
}

type closePeriodRequest struct {
	ClosedBy string `json:"closedBy"`
}

// errorResponse mirrors error.schema.json#/$defs/Error's minimal shape.
type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func allocationFromDTO(a *allocationDTO) *postingrules.Allocation {
	if a == nil {
		return nil
	}
	return &postingrules.Allocation{
		FeeAmount: toDomainMoney(a.FeeAmount), InterestAmount: toDomainMoney(a.InterestAmount), PrincipalAmount: toDomainMoney(a.PrincipalAmount),
	}
}

func capitalizationFromDTO(c *capitalizationDTO) *postingrules.Capitalization {
	if c == nil {
		return nil
	}
	return &postingrules.Capitalization{InterestAmount: toDomainMoney(c.InterestAmount), FeeAmount: toDomainMoney(c.FeeAmount)}
}

func amountFromDTO(m *moneyDTO) *domain.Money {
	if m == nil {
		return nil
	}
	v := toDomainMoney(*m)
	return &v
}
