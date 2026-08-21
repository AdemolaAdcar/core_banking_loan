// Package api is the HTTP transport layer: chi handlers that translate
// between the wire shapes defined in
// specs/openapi/loan-account-subledger.yaml and internal/service's
// calls. Every DTO here mirrors that spec (or
// specs/schemas/loan-account.schema.json / error.schema.json)
// field-for-field.
package api

import (
	"time"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/service"
)

type moneyDTO struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

func toMoneyDTO(m domain.Money) moneyDTO { return moneyDTO{Amount: m.Amount, Currency: m.Currency} }
func fromMoneyDTO(m moneyDTO) domain.Money {
	return domain.Money{Amount: m.Amount, Currency: m.Currency}
}

type termSetDTO struct {
	PrincipalAmount       moneyDTO `json:"principalAmount"`
	AnnualInterestRateBps int      `json:"annualInterestRateBps"`
	TermMonths            int      `json:"termMonths"`
	DayCountConvention    string   `json:"dayCountConvention"`
}

func fromTermSetDTO(t termSetDTO) domain.TermSet {
	return domain.TermSet{PrincipalAmount: fromMoneyDTO(t.PrincipalAmount), AnnualInterestRateBps: t.AnnualInterestRateBps, TermMonths: t.TermMonths, DayCountConvention: t.DayCountConvention}
}

type termVersionDTO struct {
	PrincipalAmount       moneyDTO `json:"principalAmount"`
	AnnualInterestRateBps int      `json:"annualInterestRateBps"`
	TermMonths            int      `json:"termMonths"`
	DayCountConvention    string   `json:"dayCountConvention"`
	Version               int      `json:"version"`
	EffectiveDate         string   `json:"effectiveDate"`
}

func toTermVersionDTO(v domain.TermVersion) termVersionDTO {
	return termVersionDTO{
		PrincipalAmount: toMoneyDTO(v.PrincipalAmount), AnnualInterestRateBps: v.AnnualInterestRateBps, TermMonths: v.TermMonths,
		DayCountConvention: v.DayCountConvention, Version: v.Version, EffectiveDate: v.EffectiveDate.Format("2006-01-02"),
	}
}

type loanAccountDTO struct {
	LoanAccountID       string         `json:"loanAccountId"`
	ApprovalReferenceID string         `json:"approvalReferenceId"`
	PartyID             string         `json:"partyId"`
	Status              string         `json:"status"`
	CurrentTermVersion  termVersionDTO `json:"currentTermVersion"`
	BookedBy            string         `json:"bookedBy"`
	DPD                 int            `json:"dpd"`
	DPDBucket           string         `json:"dpdBucket"`
	NonAccrualFlag      bool           `json:"nonAccrualFlag"`
	NonAccrualSince     *string        `json:"nonAccrualSince"`
	CreatedAt           string         `json:"createdAt"`
	UpdatedAt           string         `json:"updatedAt"`
}

func toLoanAccountDTO(a domain.LoanAccount) loanAccountDTO {
	d := loanAccountDTO{
		LoanAccountID: a.LoanAccountID, ApprovalReferenceID: a.ApprovalReferenceID, PartyID: a.PartyID, Status: string(a.Status),
		CurrentTermVersion: toTermVersionDTO(a.CurrentTermVersion), BookedBy: a.BookedBy, DPD: a.DPD, DPDBucket: string(a.DPDBucket),
		NonAccrualFlag: a.NonAccrualFlag, CreatedAt: a.CreatedAt.Format(time.RFC3339), UpdatedAt: a.UpdatedAt.Format(time.RFC3339),
	}
	if a.NonAccrualSince != nil {
		s := a.NonAccrualSince.Format("2006-01-02")
		d.NonAccrualSince = &s
	}
	return d
}

type bookLoanAccountRequestDTO struct {
	ApprovalReferenceID string     `json:"approvalReferenceId"`
	PartyID             string     `json:"partyId"`
	BookedBy            string     `json:"bookedBy"`
	Terms               termSetDTO `json:"terms"`
}

type disbursementRequestDTO struct {
	RequestedBy string `json:"requestedBy"`
}

type disbursementDTO struct {
	DisbursementID  string   `json:"disbursementId"`
	LoanAccountID   string   `json:"loanAccountId"`
	Status          string   `json:"status"`
	PrincipalAmount moneyDTO `json:"principalAmount"`
	RejectionReason *string  `json:"rejectionReason"`
	JournalEntryID  *string  `json:"journalEntryId"`
	InstructionID   *string  `json:"instructionId"`
	RequestedBy     string   `json:"requestedBy"`
	CreatedAt       string   `json:"createdAt"`
	UpdatedAt       string   `json:"updatedAt"`
}

func toDisbursementDTO(d domain.Disbursement) disbursementDTO {
	return disbursementDTO{
		DisbursementID: d.DisbursementID, LoanAccountID: d.LoanAccountID, Status: string(d.Status), PrincipalAmount: toMoneyDTO(d.PrincipalAmount),
		RejectionReason: d.RejectionReason, JournalEntryID: d.JournalEntryID, InstructionID: d.InstructionID, RequestedBy: d.RequestedBy,
		CreatedAt: d.CreatedAt.Format(time.RFC3339), UpdatedAt: d.UpdatedAt.Format(time.RFC3339),
	}
}

type disbursementReverseRequestDTO struct {
	ConfirmedBy string `json:"confirmedBy"`
	ReasonCode  string `json:"reasonCode"`
}

type allocationDTO struct {
	FeeAmount       moneyDTO `json:"feeAmount"`
	InterestAmount  moneyDTO `json:"interestAmount"`
	PrincipalAmount moneyDTO `json:"principalAmount"`
}

func toAllocationDTO(a domain.Allocation) allocationDTO {
	return allocationDTO{FeeAmount: toMoneyDTO(a.FeeAmount), InterestAmount: toMoneyDTO(a.InterestAmount), PrincipalAmount: toMoneyDTO(a.PrincipalAmount)}
}

type receiveRepaymentNotificationRequestDTO struct {
	LoanAccountRef *string  `json:"loanAccountRef"`
	PayoffQuoteID  *string  `json:"payoffQuoteId"`
	Amount         moneyDTO `json:"amount"`
	Rail           string   `json:"rail"`
	ReceivedAt     string   `json:"receivedAt"`
}

type repaymentDTO struct {
	RepaymentID          string         `json:"repaymentId"`
	LoanAccountID        *string        `json:"loanAccountId"`
	Status               string         `json:"status"`
	Amount               moneyDTO       `json:"amount"`
	Allocation           *allocationDTO `json:"allocation,omitempty"`
	SuspenseAmount       moneyDTO       `json:"suspenseAmount"`
	JournalEntryID       *string        `json:"journalEntryId"`
	UnmatchedReasonCode  *string        `json:"unmatchedReasonCode"`
	CorrectedRepaymentID *string        `json:"correctedRepaymentId"`
	ReceivedAt           string         `json:"receivedAt"`
	UpdatedAt            string         `json:"updatedAt"`
}

func toRepaymentDTO(r domain.Repayment) repaymentDTO {
	d := repaymentDTO{
		RepaymentID: r.RepaymentID, LoanAccountID: r.LoanAccountID, Status: string(r.Status), Amount: toMoneyDTO(r.Amount),
		SuspenseAmount: toMoneyDTO(r.SuspenseAmount), JournalEntryID: r.JournalEntryID, CorrectedRepaymentID: r.CorrectedRepaymentID,
		ReceivedAt: r.ReceivedAt.Format(time.RFC3339), UpdatedAt: r.UpdatedAt.Format(time.RFC3339),
	}
	if r.Allocation != nil {
		a := toAllocationDTO(*r.Allocation)
		d.Allocation = &a
	}
	if r.UnmatchedReasonCode != nil {
		s := string(*r.UnmatchedReasonCode)
		d.UnmatchedReasonCode = &s
	}
	return d
}

type reverseRepaymentRequestDTO struct {
	ConfirmedBy            string  `json:"confirmedBy"`
	ReasonCode             string  `json:"reasonCode"`
	CorrectedLoanAccountID *string `json:"correctedLoanAccountId"`
}

type payoffQuoteDTO struct {
	QuoteID                 string   `json:"quoteId"`
	LoanAccountID           string   `json:"loanAccountId"`
	GoodThrough             string   `json:"goodThrough"`
	PrincipalAmount         moneyDTO `json:"principalAmount"`
	ProjectedInterestAmount moneyDTO `json:"projectedInterestAmount"`
	FeesAmount              moneyDTO `json:"feesAmount"`
	TotalAmountDue          moneyDTO `json:"totalAmountDue"`
	GeneratedAt             string   `json:"generatedAt"`
}

func toPayoffQuoteDTO(q domain.PayoffQuote) payoffQuoteDTO {
	return payoffQuoteDTO{
		QuoteID: q.QuoteID, LoanAccountID: q.LoanAccountID, GoodThrough: q.GoodThrough.Format("2006-01-02"),
		PrincipalAmount: toMoneyDTO(q.PrincipalAmount), ProjectedInterestAmount: toMoneyDTO(q.ProjectedInterestAmount),
		FeesAmount: toMoneyDTO(q.FeesAmount), TotalAmountDue: toMoneyDTO(q.TotalAmountDue), GeneratedAt: q.GeneratedAt.Format(time.RFC3339),
	}
}

type payoffDTO struct {
	PayoffID       string   `json:"payoffId"`
	LoanAccountID  string   `json:"loanAccountId"`
	QuoteID        string   `json:"quoteId"`
	Status         string   `json:"status"`
	AmountPosted   moneyDTO `json:"amountPosted"`
	SuspenseAmount moneyDTO `json:"suspenseAmount"`
	JournalEntryID *string  `json:"journalEntryId"`
	ClosedAt       *string  `json:"closedAt"`
	UpdatedAt      string   `json:"updatedAt"`
}

func toPayoffDTO(p domain.Payoff) payoffDTO {
	d := payoffDTO{
		PayoffID: p.PayoffID, LoanAccountID: p.LoanAccountID, QuoteID: p.QuoteID, Status: string(p.Status),
		AmountPosted: toMoneyDTO(p.AmountPosted), SuspenseAmount: toMoneyDTO(p.SuspenseAmount), JournalEntryID: p.JournalEntryID, UpdatedAt: p.UpdatedAt.Format(time.RFC3339),
	}
	if p.ClosedAt != nil {
		s := p.ClosedAt.Format(time.RFC3339)
		d.ClosedAt = &s
	}
	return d
}

type recordChargeOffRequestDTO struct {
	ChargeoffDecisionReference string `json:"chargeoffDecisionReference"`
	ConfirmedBy                string `json:"confirmedBy"`
}

type chargeOffDTO struct {
	ChargeoffID                string   `json:"chargeoffId"`
	LoanAccountID              string   `json:"loanAccountId"`
	Status                     string   `json:"status"`
	WriteOffAmount             moneyDTO `json:"writeOffAmount"`
	JournalEntryID             *string  `json:"journalEntryId"`
	ChargeoffDecisionReference string   `json:"chargeoffDecisionReference"`
	ConfirmedBy                string   `json:"confirmedBy"`
	ChargedOffAt               *string  `json:"chargedOffAt"`
	UpdatedAt                  string   `json:"updatedAt"`
}

func toChargeOffDTO(c domain.ChargeOff) chargeOffDTO {
	d := chargeOffDTO{
		ChargeoffID: c.ChargeoffID, LoanAccountID: c.LoanAccountID, Status: string(c.Status), WriteOffAmount: toMoneyDTO(c.WriteOffAmount),
		JournalEntryID: c.JournalEntryID, ChargeoffDecisionReference: c.ChargeoffDecisionReference, ConfirmedBy: c.ConfirmedBy, UpdatedAt: c.UpdatedAt.Format(time.RFC3339),
	}
	if c.ChargedOffAt != nil {
		s := c.ChargedOffAt.Format(time.RFC3339)
		d.ChargedOffAt = &s
	}
	return d
}

type recoveryDTO struct {
	RecoveryID     string   `json:"recoveryId"`
	LoanAccountID  string   `json:"loanAccountId"`
	Amount         moneyDTO `json:"amount"`
	JournalEntryID *string  `json:"journalEntryId"`
	ReceivedAt     string   `json:"receivedAt"`
	UpdatedAt      string   `json:"updatedAt"`
}

func toRecoveryDTO(r domain.Recovery) recoveryDTO {
	return recoveryDTO{
		RecoveryID: r.RecoveryID, LoanAccountID: r.LoanAccountID, Amount: toMoneyDTO(r.Amount), JournalEntryID: r.JournalEntryID,
		ReceivedAt: r.ReceivedAt.Format(time.RFC3339), UpdatedAt: r.UpdatedAt.Format(time.RFC3339),
	}
}

type capitalizationDTO struct {
	InterestAmount moneyDTO `json:"interestAmount"`
	FeeAmount      moneyDTO `json:"feeAmount"`
}

type applyModificationRequestDTO struct {
	EffectiveDate            string             `json:"effectiveDate"`
	ConfirmedBy              string             `json:"confirmedBy"`
	NewAnnualInterestRateBps *int               `json:"newAnnualInterestRateBps"`
	NewTermMonths            *int               `json:"newTermMonths"`
	Capitalization           *capitalizationDTO `json:"capitalization"`
}

type modificationDTO struct {
	ModificationID      string    `json:"modificationId"`
	LoanAccountID       string    `json:"loanAccountId"`
	EffectiveDate       string    `json:"effectiveDate"`
	PreviousTermVersion int       `json:"previousTermVersion"`
	NewTermVersion      int       `json:"newTermVersion"`
	CapitalizedAmount   *moneyDTO `json:"capitalizedAmount"`
	JournalEntryID      *string   `json:"journalEntryId"`
	ConfirmedBy         string    `json:"confirmedBy"`
	UpdatedAt           string    `json:"updatedAt"`
}

func toModificationDTO(m domain.Modification) modificationDTO {
	d := modificationDTO{
		ModificationID: m.ModificationID, LoanAccountID: m.LoanAccountID, EffectiveDate: m.EffectiveDate.Format("2006-01-02"),
		PreviousTermVersion: m.PreviousTermVersion, NewTermVersion: m.NewTermVersion, JournalEntryID: m.JournalEntryID,
		ConfirmedBy: m.ConfirmedBy, UpdatedAt: m.UpdatedAt.Format(time.RFC3339),
	}
	if m.CapitalizedAmount != nil {
		mm := toMoneyDTO(*m.CapitalizedAmount)
		d.CapitalizedAmount = &mm
	}
	return d
}

type feeDTO struct {
	FeeID            string   `json:"feeId"`
	LoanAccountID    string   `json:"loanAccountId"`
	ScheduledDueDate string   `json:"scheduledDueDate"`
	Amount           moneyDTO `json:"amount"`
	Status           string   `json:"status"`
	JournalEntryID   *string  `json:"journalEntryId"`
	WaivedBy         *string  `json:"waivedBy"`
	WaiveReasonCode  *string  `json:"waiveReasonCode"`
	WaivedAt         *string  `json:"waivedAt"`
	AssessedAt       string   `json:"assessedAt"`
	UpdatedAt        string   `json:"updatedAt"`
}

func toFeeDTO(f domain.Fee) feeDTO {
	d := feeDTO{
		FeeID: f.FeeID, LoanAccountID: f.LoanAccountID, ScheduledDueDate: f.ScheduledDueDate.Format("2006-01-02"), Amount: toMoneyDTO(f.Amount),
		Status: string(f.Status), JournalEntryID: f.JournalEntryID, WaivedBy: f.WaivedBy, AssessedAt: f.AssessedAt.Format(time.RFC3339), UpdatedAt: f.UpdatedAt.Format(time.RFC3339),
	}
	if f.WaiveReasonCode != nil {
		s := string(*f.WaiveReasonCode)
		d.WaiveReasonCode = &s
	}
	if f.WaivedAt != nil {
		s := f.WaivedAt.Format(time.RFC3339)
		d.WaivedAt = &s
	}
	return d
}

type waiveFeeRequestDTO struct {
	ConfirmedBy string `json:"confirmedBy"`
	ReasonCode  string `json:"reasonCode"`
	Notes       string `json:"notes"`
}

type accrualBatchRequestDTO struct {
	AsOf           string `json:"asOf"`
	PartitionIndex *int   `json:"partitionIndex"`
	PartitionCount *int   `json:"partitionCount"`
}

type accrualBatchSummaryDTO struct {
	AsOf                 string   `json:"asOf"`
	PartitionIndex       *int     `json:"partitionIndex"`
	PartitionCount       *int     `json:"partitionCount"`
	EligibleAccountCount int      `json:"eligibleAccountCount"`
	PostedCount          int      `json:"postedCount"`
	ExcludedCount        int      `json:"excludedCount"`
	FailedAccountIDs     []string `json:"failedAccountIds"`
	StartedAt            string   `json:"startedAt"`
	CompletedAt          string   `json:"completedAt"`
	Status               string   `json:"status"`
}

func toAccrualBatchSummaryDTO(s service.AccrualBatchSummary) accrualBatchSummaryDTO {
	status := "Completed"
	if len(s.FailedAccountIDs) > 0 {
		status = "PartiallyFailed"
	}
	ids := s.FailedAccountIDs
	if ids == nil {
		ids = []string{}
	}
	return accrualBatchSummaryDTO{
		AsOf: s.AsOf.Format("2006-01-02"), EligibleAccountCount: s.EligibleAccountCount, PostedCount: s.PostedCount,
		ExcludedCount: s.ExcludedCount, FailedAccountIDs: ids, StartedAt: s.StartedAt.Format(time.RFC3339), CompletedAt: s.CompletedAt.Format(time.RFC3339), Status: status,
	}
}

type delinquencyBatchSummaryDTO struct {
	AsOf                   string   `json:"asOf"`
	EvaluatedAccountCount  int      `json:"evaluatedAccountCount"`
	NewlyPastDueCount      int      `json:"newlyPastDueCount"`
	FeesAssessedCount      int      `json:"feesAssessedCount"`
	NonAccrualFlaggedCount int      `json:"nonAccrualFlaggedCount"`
	FailedAccountIDs       []string `json:"failedAccountIds"`
	StartedAt              string   `json:"startedAt"`
	CompletedAt            string   `json:"completedAt"`
	Status                 string   `json:"status"`
}

func toDelinquencyBatchSummaryDTO(s service.DelinquencyBatchSummary) delinquencyBatchSummaryDTO {
	status := "Completed"
	if len(s.FailedAccountIDs) > 0 {
		status = "PartiallyFailed"
	}
	ids := s.FailedAccountIDs
	if ids == nil {
		ids = []string{}
	}
	return delinquencyBatchSummaryDTO{
		AsOf: s.AsOf.Format("2006-01-02"), EvaluatedAccountCount: s.EvaluatedAccountCount, NewlyPastDueCount: s.NewlyPastDueCount,
		FeesAssessedCount: s.FeesAssessedCount, NonAccrualFlaggedCount: s.NonAccrualFlaggedCount, FailedAccountIDs: ids,
		StartedAt: s.StartedAt.Format(time.RFC3339), CompletedAt: s.CompletedAt.Format(time.RFC3339), Status: status,
	}
}

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
